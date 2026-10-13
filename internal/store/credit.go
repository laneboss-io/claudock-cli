package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"
)

var creditPattern = regexp.MustCompile(`^[0-9]+(?:\.[0-9]{1,2})?$`)

func ValidateCredit(text string) (float64, error) {
	if len(text) > 32 || !creditPattern.MatchString(text) {
		return 0, errors.New("credit must be from 0 to 1000000 US dollars with at most two decimals")
	}
	amount, err := strconv.ParseFloat(text, 64)
	if err != nil || amount > 1000000 {
		return 0, errors.New("credit must be from 0 to 1000000 US dollars with at most two decimals")
	}
	return amount, nil
}

func SetCredit(p Profile, text string) (time.Time, error) {
	amount, err := ValidateCredit(text)
	if err != nil {
		return time.Time{}, err
	}
	if p.Kind != APIKey && p.Kind != ConsoleLogin {
		return time.Time{}, errors.New("credit applies only to Console API-key and Console-login profiles")
	}
	base, err := Directory()
	if err != nil {
		return time.Time{}, err
	}
	at := time.Now().UTC().Truncate(time.Millisecond)
	err = withSnapshot(false, func(s *snapshot) error {
		if _, _, err := currentProfile(s, p); err != nil {
			return err
		}
		return withLock(filepath.Join(base, ".api-credit-lock"), true, func() error {
			path := filepath.Join(base, "api-credit.json")
			data, err := readOptional(path, 32*maximumCredentialBytes, false)
			if err != nil {
				return errors.New("credit ledger is unavailable or unsafe")
			}
			var root map[string]json.RawMessage
			valid := data == nil || decodeJSON(data, &root) == nil
			var version int
			var profiles map[string]json.RawMessage
			if data != nil {
				valid = valid && root != nil && json.Unmarshal(root["version"], &version) == nil && version == 1 && json.Unmarshal(root["profiles"], &profiles) == nil && profiles != nil && validCreditAccounts(profiles)
			}
			if !valid {
				backup := filepath.Join(base, "api-credit.unreadable-"+at.Format("20060102T150405")+".json")
				for index := 1; ; index++ {
					if _, err := os.Lstat(backup); errors.Is(err, os.ErrNotExist) {
						break
					}
					if index > 10000 {
						return errors.New("could not find a credit ledger backup path")
					}
					backup = filepath.Join(base, fmt.Sprintf("api-credit.unreadable-%s-%d.json", at.Format("20060102T150405"), index))
				}
				if err := os.Rename(path, backup); err != nil {
					return errors.New("could not preserve unreadable credit ledger")
				}
				data = nil
				root = nil
				profiles = nil
			}
			if root == nil {
				root = map[string]json.RawMessage{"version": json.RawMessage("1")}
			}
			if profiles == nil {
				profiles = map[string]json.RawMessage{}
			}
			account := struct {
				BalanceUSD  float64 `json:"balanceUSD"`
				AsOf        string  `json:"asOf"`
				Requests    []any   `json:"requests"`
				Adjustments []any   `json:"adjustments"`
			}{BalanceUSD: amount, AsOf: at.Format("2006-01-02T15:04:05.000Z"), Requests: []any{}, Adjustments: []any{}}
			encoded, err := json.Marshal(account)
			if err != nil {
				return errors.New("could not encode Console credit")
			}
			profiles[p.ID()] = encoded
			root["profiles"], err = json.Marshal(profiles)
			if err != nil {
				return errors.New("could not encode credit ledger")
			}
			encoded, err = json.Marshal(root)
			if err != nil || len(encoded) > 32*maximumCredentialBytes {
				return errors.New("credit ledger exceeds its size limit")
			}
			if !unchanged(path, data, 32*maximumCredentialBytes) {
				return errors.New("credit ledger changed while saving")
			}
			return atomicWrite(path, append(encoded, '\n'))
		})
	})
	return at, err
}

func validCreditAccounts(profiles map[string]json.RawMessage) bool {
	for _, raw := range profiles {
		var account map[string]json.RawMessage
		if decodeJSON(raw, &account) != nil || account == nil {
			return false
		}
		balance, ok := creditNumber(account["balanceUSD"])
		if !ok || balance < 0 || balance > 1000000 || !creditDate(account["asOf"]) {
			return false
		}
		var requests, adjustments []map[string]json.RawMessage
		if decodeJSON(account["requests"], &requests) != nil || requests == nil || decodeJSON(account["adjustments"], &adjustments) != nil || adjustments == nil {
			return false
		}
		for _, request := range requests {
			cost, ok := creditNumber(request["costUSD"])
			if !ok || cost < 0 || !creditText(request["id"]) || !creditText(request["sessionID"]) || !creditDate(request["at"]) {
				return false
			}
			if model, present := request["model"]; present {
				var text *string
				if json.Unmarshal(model, &text) != nil {
					return false
				}
			}
		}
		for _, adjustment := range adjustments {
			amount, ok := creditNumber(adjustment["amountUSD"])
			if !ok || amount < 0 || !creditText(adjustment["kind"]) || !creditText(adjustment["sessionID"]) || !creditDate(adjustment["sessionStart"]) || !creditDate(adjustment["recordedAt"]) {
				return false
			}
			if _, ok := creditNumber(adjustment["lastCostUSD"]); !ok {
				return false
			}
			if _, ok := creditNumber(adjustment["capturedUSD"]); !ok {
				return false
			}
		}
	}
	return true
}

func creditNumber(raw json.RawMessage) (float64, bool) {
	var value *float64
	if json.Unmarshal(raw, &value) != nil || value == nil || math.IsNaN(*value) || math.IsInf(*value, 0) {
		return 0, false
	}
	return *value, true
}

func creditText(raw json.RawMessage) bool {
	var text *string
	return json.Unmarshal(raw, &text) == nil && text != nil
}

func creditDate(raw json.RawMessage) bool {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, text)
	return err == nil
}
