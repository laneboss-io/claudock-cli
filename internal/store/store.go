// Package store is the profile registry shared by every claudock command: the profiles Claudock manages,
// their kinds and directories, and their credentials (macOS Keychain, or a file store elsewhere and in tests).
// It reads and writes the same registry the Swift Claudock app uses, so the app and this CLI share profiles.
package store

import "errors"

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
	Name            string
	Command         string
	ConfigDirectory string
	Kind            Kind
	Managed         bool
	EndpointURL     string
	EndpointModel   string
}

// ErrNotPorted marks a function that has no implementation yet.
var ErrNotPorted = errors.New("not yet ported to Go")

// List returns every profile in registry order.
func List() ([]Profile, error) { return nil, ErrNotPorted }

// Get returns one profile by name.
func Get(name string) (Profile, error) { return Profile{}, ErrNotPorted }

// Secret returns a profile's named credential (an OAuth token, an API key or an endpoint key) for use in a
// request or a launch. Callers never print it.
func Secret(profile Profile, item string) (string, error) { return "", ErrNotPorted }
