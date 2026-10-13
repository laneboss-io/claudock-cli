package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/text/unicode/norm"
)

const (
	OAuthItem          = "oauth"
	APIKeyItem         = "apiKey"
	EndpointKeyItem    = "endpointKey"
	InferenceTokenItem = "inferenceToken"
	// InferenceTokenRecordItem retains provenance, expiry and identity for launch policy.
	InferenceTokenRecordItem = "inferenceTokenRecord"
	ConsoleKeyItem           = "consoleKey"
)

const maximumCredentialBytes = 1024 * 1024

func credentialSuffix(p Profile) string {
	if p.Command == "claude" {
		return ""
	}
	hash := sha256.Sum256([]byte(norm.NFC.String(p.ConfigDirectory)))
	return "-" + hex.EncodeToString(hash[:4])
}

// CredentialService is Claude Code's exact per-folder OAuth Keychain service.
func CredentialService(p Profile) string { return "Claude Code-credentials" + credentialSuffix(p) }

func Service(p Profile, item string) (string, error) {
	if p.IsVertex || p.DiscoveryNote != "" || !validPath(p.ConfigDirectory) || p.Kind == Codex {
		return "", errors.New("profile has no supported credential identity")
	}
	base := CredentialService(p)
	hash := sha256.Sum256([]byte(base))
	suffix := hex.EncodeToString(hash[:])
	switch item {
	case OAuthItem, "accessToken", "refreshToken", "credentials", "login":
		return base, nil
	case ConsoleKeyItem:
		return "Claude Code" + credentialSuffix(p), nil
	case APIKeyItem, "api-key", "key":
		return "Claudock-apikey-" + suffix, nil
	case EndpointKeyItem, "endpoint-key":
		return "Claudock-endpointkey-" + suffix, nil
	case InferenceTokenItem, InferenceTokenRecordItem, "inference-token", "token":
		return "Claudock-inference-" + suffix, nil
	default:
		return "", errors.New("unknown credential item")
	}
}

type credentialBackend interface {
	read(string) ([]byte, error)
	write(string, []byte) error
	exists(string) (bool, error)
	delete(string) (bool, error)
}

func backend() (credentialBackend, error) {
	choice := os.Getenv("CLAUDOCK_CREDENTIAL_STORE")
	if choice != "" {
		if !strings.HasPrefix(choice, "file:") {
			return nil, errors.New("unsupported credential store; use file:<absolute path>")
		}
		path := strings.TrimPrefix(choice, "file:")
		if !validPath(path) {
			return nil, errors.New("credential store file must have an absolute path")
		}
		return fileCredentials{path: path}, nil
	}
	if runtime.GOOS == "darwin" {
		account, err := user.Current()
		if err != nil || !regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(account.Username) {
			return nil, errors.New("could not identify Keychain account")
		}
		return keychainCredentials{account: account.Username}, nil
	}
	base, err := Directory()
	if err != nil {
		return nil, err
	}
	return fileCredentials{path: filepath.Join(base, "credentials.json")}, nil
}

type fileCredentials struct{ path string }
type credentialFile struct {
	Version int               `json:"version"`
	Items   map[string]string `json:"items"`
}

func (b fileCredentials) locked(exclusive bool, operation func(*credentialFile) error) error {
	if err := credentialDirectory(filepath.Dir(b.path)); err != nil {
		return errors.New("credential store directory is unsafe or unavailable")
	}
	return withLock(filepath.Join(filepath.Dir(b.path), ".credentials-lock"), exclusive, func() error {
		data, err := readOptional(b.path, 16*maximumCredentialBytes, true)
		if err != nil {
			return errors.New("credential store file is unsafe or unreadable")
		}
		state := credentialFile{Version: 1, Items: map[string]string{}}
		if data != nil && (decodeJSON(data, &state) != nil || state.Version != 1 || state.Items == nil || len(state.Items) > 10000) {
			return errors.New("credential store file is invalid")
		}
		if err := operation(&state); err != nil {
			return err
		}
		if !exclusive {
			return nil
		}
		encoded, err := json.MarshalIndent(state, "", "  ")
		if err != nil || len(encoded) > 16*maximumCredentialBytes {
			return errors.New("credential store exceeds its size limit")
		}
		if !unchanged(b.path, data, 16*maximumCredentialBytes) {
			return errors.New("credential store changed while saving")
		}
		return atomicWrite(b.path, append(encoded, '\n'))
	})
}

func (b fileCredentials) read(service string) ([]byte, error) {
	var result []byte
	err := b.locked(false, func(state *credentialFile) error {
		value, ok := state.Items[service]
		if !ok {
			return ErrNoSecret
		}
		if len(value) > maximumCredentialBytes {
			return errors.New("credential item exceeds its size limit")
		}
		result = []byte(value)
		return nil
	})
	return result, err
}

func (b fileCredentials) write(service string, value []byte) error {
	if len(value) > maximumCredentialBytes {
		return errors.New("credential item exceeds its size limit")
	}
	return b.locked(true, func(state *credentialFile) error { state.Items[service] = string(value); return nil })
}

func (b fileCredentials) exists(service string) (bool, error) {
	var found bool
	err := b.locked(false, func(state *credentialFile) error { _, found = state.Items[service]; return nil })
	return found, err
}

func (b fileCredentials) delete(service string) (bool, error) {
	var found bool
	err := b.locked(true, func(state *credentialFile) error {
		_, found = state.Items[service]
		if !found {
			return ErrNoSecret
		}
		delete(state.Items, service)
		return nil
	})
	if errors.Is(err, ErrNoSecret) {
		return false, nil
	}
	return found, err
}

type keychainCredentials struct{ account string }

type boundedOutput struct {
	bytes.Buffer
	limit int
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	if len(data) > b.limit-b.Len() {
		return 0, errors.New("credential output exceeds its size limit")
	}
	return b.Buffer.Write(data)
}

func (b keychainCredentials) run(arguments []string, input []byte, read bool) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/bin/security", arguments...)
	cmd.Stderr = io.Discard
	output := &boundedOutput{limit: 2*maximumCredentialBytes + 2}
	if read {
		cmd.Stdout = output
	} else {
		cmd.Stdout = io.Discard
	}
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	err := cmd.Run()
	if err == nil {
		return output.Bytes(), 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && ctx.Err() == nil {
		return nil, exit.ExitCode(), nil
	}
	return nil, -1, errors.New("Keychain operation is unavailable")
}

func (b keychainCredentials) read(service string) ([]byte, error) {
	data, status, err := b.run([]string{"find-generic-password", "-a", b.account, "-s", service, "-w"}, nil, true)
	if err != nil {
		return nil, err
	}
	if status == 44 {
		return nil, ErrNoSecret
	}
	if status != 0 {
		return nil, errors.New("Keychain item is unavailable; unlock Keychain and try again")
	}
	return bytes.TrimSuffix(data, []byte{'\n'}), nil
}

func (b keychainCredentials) write(service string, value []byte) error {
	allowed := regexp.MustCompile(`^(Claude Code(?:-credentials)?(?:-[a-f0-9]{8})?|Claudock-(?:apikey|endpointkey|inference)-[a-f0-9]{64})$`)
	if !allowed.MatchString(service) || len(value) > 2016 {
		return errors.New("credential cannot be stored using the bounded Keychain stdin route")
	}
	command := []byte(fmt.Sprintf("add-generic-password -U -a \"%s\" -s \"%s\" -X \"%s\"\n", b.account, service, hex.EncodeToString(value)))
	if len(command) > 4032 {
		return errors.New("credential exceeds the Keychain command size limit")
	}
	_, status, err := b.run([]string{"-i"}, command, false)
	if err != nil || status != 0 {
		return errors.New("credential could not be saved in Keychain")
	}
	saved, err := b.read(service)
	if err == nil && !bytes.Equal(saved, value) {
		if decoded, decodeErr := hex.DecodeString(strings.TrimSpace(string(saved))); decodeErr == nil {
			saved = decoded
		}
	}
	if err != nil || !bytes.Equal(saved, value) {
		return errors.New("credential write could not be verified in Keychain")
	}
	return nil
}

func (b keychainCredentials) exists(service string) (bool, error) {
	_, status, err := b.run([]string{"find-generic-password", "-a", b.account, "-s", service}, nil, false)
	if err != nil {
		return false, err
	}
	if status == 44 {
		return false, nil
	}
	if status != 0 {
		return false, errors.New("Keychain item could not be checked")
	}
	return true, nil
}

func (b keychainCredentials) delete(service string) (bool, error) {
	_, status, err := b.run([]string{"delete-generic-password", "-a", b.account, "-s", service}, nil, false)
	if err != nil {
		return false, err
	}
	if status == 44 {
		return false, nil
	}
	if status != 0 {
		return false, errors.New("credential item could not be deleted")
	}
	found, err := b.exists(service)
	if err != nil || found {
		return false, errors.New("credential deletion could not be verified")
	}
	return true, nil
}

func itemError(p Profile, item string, message string) error {
	// An unknown item argument may itself be a misplaced credential; do not echo it.
	switch item {
	case OAuthItem, "accessToken", "refreshToken", "credentials", "login", ConsoleKeyItem, APIKeyItem, "api-key", "key", EndpointKeyItem, "endpoint-key", InferenceTokenItem, InferenceTokenRecordItem, "inference-token", "token":
	default:
		item = "unknown"
	}
	return fmt.Errorf("profile %s, item %s: %s", p.Name, item, message)
}

func rawItem(p Profile, item string) ([]byte, error) {
	service, err := Service(p, item)
	if err != nil {
		return nil, itemError(p, item, "unsupported profile or item")
	}
	b, err := backend()
	if err != nil {
		return nil, itemError(p, item, err.Error())
	}
	data, err := b.read(service)
	if err != nil && !errors.Is(err, ErrNoSecret) {
		return nil, itemError(p, item, err.Error())
	}
	if errors.Is(err, ErrNoSecret) && (item == OAuthItem || item == "accessToken" || item == "refreshToken" || item == "credentials" || item == "login") {
		if err := credentialParent(p.ConfigDirectory); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, ErrNoSecret
			}
			return nil, itemError(p, item, "Claude credential directory is unsafe or unavailable")
		}
		data, fileErr := readFile(filepath.Join(p.ConfigDirectory, ".credentials.json"), maximumCredentialBytes, true)
		if errors.Is(fileErr, os.ErrNotExist) {
			return nil, ErrNoSecret
		}
		if fileErr != nil {
			return nil, itemError(p, item, "Claude credential file is unsafe or unavailable")
		}
		return data, nil
	}
	return data, err
}

func decodeCredentialJSON(data []byte, target any) error {
	if len(data) <= maximumCredentialBytes && decodeJSON(data, target) == nil {
		return nil
	}
	decoded, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(decoded) > maximumCredentialBytes || decodeJSON(decoded, target) != nil {
		return errors.New("credential item is invalid")
	}
	return nil
}

func readSecret(p Profile, item string) (string, error) {
	if (item == APIKeyItem || item == "api-key" || item == "key") && (p.Kind != APIKey || !p.Managed) {
		return "", itemError(p, APIKeyItem, "profile does not use a pasted API key")
	}
	if (item == EndpointKeyItem || item == "endpoint-key") && (p.Kind != Endpoint || !p.Managed) {
		return "", itemError(p, EndpointKeyItem, "profile does not use an endpoint key")
	}
	if item == InferenceTokenItem || item == InferenceTokenRecordItem || item == "inference-token" || item == "token" {
		if err := tokenProfile(p); err != nil {
			return "", itemError(p, InferenceTokenItem, "profile does not support inference tokens")
		}
	}
	data, err := rawItem(p, item)
	if err != nil {
		return "", err
	}
	switch item {
	case OAuthItem, "accessToken", "refreshToken", "credentials", "login":
		var root map[string]json.RawMessage
		if decodeCredentialJSON(data, &root) != nil || root == nil {
			return "", itemError(p, item, "OAuth credential item is invalid")
		}
		var oauth map[string]json.RawMessage
		if decodeJSON(root["claudeAiOauth"], &oauth) != nil || oauth == nil {
			return "", itemError(p, item, "OAuth credential item is invalid")
		}
		var access string
		if json.Unmarshal(oauth["accessToken"], &access) != nil || access == "" {
			return "", itemError(p, item, "OAuth access token is missing or invalid")
		}
		if item == "credentials" {
			encoded, err := json.Marshal(root)
			if err != nil {
				return "", itemError(p, item, "OAuth credential item is invalid")
			}
			return string(encoded), nil
		}
		if item == "refreshToken" {
			var value string
			if json.Unmarshal(oauth["refreshToken"], &value) != nil || value == "" {
				return "", ErrNoSecret
			}
			return value, nil
		}
		return access, nil
	case InferenceTokenItem, InferenceTokenRecordItem, "inference-token", "token":
		var record tokenRecord
		if decodeCredentialJSON(data, &record) != nil || !validTokenRecord(record) {
			return "", itemError(p, item, "inference token item is invalid")
		}
		if item == InferenceTokenRecordItem {
			// Launch must check the binding before deciding whether an expired
			// browser token may fall back to the profile's normal login.
			encoded, err := json.Marshal(record)
			if err != nil {
				return "", itemError(p, item, "inference token item is invalid")
			}
			return string(encoded), nil
		}
		if err := verifyTokenIdentity(p, record); err != nil {
			return "", err
		}
		if record.ExpiresAt != nil && *record.ExpiresAt <= float64(time.Now().UnixNano())/1e9 {
			return "", itemError(p, item, "inference token has expired")
		}
		return record.AccessToken, nil
	case APIKeyItem, "api-key", "key":
		value, err := ParseKey(string(data), APIKey)
		if err != nil || value != string(data) {
			return "", itemError(p, item, "saved API key is invalid")
		}
		return value, nil
	case EndpointKeyItem, "endpoint-key":
		value, err := ParseKey(string(data), Endpoint)
		if err != nil || value != string(data) {
			return "", itemError(p, item, "saved endpoint key is invalid")
		}
		return value, nil
	case ConsoleKeyItem:
		return string(data), nil
	default:
		return "", itemError(p, item, "unknown credential item")
	}
}

func assignmentValue(raw string, pattern *regexp.Regexp) (string, error) {
	if len(raw) > 4096 {
		return "", errors.New("secret input is larger than 4 KiB")
	}
	value := strings.TrimSpace(raw)
	if prefix := pattern.FindStringIndex(value); prefix != nil {
		value = value[prefix[1]:]
		if len(value) > 0 && (value[0] == '\'' || value[0] == '"') {
			if len(value) < 2 || value[len(value)-1] != value[0] {
				return "", errors.New("secret assignment is incomplete")
			}
			value = value[1 : len(value)-1]
		}
	}
	return value, nil
}

var consoleAssignment = regexp.MustCompile(`^export[ \t]+ANTHROPIC_API_KEY=`)
var endpointAssignment = regexp.MustCompile(`^(?:export[ \t]+)?[A-Za-z_][A-Za-z0-9_]*=`)
var tokenAssignment = regexp.MustCompile(`^export[ \t]+CLAUDE_CODE_OAUTH_TOKEN=`)
var consoleKeyShape = regexp.MustCompile(`^sk-ant-[a-z]+[0-9]{0,4}-[A-Za-z0-9_-]+$`)
var refusedConsoleKey = regexp.MustCompile(`^sk-ant-(?:oat|ort|admin|sid|si|cc|ccsr)[0-9]{0,4}-`)
var inferenceShape = regexp.MustCompile(`^sk-ant-oat01-[A-Za-z0-9._~+/=-]+$`)

func ParseKey(raw string, kind Kind) (string, error) {
	if kind == APIKey || kind == ConsoleLogin {
		value, err := assignmentValue(raw, consoleAssignment)
		if err != nil {
			return "", err
		}
		if refusedConsoleKey.MatchString(value) {
			return "", errors.New("OAuth, session and admin credentials cannot be Console API keys")
		}
		if len(value) > 512 || !consoleKeyShape.MatchString(value) {
			return "", errors.New("invalid Console API key input; provide exactly one key through standard input")
		}
		return value, nil
	}
	if kind != Endpoint {
		return "", errors.New("this profile kind cannot store a key")
	}
	value := strings.TrimSpace(raw)
	if len(raw) > 4096 {
		return "", errors.New("secret input is larger than 4 KiB")
	}
	prefix := endpointAssignment.FindStringIndex(value)
	if prefix != nil && (strings.HasPrefix(value, "export ") || strings.HasPrefix(value, "export\t") || !strings.HasPrefix(value[prefix[1]:], "=")) {
		var err error
		value, err = assignmentValue(value, endpointAssignment)
		if err != nil {
			return "", err
		}
	}
	if strings.HasPrefix(strings.ToLower(value), "sk-ant-") {
		return "", errors.New("Anthropic credentials cannot be sent to a third-party endpoint")
	}
	if len(value) == 0 || len(value) > 512 || strings.ContainsAny(value[:1]+value[len(value)-1:], "'\"") {
		return "", errors.New("enter one endpoint key or one complete key assignment")
	}
	for _, r := range value {
		if r < 33 || r > 126 {
			return "", errors.New("endpoint keys must contain printable ASCII without whitespace")
		}
	}
	return value, nil
}

func saveKeyWithUndo(p Profile, key string) (func() error, error) {
	item := APIKeyItem
	if p.Kind == Endpoint {
		item = EndpointKeyItem
	}
	return saveItemWithUndo(p, item, []byte(key))
}

func saveItemWithUndo(p Profile, item string, data []byte) (func() error, error) {
	service, err := Service(p, item)
	if err != nil {
		return nil, err
	}
	b, err := backend()
	if err != nil {
		return nil, err
	}
	previous, err := b.read(service)
	missing := errors.Is(err, ErrNoSecret)
	if err != nil && !missing {
		return nil, itemError(p, item, err.Error())
	}
	undo := func() error {
		if missing {
			_, err := b.delete(service)
			return err
		}
		return b.write(service, previous)
	}
	if err := b.write(service, data); err != nil {
		if rollbackErr := undo(); rollbackErr != nil {
			return nil, itemError(p, item, "save failed and rollback failed")
		}
		return nil, itemError(p, item, err.Error())
	}
	return undo, nil
}

func SetKey(p Profile, raw string) (Profile, error) {
	if !p.Managed || (p.Kind != APIKey && p.Kind != ConsoleLogin && p.Kind != Endpoint) {
		return Profile{}, errors.New("keys belong only to managed Console or endpoint profiles")
	}
	key, err := ParseKey(raw, p.Kind)
	if err != nil {
		return Profile{}, err
	}
	if p.Kind == ConsoleLogin {
		return SetConsoleKind(p, APIKey, key)
	}
	err = withSnapshot(true, func(s *snapshot) error {
		if _, _, err := currentProfile(s, p); err != nil {
			return err
		}
		_, err := saveKeyWithUndo(p, key)
		return err
	})
	return p, err
}

type tokenIdentity struct {
	AccountUUID      string `json:"accountUUID"`
	OrganizationUUID string `json:"organizationUUID"`
}

type tokenRecord struct {
	Version     int            `json:"version"`
	AccessToken string         `json:"accessToken"`
	ExpiresAt   *float64       `json:"expiresAt,omitempty"`
	Identity    *tokenIdentity `json:"identity,omitempty"`
	Provenance  string         `json:"provenance,omitempty"`
}

func validTokenRecord(record tokenRecord) bool {
	if record.Version != 1 && record.Version != 2 {
		return false
	}
	if record.AccessToken == "" || len(record.AccessToken) > 16384 {
		return false
	}
	for _, character := range record.AccessToken {
		if character <= 32 || character >= 127 {
			return false
		}
	}
	if record.ExpiresAt != nil && *record.ExpiresAt <= 0 {
		return false
	}
	if record.Identity != nil && (!uuidPattern.MatchString(record.Identity.AccountUUID) || !uuidPattern.MatchString(record.Identity.OrganizationUUID)) {
		return false
	}
	if record.Version == 1 {
		return (record.Provenance == "" || record.Provenance == "browser") && record.ExpiresAt != nil && record.Identity != nil
	}
	if record.Provenance == "pasted" {
		return inferenceShape.MatchString(record.AccessToken)
	}
	return record.Provenance == "browser" && record.ExpiresAt != nil && record.Identity != nil
}

func tokenProfile(p Profile) error {
	if p.Kind != Subscription || p.IsVertex || p.DiscoveryNote != "" || !validPath(p.ConfigDirectory) {
		return errors.New("inference tokens belong only to supported subscription profiles")
	}
	return nil
}

func cachedIdentity(p Profile) *tokenIdentity {
	path := filepath.Join(p.ConfigDirectory, ".claude.json")
	if p.Command == "claude" {
		home, err := homeDirectory()
		if err != nil {
			return nil
		}
		path = filepath.Join(home, ".claude.json")
	}
	data, err := readOptional(path, maximumCredentialBytes, false)
	if err != nil || data == nil {
		return nil
	}
	var root struct {
		Account struct {
			AccountUUID      string `json:"accountUuid"`
			OrganizationUUID string `json:"organizationUuid"`
		} `json:"oauthAccount"`
	}
	if decodeJSON(data, &root) != nil || !uuidPattern.MatchString(root.Account.AccountUUID) || !uuidPattern.MatchString(root.Account.OrganizationUUID) {
		return nil
	}
	return &tokenIdentity{AccountUUID: strings.ToLower(root.Account.AccountUUID), OrganizationUUID: strings.ToLower(root.Account.OrganizationUUID)}
}

func SetToken(p Profile, raw string, expiry *time.Time) error {
	if err := tokenProfile(p); err != nil {
		return err
	}
	value, err := assignmentValue(raw, tokenAssignment)
	if err != nil {
		return err
	}
	if !inferenceShape.MatchString(value) {
		return errors.New("invalid inference token input; provide exactly one token through standard input")
	}
	record := tokenRecord{Version: 2, AccessToken: value, Provenance: "pasted", Identity: cachedIdentity(p)}
	if expiry != nil {
		seconds := float64(expiry.Unix()) + float64(expiry.Nanosecond())/1e9
		if seconds <= 0 {
			return errors.New("token expiration must be after the Unix epoch")
		}
		record.ExpiresAt = &seconds
	}
	data, err := json.Marshal(record)
	if err != nil {
		return errors.New("could not encode inference token")
	}
	return withSnapshot(true, func(s *snapshot) error {
		if _, _, err := currentProfile(s, p); err != nil {
			return err
		}
		_, err := saveItemWithUndo(p, InferenceTokenItem, data)
		return err
	})
}

func ClearToken(p Profile) (bool, error) {
	if err := tokenProfile(p); err != nil {
		return false, err
	}
	var removed bool
	err := withSnapshot(true, func(s *snapshot) error {
		if _, _, err := currentProfile(s, p); err != nil {
			return err
		}
		service, err := Service(p, InferenceTokenItem)
		if err != nil {
			return err
		}
		b, err := backend()
		if err != nil {
			return err
		}
		removed, err = b.delete(service)
		if err != nil {
			return itemError(p, InferenceTokenItem, err.Error())
		}
		return nil
	})
	return removed, err
}

func TokenStatus(p Profile) (string, string, error) {
	if tokenProfile(p) != nil {
		return "n/a", "-", nil
	}
	data, err := rawItem(p, InferenceTokenItem)
	if errors.Is(err, ErrNoSecret) {
		return "none", "-", nil
	}
	if err != nil {
		return "unavailable", "-", err
	}
	var record tokenRecord
	if decodeCredentialJSON(data, &record) != nil || !validTokenRecord(record) {
		return "unavailable", "-", itemError(p, InferenceTokenItem, "saved token is invalid")
	}
	if err := verifyTokenIdentity(p, record); err != nil {
		return "unavailable", "-", err
	}
	status, expiry := "pasted-unverified", "unknown"
	if record.Provenance == "browser" || record.Version == 1 {
		status = "active"
	}
	if record.ExpiresAt != nil {
		seconds, fraction := math.Modf(*record.ExpiresAt)
		at := time.Unix(int64(seconds), int64(fraction*1e9)).UTC()
		expiry = at.Format(time.RFC3339)
		if !at.After(time.Now()) {
			status = "expired"
		}
	}
	return status, expiry, nil
}

type Leftover struct {
	Item      string
	Service   string
	Present   bool
	Unchecked bool
}

// CredentialItems uses attribute-only Keychain queries. Removal never deletes these items.
func CredentialItems(p Profile) []Leftover {
	if p.Kind == Codex || p.IsVertex || p.DiscoveryNote != "" {
		return nil
	}
	b, backendErr := backend()
	var result []Leftover
	for _, item := range []string{OAuthItem, ConsoleKeyItem, InferenceTokenItem, APIKeyItem, EndpointKeyItem} {
		service, err := Service(p, item)
		if err != nil {
			continue
		}
		entry := Leftover{Item: item, Service: service}
		if backendErr != nil {
			entry.Unchecked = true
		} else {
			entry.Present, err = b.exists(service)
			entry.Unchecked = err != nil
		}
		if entry.Present || entry.Unchecked {
			result = append(result, entry)
		}
	}
	return result
}

func ConsoleSignedIn(p Profile) (bool, error) {
	if !p.Managed || (p.Kind != APIKey && p.Kind != ConsoleLogin) {
		return false, errors.New("Console sign-in belongs only to managed Console profiles")
	}
	service, err := Service(p, ConsoleKeyItem)
	if err != nil {
		return false, err
	}
	b, err := backend()
	if err != nil {
		return false, err
	}
	found, err := b.exists(service)
	if err != nil {
		return false, itemError(p, ConsoleKeyItem, err.Error())
	}
	if !found {
		if _, fileBackend := b.(fileCredentials); fileBackend {
			if err := credentialParent(p.ConfigDirectory); err != nil {
				if errors.Is(err, os.ErrNotExist) {
					return false, nil
				}
				return false, itemError(p, ConsoleKeyItem, "credential parent is unsafe")
			}
			for _, name := range []string{".credentials.json", ".claude.json"} {
				data, err := readOptional(filepath.Join(p.ConfigDirectory, name), maximumCredentialBytes, true)
				if err != nil {
					return false, itemError(p, ConsoleKeyItem, "Claude credential file is unsafe or unavailable")
				}
				if data == nil {
					continue
				}
				var root map[string]json.RawMessage
				if decodeJSON(data, &root) != nil || root == nil {
					return false, itemError(p, ConsoleKeyItem, "Claude credential file is invalid")
				}
				for _, key := range []string{"apiKey", "primaryApiKey"} {
					if raw, present := root[key]; present {
						var value string
						if json.Unmarshal(raw, &value) != nil {
							return false, itemError(p, ConsoleKeyItem, "Claude Console key is invalid")
						}
						if value != "" {
							if _, err := ParseKey(value, APIKey); err != nil {
								return false, itemError(p, ConsoleKeyItem, "Claude Console key is invalid")
							}
							return true, nil
						}
					}
				}
			}
		}
	}
	return found, nil
}

// CredentialFiles identifies retained files by metadata only; callers must not print their contents.
func CredentialFiles(p Profile) []string {
	if p.DiscoveryNote != "" || !validPath(p.ConfigDirectory) {
		return nil
	}
	names := []string{".credentials.json", ".claude.json"}
	if p.Kind == Codex {
		names = []string{"auth.json"}
	}
	var paths []string
	for _, name := range names {
		path := filepath.Join(p.ConfigDirectory, name)
		if p.Command == "claude" && name == ".claude.json" {
			home, err := homeDirectory()
			if err != nil {
				continue
			}
			path = filepath.Join(home, name)
		}
		if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
			paths = append(paths, path)
		}
	}
	return paths
}

func credentialDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return errors.New("could not create credential directory")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("credential directory must be a private real directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("credential directory must be owned by this user")
	}
	return nil
}

func credentialParent(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return os.ErrNotExist
	}
	if err != nil || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return errors.New("credential parent is unsafe")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return errors.New("credential parent is not owned by this user")
	}
	return nil
}

func verifyTokenIdentity(p Profile, record tokenRecord) error {
	identity := cachedIdentity(p)
	matches := func() bool {
		return identity != nil && record.Identity != nil && strings.EqualFold(identity.AccountUUID, record.Identity.AccountUUID) && strings.EqualFold(identity.OrganizationUUID, record.Identity.OrganizationUUID)
	}
	if record.Provenance == "browser" || record.Version == 1 {
		if !matches() {
			return itemError(p, InferenceTokenItem, "token account could not be verified")
		}
	} else if identity != nil && record.Identity != nil && !matches() {
		return itemError(p, InferenceTokenItem, "token account no longer matches")
	}
	return nil
}
