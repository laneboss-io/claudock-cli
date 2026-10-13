package usage

import (
	"encoding/json"
	"errors"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/laneboss-io/claudock-cli/internal/state"
	"github.com/laneboss-io/claudock-cli/internal/store"
)

// Only metadata absent from the initial shared Profile contract is read here.
// Listing and all credential access remain owned by internal/store. This adapter
// can disappear once the contract exposes registry ID and discovery status.
type profileMetadata struct {
	Command         string  `json:"command"`
	ConfigDirectory string  `json:"configDirectory"`
	RegistryID      string  `json:"registryID"`
	IsVertex        bool    `json:"isVertex"`
	DiscoveryNote   *string `json:"discoveryNote"`
}

func metadata(home string) (map[string]profileMetadata, error) {
	result := make(map[string]profileMetadata)
	data, err := state.Read(filepath.Join(dataDirectory(home), "profiles.json"), 2_097_152)
	if errors.Is(err, os.ErrNotExist) {
		return result, nil
	}
	if err != nil {
		return nil, errors.New("Could not read the profile registry metadata.")
	}
	var registry struct {
		Profiles []profileMetadata `json:"profiles"`
	}
	if json.Unmarshal(data, &registry) != nil {
		return nil, errors.New("Could not read the profile registry metadata.")
	}
	for _, entry := range registry.Profiles {
		key := entry.Command + "\x00" + entry.ConfigDirectory
		if _, exists := result[key]; exists {
			return nil, errors.New("Profile registry metadata is ambiguous.")
		}
		result[key] = entry
	}
	return result, nil
}

func metadataFor(profile store.Profile, entries map[string]profileMetadata) profileMetadata {
	return entries[profile.Command+"\x00"+profile.ConfigDirectory]
}

func profileID(profile store.Profile, meta profileMetadata) string {
	if meta.RegistryID != "" {
		return meta.RegistryID
	}
	return profile.Command
}

type creditRequest struct {
	ID        *string     `json:"id"`
	SessionID *string     `json:"sessionID"`
	Model     *string     `json:"model"`
	At        time.Time   `json:"at"`
	CostUSD   json.Number `json:"costUSD"`
}

type creditAdjustment struct {
	Kind         *string     `json:"kind"`
	SessionID    *string     `json:"sessionID"`
	SessionStart time.Time   `json:"sessionStart"`
	AmountUSD    json.Number `json:"amountUSD"`
	LastCostUSD  *float64    `json:"lastCostUSD"`
	CapturedUSD  *float64    `json:"capturedUSD"`
	RecordedAt   *time.Time  `json:"recordedAt"`
}

type creditAccount struct {
	BalanceUSD  json.Number         `json:"balanceUSD"`
	AsOf        time.Time           `json:"asOf"`
	Requests    *[]creditRequest    `json:"requests"`
	Adjustments *[]creditAdjustment `json:"adjustments"`
}

type creditLedger struct {
	Version  int                      `json:"version"`
	Profiles map[string]creditAccount `json:"profiles"`
}

type creditStatus struct {
	balance *big.Rat
	spent   *big.Rat
	left    *big.Rat
}

var errLedgerUnreadable = errors.New("The Console credit ledger (api-credit.json in Claudock's Application Support folder) is unreadable. Set the credit again to start a new one; the unreadable file is kept beside it.")
var errLedgerUnavailable = errors.New("Claudock could not read the Console credit ledger just now. Try again.")
var errLedgerBusy = errors.New("Another Claudock process is updating the Console credit ledger. Try again.")

func decimal(number json.Number) (*big.Rat, bool) {
	// Swift decodes a Double, then converts its shortest decimal spelling to
	// Decimal. Do the same before exact rational summation and half-up rounding.
	// Parsing as a finite Double first also bounds hostile decimal exponents.
	value, err := number.Float64()
	if err != nil || value < 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return nil, false
	}
	decimal, ok := new(big.Rat).SetString(strconv.FormatFloat(value, 'g', -1, 64))
	return decimal, ok
}

func validAccount(account creditAccount) bool {
	balance, ok := decimal(account.BalanceUSD)
	if !ok || balance.Cmp(big.NewRat(1_000_000, 1)) > 0 || account.AsOf.IsZero() || account.Requests == nil || account.Adjustments == nil {
		return false
	}
	for _, request := range *account.Requests {
		if _, ok := decimal(request.CostUSD); !ok || request.At.IsZero() || request.ID == nil || request.SessionID == nil {
			return false
		}
	}
	for _, adjustment := range *account.Adjustments {
		if _, ok := decimal(adjustment.AmountUSD); !ok || adjustment.SessionStart.IsZero() || adjustment.Kind == nil || adjustment.SessionID == nil || adjustment.LastCostUSD == nil || adjustment.CapturedUSD == nil || adjustment.RecordedAt == nil {
			return false
		}
	}
	return true
}

func readCredit(home, id string) (*creditStatus, error) {
	base := dataDirectory(home)
	file := filepath.Join(base, "api-credit.json")
	if _, err := os.Lstat(file); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, errLedgerUnavailable
	}
	lock, err := state.OpenLock(filepath.Join(base, ".api-credit-lock"))
	if err != nil {
		return nil, errLedgerUnavailable
	}
	defer state.Unlock(lock)
	if state.Lock(lock, false, 5*time.Second) != nil {
		return nil, errLedgerBusy
	}
	data, err := state.Read(file, 32*1_048_576)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		if errors.Is(err, state.ErrUnsafe) || errors.Is(err, syscall.ELOOP) {
			return nil, errLedgerUnreadable
		}
		return nil, errLedgerUnavailable
	}
	var ledger creditLedger
	if json.Unmarshal(data, &ledger) != nil || ledger.Version != 1 || ledger.Profiles == nil {
		return nil, errLedgerUnreadable
	}
	for _, account := range ledger.Profiles {
		if !validAccount(account) {
			return nil, errLedgerUnreadable
		}
	}
	account, ok := ledger.Profiles[id]
	if !ok {
		return nil, nil
	}
	balance, _ := decimal(account.BalanceUSD)
	spent := new(big.Rat)
	for _, request := range *account.Requests {
		if !request.At.Before(account.AsOf) {
			amount, _ := decimal(request.CostUSD)
			spent.Add(spent, amount)
		}
	}
	for _, adjustment := range *account.Adjustments {
		if !adjustment.SessionStart.Before(account.AsOf) {
			amount, _ := decimal(adjustment.AmountUSD)
			spent.Add(spent, amount)
		}
	}
	left := new(big.Rat).Sub(balance, spent)
	if left.Sign() < 0 {
		left.SetInt64(0)
	}
	return &creditStatus{balance: balance, spent: spent, left: left}, nil
}

func fixed(value *big.Rat, grouping bool) string {
	scaled := new(big.Rat).Mul(value, big.NewRat(100, 1))
	scaled.Add(scaled, big.NewRat(1, 2))
	cents := new(big.Int).Quo(scaled.Num(), scaled.Denom()).String()
	if len(cents) < 3 {
		cents = strings.Repeat("0", 3-len(cents)) + cents
	}
	whole, fraction := cents[:len(cents)-2], cents[len(cents)-2:]
	if grouping {
		for at := len(whole) - 3; at > 0; at -= 3 {
			whole = whole[:at] + "," + whole[at:]
		}
	}
	return whole + "." + fraction
}

func (credit *creditStatus) usedPercent() string {
	percent := big.NewRat(100, 1)
	if credit.balance.Sign() > 0 {
		percent = new(big.Rat).Mul(new(big.Rat).Quo(credit.spent, credit.balance), percent)
	}
	return fixed(percent, false)
}

func (credit *creditStatus) leftText() string { return "$" + fixed(credit.left, true) }

func (credit *creditStatus) usageWindow() string {
	return "Credit · " + credit.leftText() + " of $" + fixed(credit.balance, true) + " left"
}

func (credit *creditStatus) rank() float64 {
	if credit.balance.Sign() <= 0 {
		return -2
	}
	share, _ := new(big.Rat).Quo(credit.left, credit.balance).Float64()
	return -2 + share
}
