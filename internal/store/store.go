// Package store is the profile registry shared by every claudock command: the profiles Claudock manages,
// their kinds and directories, and their credentials (macOS Keychain, or a file store elsewhere and in tests).
// It reads and writes the same registry the Swift Claudock app uses, so the app and this CLI share profiles.
package store

import (
	"encoding/json"
	"errors"
	"reflect"
	"regexp"
	"strings"
)

// Kind is how a profile authenticates.
type Kind string

const (
	Subscription Kind = "subscription"
	APIKey       Kind = "apiKey"
	ConsoleLogin Kind = "consoleLogin"
	Endpoint     Kind = "endpoint"
	// Codex is new in the Go CLI: a profile that runs the Codex CLI with its own CODEX_HOME.
	Codex Kind = "codex"
)

// Profile is one registered profile. Name is "default" for the plain claude command.
type Profile struct {
	Name            string                 `json:"-"`
	Command         string                 `json:"command"`
	ConfigDirectory string                 `json:"configDirectory"`
	Kind            Kind                   `json:"authKind,omitempty"`
	Managed         bool                   `json:"managed"`
	EndpointURL     string                 `json:"-"`
	EndpointModel   string                 `json:"-"`
	RegistryID      string                 `json:"registryID,omitempty"`
	IsVertex        bool                   `json:"isVertex"`
	DiscoveryNote   string                 `json:"discoveryNote,omitempty"`
	Endpoint        *EndpointConfiguration `json:"endpoint,omitempty"`
	extra           map[string]json.RawMessage
}

// ErrNotPorted is retained for callers that used the original porting contract.
var ErrNotPorted = errors.New("not yet ported to Go")
var ErrNotFound = errors.New("profile not found")
var ErrNoSecret = errors.New("credential item not found")
var ErrInvalidRegistry = errors.New("profile registry is invalid; restore it before making changes")
var ErrBusy = errors.New("profile store is busy; try again")

// EndpointConfiguration uses the Swift app's endpoint record verbatim.
type EndpointConfiguration struct {
	BaseURL   string `json:"baseURL"`
	Model     string `json:"model"`
	BehavesAs string `json:"behavesAs,omitempty"`
}

func (p Profile) ID() string {
	if p.RegistryID != "" {
		return p.RegistryID
	}
	return p.Command
}

// Equal compares profile snapshots while treating absent and empty unknown-field maps alike.
// Newly-created records and their JSON round trips must identify the same profile.
func (p Profile) Equal(other Profile) bool {
	if len(p.extra) == 0 {
		p.extra = nil
	}
	if len(other.extra) == 0 {
		other.extra = nil
	}
	return reflect.DeepEqual(p, other)
}

func profileName(command string) string {
	if command == "claude" {
		return "default"
	}
	if strings.HasPrefix(command, "codex-") {
		return strings.TrimPrefix(command, "codex-")
	}
	return strings.TrimPrefix(command, "claude-")
}

func (p Profile) MarshalJSON() ([]byte, error) {
	type plain Profile
	copy := p
	if copy.Kind == Subscription {
		copy.Kind = ""
	}
	data, err := json.Marshal(plain(copy))
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(data, &fields); err != nil {
		return nil, err
	}
	for key, value := range p.extra {
		if _, known := fields[key]; !known {
			fields[key] = value
		}
	}
	return json.Marshal(fields)
}

func (p *Profile) UnmarshalJSON(data []byte) error {
	type plain Profile
	value := plain{Kind: Subscription}
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	*p = Profile(value)
	p.Name = profileName(p.Command)
	if p.Endpoint != nil {
		p.EndpointURL, p.EndpointModel = p.Endpoint.BaseURL, p.Endpoint.Model
	}
	if err := json.Unmarshal(data, &p.extra); err != nil {
		return err
	}
	for _, key := range []string{"command", "configDirectory", "authKind", "managed", "registryID", "isVertex", "discoveryNote", "endpoint"} {
		delete(p.extra, key)
	}
	return nil
}

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,39}$`)
var selectorPattern = regexp.MustCompile(`^[\pL\pN_-]+$`)
var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// ValidateSelector never echoes input: a misplaced credential is not an error detail.
func ValidateSelector(name string) error {
	if !selectorPattern.MatchString(name) || strings.HasPrefix(strings.ToLower(name), "sk-ant-") {
		return errors.New("use a profile name or its exact selector; secrets are read from standard input")
	}
	return nil
}

func ValidateName(name string) error {
	if err := ValidateSelector(name); err != nil {
		return err
	}
	if strings.EqualFold(name, "default") || strings.EqualFold(name, "auto") {
		return errors.New("profile names default and auto are reserved")
	}
	if !namePattern.MatchString(name) {
		return errors.New("use 1–40 letters, numbers, underscores or hyphens, starting with a letter or number")
	}
	return nil
}

// List returns every profile in registry order.
func List() ([]Profile, error) { return listProfiles() }

// Get returns one profile by name.
func Get(name string) (Profile, error) {
	if err := ValidateSelector(name); err != nil {
		return Profile{}, err
	}
	profiles, err := List()
	if err != nil {
		return Profile{}, err
	}
	for _, p := range profiles {
		if p.Command == name {
			return p, nil
		}
	}
	var matches []Profile
	for _, p := range profiles {
		if p.Name == name {
			matches = append(matches, p)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return Profile{}, errors.New("ambiguous profile name; use an exact selector")
	}
	return Profile{}, ErrNotFound
}

// Secret returns a profile's named credential (an OAuth token, an API key or an endpoint key) for use in a
// request or a launch. Callers never print it.
func Secret(profile Profile, item string) (string, error) { return readSecret(profile, item) }
