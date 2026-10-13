// Package profilecmd implements profile commands independently of launch and usage commands.
package profilecmd

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/laneboss-io/claudock-cli/internal/store"
)

type command struct {
	profileLabel    string
	verb            string
	name            string
	replacement     string
	directory       string
	kind            store.Kind
	endpoint        string
	model           string
	behavesAs       string
	changeBehavesAs bool
	expiry          *time.Time
	credit          string
	console         bool
}

func parse(args []string) (command, error) {
	var c command
	if len(args) == 0 {
		return c, errors.New("use claudock profile COMMAND")
	}
	c.verb = args[0]
	switch c.verb {
	case "list", "tokens", "import-shell":
		if len(args) != 1 {
			return c, errors.New("unexpected arguments for profile command")
		}
	case "add":
		if len(args) < 2 {
			return c, errors.New("use profile add NAME [--api-key | --console | --codex | --endpoint URL --model MODEL] [--directory PATH]")
		}
		c.name = args[1]
		if err := store.ValidateName(c.name); err != nil {
			return c, err
		}
		c.kind = store.Subscription
		seen := map[string]bool{}
		selected := false
		for index := 2; index < len(args); index++ {
			option := args[index]
			if seen[option] {
				return c, errors.New("profile add options cannot be repeated")
			}
			seen[option] = true
			switch option {
			case "--api-key", "--console", "--codex", "--endpoint":
				if selected {
					return c, errors.New("choose only one of --api-key, --console, --codex or --endpoint")
				}
				selected = true
				if option == "--api-key" {
					c.kind = store.APIKey
				}
				if option == "--console" {
					c.kind = store.ConsoleLogin
				}
				if option == "--codex" {
					c.kind = store.Codex
				}
				if option == "--endpoint" {
					c.kind = store.Endpoint
					index++
					if index >= len(args) {
						return c, errors.New("--endpoint needs a URL")
					}
					c.endpoint = args[index]
				}
			case "--directory", "--model", "--behaves-as":
				index++
				if index >= len(args) {
					return c, errors.New("profile option needs a value")
				}
				if option == "--directory" {
					c.directory = args[index]
					if !absolutePath(c.directory) {
						return c, errors.New("--directory requires an absolute directory path")
					}
				}
				if option == "--model" {
					c.model = args[index]
				}
				if option == "--behaves-as" {
					c.behavesAs = args[index]
				}
			default:
				return c, errors.New("unknown profile add option; keys and tokens are read from standard input")
			}
		}
		if c.kind == store.Endpoint {
			if c.model == "" {
				return c, errors.New("--endpoint needs --model MODEL")
			}
			if c.directory != "" {
				return c, errors.New("endpoint profiles have their own config directory; --directory cannot combine with --endpoint")
			}
			if seen["--behaves-as"] && c.behavesAs == "" {
				return c, errors.New("behaves-as requires a full Claude catalog model id")
			}
			endpoint, err := store.ValidateEndpoint(store.EndpointConfiguration{BaseURL: c.endpoint, Model: c.model, BehavesAs: c.behavesAs})
			if err != nil {
				return c, err
			}
			c.endpoint = endpoint.BaseURL
		} else if seen["--model"] || seen["--behaves-as"] {
			return c, errors.New("--model and --behaves-as require --endpoint")
		}
	case "set-endpoint":
		if len(args) < 2 {
			return c, errors.New("use profile set-endpoint NAME [--endpoint URL] [--model MODEL] [--behaves-as MODEL|none]")
		}
		c.name = args[1]
		seen := map[string]bool{}
		for index := 2; index < len(args); index += 2 {
			option := args[index]
			if index+1 >= len(args) || seen[option] {
				return c, errors.New("set-endpoint options need unique values")
			}
			seen[option] = true
			switch option {
			case "--endpoint":
				c.endpoint = args[index+1]
			case "--model":
				c.model = args[index+1]
			case "--behaves-as":
				c.changeBehavesAs = true
				if args[index+1] == "" {
					return c, errors.New("behaves-as requires a full Claude catalog model id or none")
				}
				if args[index+1] != "none" {
					c.behavesAs = args[index+1]
				}
			default:
				return c, errors.New("unknown set-endpoint option")
			}
		}
		// Validate supplied values before accessing the registry, using neutral values for retained fields.
		check := store.EndpointConfiguration{BaseURL: "https://example.com", Model: "model", BehavesAs: c.behavesAs}
		if seen["--endpoint"] {
			check.BaseURL = c.endpoint
		}
		if seen["--model"] {
			check.Model = c.model
		}
		validated, err := store.ValidateEndpoint(check)
		if err != nil {
			return c, err
		}
		if seen["--endpoint"] {
			c.endpoint = validated.BaseURL
		}
	case "set-token":
		if len(args) != 2 && !(len(args) == 4 && args[2] == "--expires") {
			return c, errors.New("use profile set-token NAME [--expires ISO8601_DATE]; tokens are read from standard input")
		}
		c.name = args[1]
		if len(args) == 4 {
			parsed, err := parseExpiry(args[3])
			if err != nil {
				return c, err
			}
			c.expiry = &parsed
		}
	case "set-credit":
		if len(args) != 3 {
			return c, errors.New("use profile set-credit NAME AMOUNT")
		}
		c.name, c.credit = args[1], args[2]
		if _, err := store.ValidateCredit(c.credit); err != nil {
			return c, err
		}
	case "rename":
		if len(args) != 3 {
			return c, errors.New("use profile rename NAME NEWNAME")
		}
		c.name, c.replacement = args[1], args[2]
		if err := store.ValidateName(c.replacement); err != nil {
			return c, err
		}
	case "login":
		if len(args) != 2 && !(len(args) == 3 && args[2] == "--console") {
			return c, errors.New("use profile login NAME [--console]")
		}
		c.name = args[1]
		c.console = len(args) == 3
	case "set-key", "setup-token", "clear-token", "remove":
		if len(args) != 2 {
			return c, errors.New("profile command takes one name; keys and tokens are read from standard input")
		}
		c.name = args[1]
	default:
		return c, errors.New("unknown profile command")
	}
	if c.verb != "list" && c.verb != "tokens" && c.verb != "import-shell" {
		if err := store.ValidateSelector(c.name); err != nil {
			return c, err
		}
	}
	return c, nil
}

func absolutePath(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	for _, r := range path {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func parseExpiry(text string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02"} {
		if at, err := time.Parse(layout, text); err == nil && at.After(time.Unix(0, 0)) {
			return at, nil
		}
	}
	return time.Time{}, errors.New("--expires needs an ISO 8601 date or time after the Unix epoch")
}

func field(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, text)
}

func profileKind(p store.Profile) string {
	if p.DiscoveryNote != "" {
		return "needs-import"
	}
	if p.IsVertex {
		return "vertex"
	}
	switch p.Kind {
	case store.APIKey:
		return "api-key"
	case store.ConsoleLogin:
		return "console-login"
	case store.Endpoint:
		return "endpoint"
	case store.Codex:
		return "codex"
	}
	if p.Managed {
		return "managed"
	}
	return "imported"
}

// Execute accepts arguments after "profile". Argument failures exit 2; operational failures exit 1.
func Execute(args []string, in io.Reader, out, stderr io.Writer) int {
	c, err := parse(args)
	if err != nil {
		fmt.Fprintln(stderr, "claudock:", diagnostic(c, args, err))
		return 2
	}
	status, err := execute(&c, in, out, stderr)
	if err != nil {
		fmt.Fprintln(stderr, "claudock:", diagnostic(c, args, err))
		if status == 0 {
			return 1
		}
	}
	return status
}

func diagnostic(c command, args []string, err error) string {
	verb := "command"
	switch c.verb {
	case "list", "tokens", "import-shell", "add", "set-key", "set-endpoint", "set-credit", "set-token", "setup-token", "clear-token", "rename", "remove", "login":
		verb = c.verb
	}
	profile := c.profileLabel
	if profile == "" {
		profile = "unselected"
	}
	option, item := "arguments", "profile"
	switch c.verb {
	case "set-token", "clear-token", "setup-token", "tokens":
		item = store.InferenceTokenItem
	case "set-key":
		item = store.APIKeyItem
		if c.kind == store.Endpoint {
			item = store.EndpointKeyItem
		}
	case "set-endpoint":
		item = "endpoint"
	case "login":
		item = "login"
	case "set-credit":
		item = "credit"
	case "add":
		if c.kind == store.APIKey {
			item = store.APIKeyItem
		}
		if c.kind == store.Endpoint {
			item = store.EndpointKeyItem
		}
	}
	if c.verb == "set-key" || c.verb == "set-token" || (c.verb == "add" && (c.kind == store.APIKey || c.kind == store.Endpoint)) {
		option = "standard-input"
	}
	for index, arg := range args {
		if index == 0 {
			continue
		}
		switch arg {
		case "--api-key", "--console", "--codex", "--endpoint", "--model", "--behaves-as", "--directory", "--expires":
			option = arg
		}
	}
	// Only names obtained from the registry enter operational diagnostics. Parse failures never echo supplied values.
	return fmt.Sprintf("profile %s, command %s, option %s, item %s: %s", field(profile), verb, option, item, err.Error())
}

func execute(c *command, in io.Reader, out, stderr io.Writer) (int, error) {
	switch c.verb {
	case "list":
		profiles, err := store.List()
		if err != nil {
			return 1, err
		}
		fmt.Fprintln(out, "PROFILE\tSELECTOR\tKIND\tCONFIG_DIRECTORY\tENDPOINT")
		for _, p := range profiles {
			endpoint := "-"
			if p.Endpoint != nil {
				u, _ := url.Parse(p.Endpoint.BaseURL)
				endpoint = u.Host + " · " + p.Endpoint.Model
				if p.Endpoint.BehavesAs != "" {
					endpoint += " · behaves as " + p.Endpoint.BehavesAs
				}
			}
			fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n", field(p.Name), field(p.Command), profileKind(p), field(p.ConfigDirectory), field(endpoint))
		}
		return 0, nil
	case "import-shell":
		profiles, err := store.ImportShell()
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(out, "Imported shell configuration. %d profiles are available. Original shell files were preserved.\n", len(profiles))
		return 0, nil
	case "add":
		options := store.AddOptions{Name: c.name, Directory: c.directory, Kind: c.kind}
		if c.kind == store.Endpoint {
			options.Endpoint = &store.EndpointConfiguration{BaseURL: c.endpoint, Model: c.model, BehavesAs: c.behavesAs}
		}
		if c.kind == store.APIKey || c.kind == store.Endpoint {
			raw, err := readSecretInput(in, stderr)
			if err != nil {
				return 1, err
			}
			options.Key = raw
		}
		p, err := store.Add(options)
		if err != nil {
			return 1, err
		}
		c.profileLabel = p.Name
		switch p.Kind {
		case store.APIKey:
			fmt.Fprintf(out, "Added %s with its Console API key saved in the credential store.\n", p.Name)
		case store.Endpoint:
			fmt.Fprintf(out, "Added %s for %s, pinned to %s; its key is saved in the credential store.\n", p.Name, p.EndpointURL, p.EndpointModel)
		case store.ConsoleLogin:
			fmt.Fprintf(out, "Added %s. Signing in to its Anthropic Console account.\n", p.Name)
			return launchLogin(p, true, false, in, out, stderr)
		case store.Codex:
			fmt.Fprintf(out, "Added %s with its own CODEX_HOME directory: %s\n", p.Name, field(p.ConfigDirectory))
		default:
			fmt.Fprintf(out, "Added %s. Sign in with: claudock profile login %s\n", p.Name, p.Name)
		}
		return 0, nil
	case "tokens":
		profiles, err := store.List()
		if err != nil {
			return 1, err
		}
		fmt.Fprintln(out, "PROFILE\tTOKEN_STATUS\tEXPIRES_UTC")
		status := 0
		for _, p := range profiles {
			state, expiry, err := store.TokenStatus(p)
			fmt.Fprintf(out, "%s\t%s\t%s\n", field(p.Name), state, expiry)
			if err != nil {
				fmt.Fprintln(stderr, "claudock:", err.Error())
				status = 1
			}
		}
		return status, nil
	}
	p, err := store.Get(c.name)
	if err != nil {
		return 1, err
	}
	c.profileLabel = p.Name
	c.kind = p.Kind
	switch c.verb {
	case "set-key":
		if !p.Managed || (p.Kind != store.APIKey && p.Kind != store.ConsoleLogin && p.Kind != store.Endpoint) {
			return 1, errors.New("keys belong only to managed Console or endpoint profiles")
		}
		raw, err := readSecretInput(in, stderr)
		if err != nil {
			return 1, err
		}
		changed, err := store.SetKey(p, raw)
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(out, "Saved a new key for %s in the credential store.\n", changed.Name)
		if p.Kind == store.ConsoleLogin {
			service, _ := store.Service(p, store.ConsoleKeyItem)
			fmt.Fprintf(out, "%s now uses its pasted Console API key. The previous Console sign-in item was kept: %s\n", changed.Name, service)
		}
	case "set-endpoint":
		if p.Kind != store.Endpoint || p.Endpoint == nil {
			return 1, errors.New("only an endpoint profile has an endpoint and model to change")
		}
		endpoint := *p.Endpoint
		if c.endpoint == "" && c.model == "" && !c.changeBehavesAs {
			fmt.Fprintf(out, "%s uses %s with the model %s", p.Name, endpoint.BaseURL, endpoint.Model)
			if endpoint.BehavesAs != "" {
				fmt.Fprintf(out, ", which behaves as %s", endpoint.BehavesAs)
			}
			fmt.Fprintln(out, ".")
			return 0, nil
		}
		if c.endpoint != "" {
			endpoint.BaseURL = c.endpoint
		}
		if c.model != "" {
			endpoint.Model = c.model
		}
		if c.changeBehavesAs {
			endpoint.BehavesAs = c.behavesAs
		}
		changed, err := store.SetEndpoint(p, endpoint)
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(out, "%s now uses %s with the model %s. Its key was kept.\n", changed.Name, changed.EndpointURL, changed.EndpointModel)
	case "set-credit":
		at, err := store.SetCredit(p, c.credit)
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(out, "Set %s's Console credit to $%s as of %s.\n", p.Name, c.credit, at.Format(time.RFC3339))
	case "set-token":
		if p.Kind != store.Subscription || p.DiscoveryNote != "" || p.IsVertex {
			return 1, errors.New("inference tokens belong only to supported subscription profiles")
		}
		raw, err := readSecretInput(in, stderr)
		if err != nil {
			return 1, err
		}
		if err := store.SetToken(p, raw, c.expiry); err != nil {
			return 1, err
		}
		expiry := "unknown"
		if c.expiry != nil {
			expiry = c.expiry.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(out, "Saved an inference token for %s in the credential store. Expires: %s. Its account is not verified.\n", p.Name, expiry)
	case "clear-token":
		removed, err := store.ClearToken(p)
		if err != nil {
			return 1, err
		}
		if !removed {
			return 1, errors.New("no inference token is saved for this profile")
		}
		service, _ := store.Service(p, store.InferenceTokenItem)
		fmt.Fprintf(out, "Deleted the inference token saved for %s (service %s). Claude Code's own login was not touched.\n", p.Name, service)
	case "setup-token":
		return launchLogin(p, false, true, in, out, stderr)
	case "login":
		return launchLogin(p, c.console, false, in, out, stderr)
	case "rename":
		changed, err := store.Rename(p, c.replacement)
		if err != nil {
			return 1, err
		}
		fmt.Fprintf(out, "Renamed to %s. Profile data and credentials were preserved.\n", changed.Name)
	case "remove":
		if err := store.Remove(p); err != nil {
			return 1, err
		}
		fmt.Fprintf(out, "Removed %s from Claudock. Profile data, credentials and your own shell commands were preserved.\n", p.Name)
		for _, item := range store.CredentialItems(p) {
			note := ""
			if item.Unchecked {
				note = " (could not be checked; may still exist)"
			}
			fmt.Fprintf(out, "Credential item kept: %s, service %s%s\n", item.Item, item.Service, note)
		}
		if p.ConfigDirectory != "" {
			fmt.Fprintf(out, "Config folder: %s\n", field(p.ConfigDirectory))
			for _, path := range store.CredentialFiles(p) {
				fmt.Fprintf(out, "Credential file kept (may hold credentials or account metadata): %s\n", field(path))
			}
		}
		fmt.Fprintln(out, "Keys and tokens remain valid until revoked with their provider.")
	}
	return 0, nil
}

const Help = `Profile commands:
  claudock profile list
  claudock profile add NAME [--directory ABS_PATH]
  claudock profile add NAME --api-key [--directory ABS_PATH]
  claudock profile add NAME --console [--directory ABS_PATH]
  claudock profile add NAME --endpoint URL --model MODEL [--behaves-as CATALOG_MODEL]
  claudock profile add NAME --codex [--directory ABS_PATH]
  claudock profile set-key NAME
  claudock profile set-endpoint NAME [--endpoint URL] [--model MODEL] [--behaves-as MODEL|none]
  claudock profile set-credit NAME AMOUNT
  claudock profile set-token NAME [--expires ISO8601_DATE]
  claudock profile setup-token NAME
  claudock profile tokens
  claudock profile clear-token NAME
  claudock profile rename NAME NEWNAME
  claudock profile remove NAME
  claudock profile login NAME [--console]
  claudock profile import-shell

Keys and tokens are read from standard input. Removal keeps data and credentials.
`
