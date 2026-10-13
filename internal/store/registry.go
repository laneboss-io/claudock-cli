package store

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

type registry struct {
	Version                   int       `json:"version"`
	Profiles                  []Profile `json:"profiles"`
	SuppressedCommands        []string  `json:"suppressedCommands"`
	SuppressedDirectories     []string  `json:"suppressedDirectories"`
	SubscriptionOnlyMigration *int      `json:"subscriptionOnlyMigration,omitempty"`
	extra                     map[string]json.RawMessage
}

type snapshot struct {
	main       registry
	codex      registry
	mainBytes  []byte
	codexBytes []byte
	mainPath   string
	codexPath  string
	needsWrite bool
}

// Directory is the Swift app's registry directory on both supported platforms.
// CLAUDOCK_REGISTRY and CLAUDOCK_REGISTRY_DIR provide explicit registry isolation.
func Directory() (string, error) {
	file := os.Getenv("CLAUDOCK_REGISTRY")
	if file != "" {
		if !validPath(file) {
			return "", errors.New("registry path must be absolute")
		}
		return filepath.Dir(file), nil
	}
	if base := os.Getenv("CLAUDOCK_REGISTRY_DIR"); base != "" {
		if !validPath(base) {
			return "", errors.New("registry directory must be absolute")
		}
		return base, nil
	}
	home, err := homeDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Application Support", "Claudock"), nil
}

func homeDirectory() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || !validPath(home) {
		return "", errors.New("HOME must name an absolute directory")
	}
	return home, nil
}

func registryPaths() (string, string, error) {
	base, err := Directory()
	if err != nil {
		return "", "", err
	}
	file := os.Getenv("CLAUDOCK_REGISTRY")
	if file == "" {
		file = filepath.Join(base, "profiles.json")
	}
	return file, filepath.Join(base, "codex-profiles.json"), nil
}

func emptyRegistry() registry {
	marker := 1
	return registry{Version: 1, Profiles: []Profile{}, SuppressedCommands: []string{}, SuppressedDirectories: []string{}, SubscriptionOnlyMigration: &marker}
}

func newID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", errors.New("could not generate profile identity")
	}
	value[6] = value[6]&0x0f | 0x40
	value[8] = value[8]&0x3f | 0x80
	return strings.ToUpper(fmt.Sprintf("%x-%x-%x-%x-%x", value[:4], value[4:6], value[6:8], value[8:10], value[10:])), nil
}

// decodeJSON rejects duplicate keys and trailing JSON before interpreting a store.
// Its errors never include input bytes, which may contain credentials.
func decodeJSON(data []byte, target any) error {
	if !utf8.Valid(data) {
		return errors.New("invalid stored JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value func() error
	value = func() error {
		token, err := decoder.Token()
		if err != nil {
			return errors.New("invalid stored JSON")
		}
		delimiter, composite := token.(json.Delim)
		if !composite {
			return nil
		}
		switch delimiter {
		case '{':
			seen := map[string]bool{}
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok || seen[name] {
					return errors.New("invalid stored JSON")
				}
				seen[name] = true
				if err := value(); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := value(); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid stored JSON")
		}
		_, err = decoder.Token()
		return err
	}
	if err := value(); err != nil {
		return errors.New("invalid stored JSON")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("invalid stored JSON")
	}
	if json.Unmarshal(data, target) != nil {
		return errors.New("invalid stored JSON")
	}
	return nil
}

func decodeRegistry(data []byte, codex bool) (registry, error) {
	var state registry
	if decodeJSON(data, &state) != nil || state.Profiles == nil {
		return state, ErrInvalidRegistry
	}
	if err := json.Unmarshal(data, &state.extra); err != nil {
		return state, ErrInvalidRegistry
	}
	for _, key := range []string{"version", "profiles", "suppressedCommands", "suppressedDirectories", "subscriptionOnlyMigration"} {
		delete(state.extra, key)
	}
	if err := validateRegistry(state, codex); err != nil {
		return state, err
	}
	return state, nil
}

func encodeRegistry(state registry, codex bool) ([]byte, error) {
	if state.SuppressedCommands == nil {
		state.SuppressedCommands = []string{}
	}
	if state.SuppressedDirectories == nil {
		state.SuppressedDirectories = []string{}
	}
	if !codex {
		state.Version = 1
		for _, p := range state.Profiles {
			if p.Kind == Endpoint {
				state.Version = 3
				break
			}
			if p.Kind == APIKey || p.Kind == ConsoleLogin {
				state.Version = 2
			}
		}
	}
	if err := validateRegistry(state, codex); err != nil {
		return nil, err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return nil, ErrInvalidRegistry
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, ErrInvalidRegistry
	}
	for key, value := range state.extra {
		if _, known := fields[key]; !known {
			fields[key] = value
		}
	}
	data, err = json.MarshalIndent(fields, "", "  ")
	if err != nil || len(data)+1 > maximumRegistryBytes {
		return nil, ErrInvalidRegistry
	}
	return append(data, '\n'), nil
}

func validateRegistry(state registry, codex bool) error {
	if state.Version < 1 || state.Version > 3 || (codex && state.Version != 1) || len(state.Profiles) > 1000 || len(state.SuppressedCommands) > 10000 || len(state.SuppressedDirectories) > 10000 {
		return ErrInvalidRegistry
	}
	ids, commands := map[string]bool{}, map[string]bool{}
	defaults := 0
	for _, p := range state.Profiles {
		if !uuidPattern.MatchString(p.RegistryID) || ids[p.RegistryID] || commands[p.Command] {
			return ErrInvalidRegistry
		}
		ids[p.RegistryID], commands[p.Command] = true, true
		if p.Command == "claude" {
			defaults++
		}
		prefix := "claude-"
		if codex {
			prefix = "codex-"
		}
		if p.Command != "claude" && (!strings.HasPrefix(p.Command, prefix) || len(p.Command) == len(prefix) || ValidateSelector(p.Command) != nil) {
			return ErrInvalidRegistry
		}
		if codex && p.Command == "claude" {
			return ErrInvalidRegistry
		}
		if !validPath(p.ConfigDirectory) && !(p.ConfigDirectory == "" && p.DiscoveryNote != "") {
			return ErrInvalidRegistry
		}
		if p.Kind != Subscription && !p.Managed {
			return ErrInvalidRegistry
		}
		if (p.Kind == Endpoint) != (p.Endpoint != nil) {
			return ErrInvalidRegistry
		}
		if codex {
			if p.Kind != Codex {
				return ErrInvalidRegistry
			}
		} else if p.Kind != Subscription && p.Kind != APIKey && p.Kind != ConsoleLogin && p.Kind != Endpoint {
			return ErrInvalidRegistry
		}
		if p.Managed && (p.Command == "claude" || p.IsVertex || p.DiscoveryNote != "" || !namePattern.MatchString(profileName(p.Command)) || strings.EqualFold(profileName(p.Command), "default")) {
			return ErrInvalidRegistry
		}
		if p.Endpoint != nil {
			validated, err := ValidateEndpoint(*p.Endpoint)
			if err != nil || validated != *p.Endpoint {
				return ErrInvalidRegistry
			}
		}
	}
	if (!codex && defaults != 1) || (codex && defaults != 0) {
		return ErrInvalidRegistry
	}
	seen := map[string]bool{}
	for _, command := range state.SuppressedCommands {
		prefix := "claude-"
		if codex {
			prefix = "codex-"
		}
		if seen[command] || !strings.HasPrefix(command, prefix) || len(command) == len(prefix) || ValidateSelector(command) != nil {
			return ErrInvalidRegistry
		}
		seen[command] = true
	}
	seen = map[string]bool{}
	for _, directory := range state.SuppressedDirectories {
		if seen[directory] || !validPath(directory) {
			return ErrInvalidRegistry
		}
		seen[directory] = true
	}
	return nil
}

func readSnapshot() (*snapshot, error) {
	mainPath, codexPath, err := registryPaths()
	if err != nil {
		return nil, err
	}
	s := &snapshot{mainPath: mainPath, codexPath: codexPath, codex: emptyRegistry()}
	s.mainBytes, err = readOptional(mainPath, maximumRegistryBytes, false)
	if err != nil {
		return nil, ErrInvalidRegistry
	}
	if s.mainBytes == nil {
		s.main = emptyRegistry()
		s.main.Profiles, err = discoverProfiles()
		if err != nil {
			return nil, err
		}
		s.needsWrite = true
	} else {
		s.main, err = decodeRegistry(s.mainBytes, false)
		if err != nil {
			return nil, err
		}
	}
	s.codexBytes, err = readOptional(codexPath, maximumRegistryBytes, false)
	if err != nil {
		return nil, ErrInvalidRegistry
	}
	if s.codexBytes != nil {
		s.codex, err = decodeRegistry(s.codexBytes, true)
		if err != nil {
			return nil, err
		}
	}
	if s.main.SubscriptionOnlyMigration == nil {
		discovered, err := discoverProfiles()
		if err != nil {
			return nil, err
		}
		reconcileExternal(&s.main, discovered)
		marker := 1
		s.main.SubscriptionOnlyMigration = &marker
		s.needsWrite = true
	}
	kept := s.main.Profiles[:0]
	for _, p := range s.main.Profiles {
		if p.IsVertex {
			suppress(&s.main, p)
			s.needsWrite = true
		} else {
			kept = append(kept, p)
		}
	}
	s.main.Profiles = kept
	expectedVersion := 1
	for _, p := range s.main.Profiles {
		if p.Kind == Endpoint {
			expectedVersion = 3
			break
		}
		if p.Kind == APIKey || p.Kind == ConsoleLogin {
			expectedVersion = 2
		}
	}
	if s.main.Version != expectedVersion {
		s.needsWrite = true
	}
	if err := validateRegistry(s.main, false); err != nil {
		return nil, err
	}
	if err := validateCombined(s); err != nil {
		return nil, err
	}
	return s, nil
}

func validateCombined(s *snapshot) error {
	ids := map[string]bool{}
	for _, p := range append(append([]Profile{}, s.main.Profiles...), s.codex.Profiles...) {
		if ids[p.ID()] {
			return ErrInvalidRegistry
		}
		ids[p.ID()] = true
	}
	return nil
}

func withSnapshot(exclusive bool, operation func(*snapshot) error) error {
	base, err := Directory()
	if err != nil {
		return err
	}
	if err = privateDirectory(base); err != nil {
		return err
	}
	return withLock(filepath.Join(base, ".registry-lock"), exclusive, func() error {
		s, err := readSnapshot()
		if err != nil {
			return err
		}
		return operation(s)
	})
}

func listProfiles() ([]Profile, error) {
	var result []Profile
	missing := false
	err := withSnapshot(false, func(s *snapshot) error {
		missing = s.needsWrite
		result = append(append([]Profile{}, s.main.Profiles...), s.codex.Profiles...)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if missing {
		err = withSnapshot(true, func(s *snapshot) error {
			if err := publish(s); err != nil {
				return err
			}
			result = append(append([]Profile{}, s.main.Profiles...), s.codex.Profiles...)
			return nil
		})
	}
	return result, err
}

func publish(s *snapshot) error {
	if err := validateCombined(s); err != nil {
		return err
	}
	mainBytes, err := encodeRegistry(s.main, false)
	if err != nil {
		return err
	}
	codexBytes, err := encodeRegistry(s.codex, true)
	if err != nil {
		return err
	}
	if !unchanged(s.mainPath, s.mainBytes, maximumRegistryBytes) || !unchanged(s.codexPath, s.codexBytes, maximumRegistryBytes) {
		return errors.New("profile registry changed while saving")
	}
	// Codex-only updates never rewrite Swift's registry. Updates affect one registry at a time.
	mainChanged := s.needsWrite
	if !mainChanged && s.mainBytes != nil {
		original, err := decodeRegistry(s.mainBytes, false)
		mainChanged = err != nil || !reflect.DeepEqual(original, s.main)
	}
	if mainChanged {
		if err := atomicWrite(s.mainPath, mainBytes); err != nil {
			return err
		}
	}
	codexChanged := s.codexBytes == nil && len(s.codex.Profiles) > 0
	if s.codexBytes != nil {
		original, err := decodeRegistry(s.codexBytes, true)
		codexChanged = err != nil || !reflect.DeepEqual(original, s.codex)
	}
	if codexChanged {
		if err := atomicWrite(s.codexPath, codexBytes); err != nil {
			return err
		}
	}
	return nil
}

type AddOptions struct {
	Name      string
	Directory string
	Kind      Kind
	Endpoint  *EndpointConfiguration
	Key       string
}

func Add(options AddOptions) (Profile, error) {
	if err := ValidateName(options.Name); err != nil {
		return Profile{}, err
	}
	if options.Kind == "" {
		options.Kind = Subscription
	}
	if options.Kind != Subscription && options.Kind != APIKey && options.Kind != ConsoleLogin && options.Kind != Endpoint && options.Kind != Codex {
		return Profile{}, errors.New("unsupported profile kind")
	}
	if options.Directory != "" {
		if !validPath(options.Directory) {
			return Profile{}, errors.New("choose an existing absolute config directory")
		}
		info, err := os.Stat(options.Directory)
		if err != nil || !info.IsDir() {
			return Profile{}, errors.New("choose an existing absolute config directory")
		}
	}
	if options.Kind == Endpoint {
		if options.Directory != "" || options.Endpoint == nil {
			return Profile{}, errors.New("endpoint profiles need an endpoint and their own config directory")
		}
		endpoint, err := ValidateEndpoint(*options.Endpoint)
		if err != nil {
			return Profile{}, err
		}
		options.Endpoint = &endpoint
	} else if options.Endpoint != nil {
		return Profile{}, errors.New("an endpoint belongs only to an endpoint profile")
	}
	if options.Kind == APIKey || options.Kind == Endpoint {
		key, err := ParseKey(options.Key, options.Kind)
		if err != nil {
			return Profile{}, err
		}
		options.Key = key
	}
	var result Profile
	err := withSnapshot(true, func(s *snapshot) error {
		for _, p := range append(append([]Profile{}, s.main.Profiles...), s.codex.Profiles...) {
			if p.Name == options.Name || p.Command == "claude-"+options.Name || p.Command == "codex-"+options.Name {
				return errors.New("a profile with that name already exists")
			}
		}
		id, err := newID()
		if err != nil {
			return err
		}
		base := filepath.Dir(s.mainPath)
		prefix, folder := "claude-", "claude"
		if options.Kind == Codex {
			prefix, folder = "codex-", "codex"
		}
		path := options.Directory
		if path == "" {
			path = filepath.Join(base, "accounts", id, folder)
		}
		for _, p := range append(append([]Profile{}, s.main.Profiles...), s.codex.Profiles...) {
			candidate := Profile{Command: prefix + options.Name, Kind: options.Kind, ConfigDirectory: path}
			if sameCredentialIdentity(p, candidate) {
				return errors.New("config path already belongs to a profile")
			}
		}
		result = Profile{Name: options.Name, Command: prefix + options.Name, ConfigDirectory: path, Kind: options.Kind, Managed: true, RegistryID: id, Endpoint: options.Endpoint}
		if result.Endpoint != nil {
			result.EndpointURL, result.EndpointModel = result.Endpoint.BaseURL, result.Endpoint.Model
		}
		var workspace *preparedWorkspace
		if options.Directory == "" {
			workspace, err = prepareWorkspace(path, options.Kind == Codex)
			if err != nil {
				return err
			}
			defer workspace.rollback()
		} else if options.Kind != Codex {
			if err := validateSettings(path); err != nil {
				return err
			}
		}
		var undo func() error
		if options.Kind == APIKey || options.Kind == Endpoint {
			undo, err = saveKeyWithUndo(result, options.Key)
			if err != nil {
				return err
			}
		}
		if workspace != nil {
			if err := workspace.validate(); err != nil {
				if undo != nil {
					if rollbackErr := undo(); rollbackErr != nil {
						return errors.New("workspace changed and credential rollback failed")
					}
				}
				return err
			}
		}
		target := &s.main
		if result.Kind == Codex {
			target = &s.codex
		}
		target.Profiles = append(target.Profiles, result)
		sortProfiles(target.Profiles)
		if err := publish(s); err != nil {
			if errors.Is(err, errPublicationUncertain) {
				if workspace != nil {
					workspace.commit()
				}
				return err
			}
			if undo != nil {
				if rollbackErr := undo(); rollbackErr != nil {
					return errors.New("profile publication failed and credential rollback failed; inspect its store item")
				}
			}
			return err
		}
		if workspace != nil {
			workspace.commit()
		}
		return nil
	})
	return result, err
}

func currentProfile(s *snapshot, p Profile) (*registry, int, error) {
	target := &s.main
	if p.Kind == Codex {
		target = &s.codex
	}
	for index, current := range target.Profiles {
		if current.ID() == p.ID() && current.Equal(p) {
			return target, index, nil
		}
	}
	return nil, 0, errors.New("profile changed or was removed; refresh and try again")
}

func Rename(p Profile, name string) (Profile, error) {
	if err := ValidateName(name); err != nil {
		return Profile{}, err
	}
	if p.Command == "claude" || p.IsVertex {
		return Profile{}, errors.New("default and external profiles cannot be renamed")
	}
	if p.DiscoveryNote != "" || !validPath(p.ConfigDirectory) {
		return Profile{}, errors.New("resolve this profile's config directory before renaming")
	}
	var result Profile
	err := withSnapshot(true, func(s *snapshot) error {
		target, index, err := currentProfile(s, p)
		if err != nil {
			return err
		}
		if name == p.Name {
			result = p
			return nil
		}
		for _, other := range append(append([]Profile{}, s.main.Profiles...), s.codex.Profiles...) {
			if other.Name == name {
				return errors.New("a profile with that name already exists")
			}
		}
		suppress(target, p)
		result = p
		prefix := "claude-"
		if p.Kind == Codex {
			prefix = "codex-"
		}
		result.Command = prefix + name
		result.Name = name
		result.Managed = true
		target.Profiles[index] = result
		sortProfiles(target.Profiles)
		return publish(s)
	})
	return result, err
}

func Remove(p Profile) error {
	if p.Command == "claude" || p.IsVertex {
		return errors.New("default and external profiles cannot be removed")
	}
	return withSnapshot(true, func(s *snapshot) error {
		target, index, err := currentProfile(s, p)
		if err != nil {
			return err
		}
		suppress(target, p)
		target.Profiles = append(target.Profiles[:index], target.Profiles[index+1:]...)
		return publish(s)
	})
}

func SetEndpoint(p Profile, endpoint EndpointConfiguration) (Profile, error) {
	endpoint, err := ValidateEndpoint(endpoint)
	if err != nil {
		return Profile{}, err
	}
	if p.Kind != Endpoint || !p.Managed {
		return Profile{}, errors.New("only a managed endpoint profile has an endpoint to change")
	}
	var result Profile
	err = withSnapshot(true, func(s *snapshot) error {
		target, index, err := currentProfile(s, p)
		if err != nil {
			return err
		}
		result = p
		result.Endpoint = &endpoint
		result.EndpointURL, result.EndpointModel = endpoint.BaseURL, endpoint.Model
		target.Profiles[index] = result
		return publish(s)
	})
	return result, err
}

// SetConsoleKind preserves both credential namespaces. An API-key transition saves its key first.
func SetConsoleKind(p Profile, kind Kind, key string) (Profile, error) {
	if !p.Managed || (p.Kind != APIKey && p.Kind != ConsoleLogin) || (kind != APIKey && kind != ConsoleLogin) {
		return Profile{}, errors.New("only managed Console profiles can change sign-in kind")
	}
	if kind == APIKey {
		parsed, err := ParseKey(key, APIKey)
		if err != nil {
			return Profile{}, err
		}
		key = parsed
	}
	var result Profile
	err := withSnapshot(true, func(s *snapshot) error {
		target, index, err := currentProfile(s, p)
		if err != nil {
			return err
		}
		result = p
		result.Kind = kind
		var undo func() error
		if kind == APIKey {
			undo, err = saveKeyWithUndo(result, key)
			if err != nil {
				return err
			}
		}
		target.Profiles[index] = result
		if err := publish(s); err != nil {
			if errors.Is(err, errPublicationUncertain) {
				return err
			}
			if undo != nil {
				if rollbackErr := undo(); rollbackErr != nil {
					return errors.New("kind update failed and credential rollback failed")
				}
			}
			return err
		}
		return nil
	})
	return result, err
}

func suppress(state *registry, p Profile) {
	if !contains(state.SuppressedCommands, p.Command) {
		state.SuppressedCommands = append(state.SuppressedCommands, p.Command)
	}
	if p.ConfigDirectory != "" && !contains(state.SuppressedDirectories, p.ConfigDirectory) {
		state.SuppressedDirectories = append(state.SuppressedDirectories, p.ConfigDirectory)
	}
	sort.Strings(state.SuppressedCommands)
	sort.Strings(state.SuppressedDirectories)
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}

func sortProfiles(profiles []Profile) {
	sort.SliceStable(profiles, func(i, j int) bool {
		if profiles[i].Command == "claude" {
			return profiles[j].Command != "claude"
		}
		if profiles[j].Command == "claude" {
			return false
		}
		return naturalLess(profiles[i].Command, profiles[j].Command)
	})
}

func naturalLess(a, b string) bool {
	left, right := strings.ToLower(a), strings.ToLower(b)
	for len(left) > 0 && len(right) > 0 {
		if left[0] >= '0' && left[0] <= '9' && right[0] >= '0' && right[0] <= '9' {
			i, j := 0, 0
			for i < len(left) && left[i] >= '0' && left[i] <= '9' {
				i++
			}
			for j < len(right) && right[j] >= '0' && right[j] <= '9' {
				j++
			}
			x, y := strings.TrimLeft(left[:i], "0"), strings.TrimLeft(right[:j], "0")
			if len(x) != len(y) {
				return len(x) < len(y)
			}
			if x != y {
				return x < y
			}
			left, right = left[i:], right[j:]
		} else {
			if left[0] != right[0] {
				return left[0] < right[0]
			}
			left, right = left[1:], right[1:]
		}
	}
	if left != right {
		return len(left) < len(right)
	}
	return a < b
}

func ImportShell() ([]Profile, error) {
	discovered, err := discoverProfiles()
	if err != nil {
		return nil, err
	}
	var result []Profile
	err = withSnapshot(true, func(s *snapshot) error {
		reconcileExternal(&s.main, discovered)
		for _, candidate := range discovered {
			if candidate.IsVertex || candidate.Command == "claude" || contains(s.main.SuppressedCommands, candidate.Command) {
				continue
			}
			blocked := false
			for _, directory := range s.main.SuppressedDirectories {
				if norm.NFC.String(directory) == norm.NFC.String(candidate.ConfigDirectory) {
					blocked = true
				}
			}
			for _, current := range append(append([]Profile{}, s.main.Profiles...), s.codex.Profiles...) {
				if current.Name == candidate.Name || sameCredentialIdentity(current, candidate) {
					blocked = true
				}
			}
			if blocked {
				continue
			}
			s.main.Profiles = append(s.main.Profiles, candidate)
		}
		sortProfiles(s.main.Profiles)
		if err := publish(s); err != nil {
			return err
		}
		result = append(append([]Profile{}, s.main.Profiles...), s.codex.Profiles...)
		return nil
	})
	return result, err
}

func reconcileExternal(state *registry, discovered []Profile) {
	kept := state.Profiles[:0]
	for _, stored := range state.Profiles {
		remove := stored.IsVertex
		if !stored.Managed && stored.Command != "claude" {
			for _, candidate := range discovered {
				if candidate.IsVertex && candidate.Command == stored.Command && candidate.ConfigDirectory == stored.ConfigDirectory {
					remove = true
				}
			}
		}
		if remove {
			suppress(state, stored)
		} else {
			kept = append(kept, stored)
		}
	}
	state.Profiles = kept
}

func sameCredentialIdentity(a, b Profile) bool {
	if a.IsVertex || b.IsVertex || a.DiscoveryNote != "" || b.DiscoveryNote != "" || !validPath(a.ConfigDirectory) || !validPath(b.ConfigDirectory) {
		return false
	}
	if a.Kind == Codex || b.Kind == Codex {
		return norm.NFC.String(a.ConfigDirectory) == norm.NFC.String(b.ConfigDirectory)
	}
	return CredentialService(a) == CredentialService(b)
}
