// This fixture is a real child CLI. It requests a localhost sign-in stub and records only booleans about its environment.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/text/unicode/norm"
)

type configuration struct {
	URL            string
	Directory      string
	CredentialFile string
	Codex          bool
}
type observation struct {
	Arguments         []string `json:"arguments"`
	DirectoryMatches  bool     `json:"directoryMatches"`
	OtherHomeAbsent   bool     `json:"otherHomeAbsent"`
	CredentialsAbsent bool     `json:"credentialsAbsent"`
}
type response struct {
	Exit       int    `json:"exit"`
	ConsoleKey string `json:"consoleKey"`
	CodexKey   string `json:"codexKey"`
}

func fail(message string) { fmt.Fprintln(os.Stderr, message); os.Exit(91) }

func main() {
	data, err := os.ReadFile(os.Getenv("CLAUDOCK_FIXTURE_CONFIG"))
	if err != nil {
		fail("fixture configuration unavailable")
	}
	var config configuration
	if json.Unmarshal(data, &config) != nil {
		fail("fixture configuration invalid")
	}
	u, err := url.Parse(config.URL)
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" {
		fail("fixture requires a localhost HTTP stub")
	}
	variable, other := "CLAUDE_CONFIG_DIR", "CODEX_HOME"
	if config.Codex {
		variable, other = "CODEX_HOME", "CLAUDE_CONFIG_DIR"
	}
	actual := os.Getenv(variable)
	if config.Directory == "" {
		actual = ""
		if os.Getenv(variable) != "" {
			actual = "unexpected"
		}
	}
	observed := observation{Arguments: os.Args[1:], DirectoryMatches: actual == config.Directory, OtherHomeAbsent: os.Getenv(other) == "", CredentialsAbsent: true}
	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_REFRESH_TOKEN", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_SECURESTORAGE_CONFIG_DIR", "OPENAI_API_KEY", "OPENAI_BASE_URL", "CODEX_API_KEY"} {
		if os.Getenv(key) != "" {
			observed.CredentialsAbsent = false
		}
	}
	body, err := json.Marshal(observed)
	if err != nil {
		fail("fixture observation could not be encoded")
	}
	client := http.Client{Timeout: 5 * time.Second}
	result, err := client.Post(config.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		fail("fixture HTTP request failed")
	}
	defer result.Body.Close()
	if result.StatusCode != http.StatusOK {
		fail("fixture HTTP request was rejected")
	}
	data, err = io.ReadAll(io.LimitReader(result.Body, 8192))
	if err != nil {
		fail("fixture HTTP response unavailable")
	}
	var answer response
	if json.Unmarshal(data, &answer) != nil {
		fail("fixture HTTP response invalid")
	}
	if answer.ConsoleKey != "" {
		data, err := os.ReadFile(config.CredentialFile)
		state := struct {
			Version int               `json:"version"`
			Items   map[string]string `json:"items"`
		}{Version: 1, Items: map[string]string{}}
		if err == nil {
			if json.Unmarshal(data, &state) != nil {
				fail("fixture credential store invalid")
			}
		} else if !os.IsNotExist(err) {
			fail("fixture credential store unavailable")
		}
		service := "Claude Code"
		if config.Directory != "" {
			hash := sha256.Sum256([]byte(norm.NFC.String(config.Directory)))
			service += "-" + hex.EncodeToString(hash[:4])
		}
		state.Items[service] = answer.ConsoleKey
		encoded, err := json.Marshal(state)
		if err != nil {
			fail("fixture credential store could not be encoded")
		}
		if err := os.MkdirAll(filepath.Dir(config.CredentialFile), 0700); err != nil {
			fail("fixture credential directory unavailable")
		}
		file, err := os.CreateTemp(filepath.Dir(config.CredentialFile), ".fixture-*.tmp")
		if err != nil {
			fail("fixture credential temporary file unavailable")
		}
		name := file.Name()
		if err := file.Chmod(0600); err != nil {
			fail("fixture credential permissions failed")
		}
		if _, err := file.Write(encoded); err != nil {
			fail("fixture credential write failed")
		}
		if err := file.Close(); err != nil {
			fail("fixture credential close failed")
		}
		if err := os.Rename(name, config.CredentialFile); err != nil {
			fail("fixture credential publication failed")
		}
	}
	if answer.CodexKey != "" {
		directory := os.Getenv("CODEX_HOME")
		if !filepath.IsAbs(directory) || directory != config.Directory {
			fail("fixture Codex home was not isolated correctly")
		}
		encoded, err := json.Marshal(map[string]string{"OPENAI_API_KEY": answer.CodexKey})
		if err != nil {
			fail("fixture Codex credentials could not be encoded")
		}
		if err := os.WriteFile(filepath.Join(directory, "auth.json"), encoded, 0600); err != nil {
			fail("fixture Codex credential write failed")
		}
	}
	fmt.Fprintln(os.Stdout, "fixture CLI completed")
	fmt.Fprintln(os.Stderr, "fixture CLI diagnostic")
	os.Exit(answer.Exit)
}
