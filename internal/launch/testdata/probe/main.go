// A real stand-in vendor program. It records argv and environment NAMES only.
// Comparisons against registry/credential fixtures are recorded as booleans.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/text/unicode/norm"
)

func main() {
	fail := func() { os.Exit(98) }
	var waiting chan os.Signal
	if os.Getenv("PROBE_WAIT_SIGNAL") == "1" {
		waiting = make(chan os.Signal, 1)
		signal.Notify(waiting, syscall.SIGTERM)
		defer signal.Stop(waiting)
	}
	type profile struct {
		Command, ConfigDirectory, AuthKind string
		Endpoint                           *struct{ BaseURL, Model string }
	}
	registryPath := os.Getenv("CLAUDOCK_REGISTRY")
	var profiles []profile
	for _, path := range []string{registryPath, filepath.Join(filepath.Dir(registryPath), "codex-profiles.json")} {
		data, err := os.ReadFile(path)
		var registry struct{ Profiles []profile }
		if err != nil || json.Unmarshal(data, &registry) != nil {
			fail()
		}
		profiles = append(profiles, registry.Profiles...)
	}
	name := os.Getenv("PROBE_PROFILE")
	var config, command, kind string
	var endpoint, model string
	for _, profile := range profiles {
		profileName := strings.TrimPrefix(strings.TrimPrefix(profile.Command, "claude-"), "codex-")
		if profile.Command == "claude" {
			profileName = "default"
		}
		if profileName == name {
			config, command, kind = profile.ConfigDirectory, profile.Command, profile.AuthKind
			if profile.Endpoint != nil {
				endpoint, model = profile.Endpoint.BaseURL, profile.Endpoint.Model
			}
		}
	}
	backend := os.Getenv("CLAUDOCK_CREDENTIAL_STORE")
	if !strings.HasPrefix(backend, "file:") || config == "" {
		fail()
	}
	data, err := os.ReadFile(strings.TrimPrefix(backend, "file:"))
	if err != nil {
		fail()
	}
	var credentials struct {
		Version int               `json:"version"`
		Items   map[string]string `json:"items"`
	}
	if json.Unmarshal(data, &credentials) != nil || credentials.Version != 1 || credentials.Items == nil {
		fail()
	}
	suffix := ""
	if command != "claude" {
		digest := sha256.Sum256([]byte(norm.NFC.String(config)))
		suffix = "-" + hex.EncodeToString(digest[:4])
	}
	digest := sha256.Sum256([]byte("Claude Code-credentials" + suffix))
	identity := hex.EncodeToString(digest[:])
	checks := make(map[string]bool)
	if kind == "codex" {
		checks["config"] = os.Getenv("CODEX_HOME") == config
		checks["credential"] = os.Getenv("OPENAI_API_KEY") == "" && os.Getenv("CODEX_API_KEY") == ""
	} else {
		expectedConfig := config
		if command == "claude" {
			expectedConfig = ""
		}
		checks["config"] = os.Getenv("CLAUDE_CONFIG_DIR") == expectedConfig
		switch kind {
		case "apiKey":
			key := credentials.Items["Claudock-apikey-"+identity]
			checks["credential"] = os.Getenv("ANTHROPIC_API_KEY") == key && key != ""
		case "endpoint":
			key := credentials.Items["Claudock-endpointkey-"+identity]
			checks["credential"] = os.Getenv("ANTHROPIC_AUTH_TOKEN") == key && key != ""
			checks["endpoint"] = os.Getenv("ANTHROPIC_BASE_URL") == endpoint
			checks["model"] = os.Getenv("ANTHROPIC_MODEL") == model && os.Getenv("CLAUDE_CODE_SUBAGENT_MODEL") == model
			checks["settings"] = false
			checks["settings-file"] = false
			if len(os.Args) > 2 && os.Args[1] == "--settings" {
				data := []byte(os.Args[2])
				if filepath.IsAbs(os.Args[2]) {
					var err error
					data, err = os.ReadFile(os.Args[2])
					file, fileErr := os.Lstat(os.Args[2])
					parent, parentErr := os.Lstat(filepath.Dir(os.Args[2]))
					checks["settings-file"] = err == nil && fileErr == nil && parentErr == nil &&
						file.Mode().IsRegular() && file.Mode().Perm() == 0600 && parent.IsDir() && parent.Mode().Perm() == 0700
				}
				var settings struct {
					Environment map[string]string `json:"env"`
					Available   []string          `json:"availableModels"`
				}
				if json.Unmarshal(data, &settings) == nil {
					checks["settings"] = len(settings.Available) == 1 && settings.Available[0] == strings.TrimSuffix(model, "[1m]") &&
						settings.Environment["ANTHROPIC_MODEL"] == model && settings.Environment["ANTHROPIC_AUTH_TOKEN"] == "" &&
						settings.Environment["ANTHROPIC_API_KEY"] == "" && settings.Environment["CLAUDE_CONFIG_DIR"] == config
					if source := os.Getenv("PROBE_SETTINGS_SOURCE"); source != "" {
						expectedData, err := os.ReadFile(source)
						var expected struct {
							Environment map[string]string `json:"env"`
						}
						if err != nil || json.Unmarshal(expectedData, &expected) != nil {
							fail()
						}
						token := expected.Environment["SERVICE_ACCESS_TOKEN"]
						checks["settings-canary"] = token != "" && settings.Environment["SERVICE_ACCESS_TOKEN"] == token
						checks["argv-clean"] = token != ""
						for _, arg := range os.Args {
							checks["argv-clean"] = checks["argv-clean"] && !strings.Contains(arg, token)
						}
					}
				}
			}
		case "consoleLogin":
			checks["credential"] = credentials.Items["Claude Code"+suffix] != "" && os.Getenv("ANTHROPIC_API_KEY") == "" && os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") == ""
		default:
			expected := credentials.Items["Claudock-inference-"+identity]
			if strings.HasPrefix(expected, "{") {
				var token struct {
					Access     string  `json:"accessToken"`
					Expiry     float64 `json:"expiresAt"`
					Provenance string  `json:"provenance"`
				}
				if json.Unmarshal([]byte(expected), &token) != nil {
					fail()
				}
				expected = token.Access
				if token.Expiry > 0 && token.Expiry <= float64(time.Now().Unix()) && token.Provenance == "browser" {
					expected = ""
				}
			}
			if len(os.Args) > 1 && os.Args[1] == "setup-token" {
				expected = ""
			}
			checks["credential"] = os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") == expected && os.Getenv("ANTHROPIC_API_KEY") == ""
		}
	}
	names := make([]string, 0)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		names = append(names, key)
	}
	sort.Strings(names)
	cwd, err := os.Getwd()
	if err != nil {
		fail()
	}
	record := struct {
		PID     int             `json:"pid"`
		Program string          `json:"program"`
		Args    []string        `json:"args"`
		Names   []string        `json:"names"`
		Checks  map[string]bool `json:"checks"`
		CWD     string          `json:"cwd"`
	}{os.Getpid(), filepath.Base(os.Args[0]), os.Args[1:], names, checks, cwd}
	writeRecord := func() {
		data, err := json.Marshal(record)
		if err != nil {
			fail()
		}
		path := os.Getenv("PROBE_RECORD")
		file, err := os.CreateTemp(filepath.Dir(path), ".probe-*.json")
		if err != nil {
			fail()
		}
		name := file.Name()
		defer os.Remove(name)
		_, writeErr := file.Write(data)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil || os.Rename(name, path) != nil {
			fail()
		}
	}
	writeRecord()
	os.Stdout.WriteString("probe stdout\n")
	os.Stderr.WriteString("probe stderr\n")
	if waiting != nil {
		checks["signal-forwarded"] = <-waiting == syscall.SIGTERM
		writeRecord()
		os.Exit(0)
	}
	if os.Getenv("PROBE_SIGNAL") == "TERM" {
		if syscall.Kill(os.Getpid(), syscall.SIGTERM) != nil {
			fail()
		}
		select {}
	}
	status, err := strconv.Atoi(os.Getenv("PROBE_STATUS"))
	if err != nil {
		fail()
	}
	os.Exit(status)
}
