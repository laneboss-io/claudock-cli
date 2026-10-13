package profiles_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/laneboss-io/claudock-cli/internal/testutil"
)

var binary, fixtureCLI string

func TestMain(m *testing.M) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		fmt.Fprintln(os.Stderr, "test source path unavailable")
		os.Exit(1)
	}
	root := filepath.Dir(filepath.Dir(file))
	buildRoot := root
	if override := os.Getenv("CLAUDOCK_TEST_SOURCE_DIR"); override != "" {
		buildRoot = override
	}
	buildDirectory, err := os.MkdirTemp("", "cc-go-profiles-build-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "test build directory unavailable")
		os.Exit(1)
	}
	binary = filepath.Join(buildDirectory, "claudock")
	fixtureCLI = filepath.Join(buildDirectory, "fixture-cli")
	for _, build := range []struct{ root, target, output string }{{buildRoot, "./cmd/claudock", binary}, {root, "./tests/testdata/cli", fixtureCLI}} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		command := exec.CommandContext(ctx, "go", "build", "-mod=readonly", "-o", build.output, build.target)
		command.Dir = build.root
		command.Env = append(os.Environ(), "CGO_ENABLED=0")
		output, err := command.CombinedOutput()
		cancel()
		if err != nil {
			fmt.Fprintln(os.Stderr, "black-box test binary build failed")
			fmt.Fprint(os.Stderr, string(output))
			os.Exit(1)
		}
	}
	code := m.Run()
	if err := os.RemoveAll(buildDirectory); err != nil {
		fmt.Fprintln(os.Stderr, "test build directory cleanup failed")
		code = 1
	}
	os.Exit(code)
}

type profile struct {
	Command   string            `json:"command"`
	Directory string            `json:"configDirectory"`
	Managed   bool              `json:"managed"`
	ID        string            `json:"registryID"`
	Kind      string            `json:"authKind,omitempty"`
	IsVertex  bool              `json:"isVertex"`
	Note      string            `json:"discoveryNote,omitempty"`
	Endpoint  map[string]string `json:"endpoint,omitempty"`
}
type registry struct {
	Version     int       `json:"version"`
	Profiles    []profile `json:"profiles"`
	Commands    []string  `json:"suppressedCommands"`
	Directories []string  `json:"suppressedDirectories"`
	Migration   *int      `json:"subscriptionOnlyMigration,omitempty"`
}
type result struct {
	code           int
	stdout, stderr string
}
type observation struct {
	Arguments         []string `json:"arguments"`
	DirectoryMatches  bool     `json:"directoryMatches"`
	OtherHomeAbsent   bool     `json:"otherHomeAbsent"`
	CredentialsAbsent bool     `json:"credentialsAbsent"`
}

type sandbox struct {
	t                                               *testing.T
	cliResults                                      *testutil.CLIResults
	root, home, registry, credentials, binDirectory string
	extra                                           []string
	secrets                                         []string
}

func newSandbox(t *testing.T) *sandbox {
	t.Helper()
	root := t.TempDir()
	s := &sandbox{t: t, cliResults: testutil.CaptureCLI(t), root: root, home: filepath.Join(root, "home"), registry: filepath.Join(root, "registry", "profiles.json"), credentials: filepath.Join(root, "vault", "credentials.json"), binDirectory: filepath.Join(root, "bin")}
	for _, directory := range []string{s.home, filepath.Dir(s.registry), filepath.Dir(s.credentials), s.binDirectory} {
		if err := os.MkdirAll(directory, 0700); err != nil {
			t.Fatal("could not create isolated test directory")
		}
	}
	for _, program := range []string{"claude", "codex"} {
		if err := os.Symlink(fixtureCLI, filepath.Join(s.binDirectory, program)); err != nil {
			t.Fatal("could not install child CLI fixture")
		}
	}
	return s
}

func (s *sandbox) environment() []string {
	return append([]string{"HOME=" + s.home, "PATH=" + s.binDirectory + ":/usr/bin:/bin", "CLAUDOCK_REGISTRY=" + s.registry, "CLAUDOCK_CREDENTIAL_STORE=file:" + s.credentials}, s.extra...)
}

func (s *sandbox) run(input string, args ...string) result {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, append([]string{"profile"}, args...)...)
	command.Env = s.environment()
	command.Dir = s.home
	command.Stdin = strings.NewReader(input)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			s.cliResults.Record(-1, stdout.String(), stderr.String())
			s.t.Fatal("black-box CLI did not execute or timed out")
		}
		code = exit.ExitCode()
	}
	r := result{code: code, stdout: stdout.String(), stderr: stderr.String()}
	s.cliResults.Record(r.code, r.stdout, r.stderr)
	for _, secret := range s.secrets {
		if secret != "" && (strings.Contains(r.stdout, secret) || strings.Contains(r.stderr, secret)) {
			s.t.Fatal("credential value appeared in CLI output")
		}
	}
	return r
}

func (s *sandbox) expect(input string, code int, args ...string) result {
	s.t.Helper()
	r := s.run(input, args...)
	if r.code != code {
		s.t.Fatalf("exit code %d, expected %d for profile %s; stderr: %s", r.code, code, args[0], r.stderr)
	}
	if code != 0 && r.stderr == "" {
		s.t.Fatal("failed command produced no diagnostic")
	}
	return r
}

func (s *sandbox) secret(prefix string) string {
	s.t.Helper()
	var random [18]byte
	if _, err := rand.Read(random[:]); err != nil {
		s.t.Fatal("fixture random input unavailable")
	}
	value := prefix + hex.EncodeToString(random[:])
	s.secrets = append(s.secrets, value)
	s.cliResults.Redact(value)
	return value
}

func (s *sandbox) read(path string) []byte {
	s.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		s.t.Fatal("expected test file is unavailable")
	}
	return data
}

func (s *sandbox) write(path string, data []byte) {
	s.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		s.t.Fatal("fixture parent directory unavailable")
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		s.t.Fatal("fixture write failed")
	}
}

func (s *sandbox) writeJSON(path string, value any) {
	s.t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		s.t.Fatal("fixture JSON encoding failed")
	}
	s.write(path, data)
}

func (s *sandbox) state() registry {
	s.t.Helper()
	var state registry
	if json.Unmarshal(s.read(s.registry), &state) != nil {
		s.t.Fatal("registry contains invalid JSON")
	}
	return state
}

func (s *sandbox) codexState() registry {
	s.t.Helper()
	var state registry
	if json.Unmarshal(s.read(filepath.Join(filepath.Dir(s.registry), "codex-profiles.json")), &state) != nil {
		s.t.Fatal("Codex registry contains invalid JSON")
	}
	return state
}

func (s *sandbox) get(command string) profile {
	s.t.Helper()
	state := s.state()
	if strings.HasPrefix(command, "codex-") {
		state = s.codexState()
	}
	for _, p := range state.Profiles {
		if p.Command == command {
			return p
		}
	}
	s.t.Fatal("expected profile is absent")
	return profile{}
}

func (s *sandbox) items() map[string]string {
	s.t.Helper()
	var state struct {
		Version int               `json:"version"`
		Items   map[string]string `json:"items"`
	}
	if json.Unmarshal(s.read(s.credentials), &state) != nil || state.Version != 1 {
		s.t.Fatal("credential store is invalid")
	}
	return state.Items
}

func (s *sandbox) seedItems(items map[string]string) {
	s.writeJSON(s.credentials, struct {
		Version int               `json:"version"`
		Items   map[string]string `json:"items"`
	}{Version: 1, Items: items})
}

func services(p profile) []string {
	suffix := ""
	if p.Command != "claude" {
		digest := sha256.Sum256([]byte(p.Directory))
		suffix = "-" + hex.EncodeToString(digest[:4])
	}
	login := "Claude Code-credentials" + suffix
	digest := sha256.Sum256([]byte(login))
	id := hex.EncodeToString(digest[:])
	return []string{login, "Claude Code" + suffix, "Claudock-inference-" + id, "Claudock-apikey-" + id, "Claudock-endpointkey-" + id}
}

func (s *sandbox) unchanged(path string, before []byte) {
	s.t.Helper()
	if !bytes.Equal(s.read(path), before) {
		s.t.Fatal("refused or read-only command changed a file")
	}
}
func (s *sandbox) noRegistry() {
	s.t.Helper()
	if _, err := os.Lstat(s.registry); !errors.Is(err, os.ErrNotExist) {
		s.t.Fatal("invalid arguments touched the registry")
	}
}
func (s *sandbox) noLeaks() {
	s.t.Helper()
	err := filepath.WalkDir(s.root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || path == s.credentials || entry.Name() == ".credentials.json" || entry.Name() == "auth.json" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, secret := range s.secrets {
			if bytes.Contains(data, []byte(secret)) {
				return errors.New("credential found outside its store")
			}
		}
		return nil
	})
	if err != nil {
		s.t.Fatal("credential containment check failed")
	}
}

func (s *sandbox) attachHTTP(directory string, codex bool, exit int, saveConsole bool) <-chan observation {
	s.t.Helper()
	observed := make(chan observation, 8)
	key := ""
	codexKey := ""
	if saveConsole {
		key = s.secret("sk-ant-api03-")
	}
	if codex {
		codexKey = s.secret("sk-")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "wrong method", 400)
			return
		}
		var row observation
		if json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&row) != nil {
			http.Error(w, "wrong JSON", 400)
			return
		}
		observed <- row
		if err := json.NewEncoder(w).Encode(struct {
			Exit     int    `json:"exit"`
			Key      string `json:"consoleKey"`
			CodexKey string `json:"codexKey"`
		}{Exit: exit, Key: key, CodexKey: codexKey}); err != nil {
			s.t.Error("localhost fixture response failed")
		}
	}))
	s.t.Cleanup(server.Close)
	config := filepath.Join(s.root, "fixture-config.json")
	s.writeJSON(config, struct {
		URL, Directory, CredentialFile string
		Codex                          bool
	}{URL: server.URL, Directory: directory, CredentialFile: s.credentials, Codex: codex})
	s.extra = append(s.extra, "CLAUDOCK_FIXTURE_CONFIG="+config, "ANTHROPIC_API_KEY=fixture-canary", "CLAUDE_CODE_OAUTH_TOKEN=fixture-canary", "CLAUDE_CODE_USE_VERTEX=1", "CLAUDE_SECURESTORAGE_CONFIG_DIR=fixture-canary", "OPENAI_API_KEY=fixture-canary", "OPENAI_BASE_URL=fixture-canary", "CODEX_API_KEY=fixture-canary", "CODEX_HOME=fixture-canary", "CLAUDE_CONFIG_DIR=fixture-canary")
	return observed
}

func assertRequest(t *testing.T, requests <-chan observation, args ...string) {
	t.Helper()
	select {
	case row := <-requests:
		if !reflect.DeepEqual(row.Arguments, args) || !row.DirectoryMatches || !row.OtherHomeAbsent || !row.CredentialsAbsent {
			t.Fatal("child CLI arguments or credential isolation were incorrect")
		}
	case <-time.After(time.Second):
		t.Fatal("child CLI never requested the localhost API")
	}
}

func TestProfileList(t *testing.T) {
	t.Run("bootstrap", func(t *testing.T) {
		s := newSandbox(t)
		r := s.expect("", 0, "list")
		if !strings.Contains(r.stdout, "default\tclaude\timported\t"+filepath.Join(s.home, ".claude")) || !strings.HasPrefix(r.stdout, "PROFILE\tSELECTOR\tKIND") {
			t.Fatal("default profile list row is incorrect")
		}
		state := s.state()
		if state.Version != 1 || len(state.Profiles) != 1 || state.Profiles[0].ID == "" || state.Profiles[0].Kind != "" {
			t.Fatal("bootstrap does not match Swift registry schema")
		}
		info, err := os.Stat(s.registry)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("registry permissions are not private")
		}
		before := s.read(s.registry)
		s.expect("", 0, "list")
		s.unchanged(s.registry, before)
	})
	for _, version := range []int{1, 2, 3} {
		t.Run(fmt.Sprintf("Swift-v%d", version), func(t *testing.T) {
			s := newSandbox(t)
			s.expect("", 0, "list")
			state := s.state()
			state.Version = version
			p := profile{Command: "claude-fixture", Directory: filepath.Join(s.home, "explicit"), ID: "10000000-0000-4000-8000-000000000001", Managed: true}
			kind := "managed"
			if version == 2 {
				p.Kind = "apiKey"
				kind = "api-key"
			}
			if version == 3 {
				p.Kind = "endpoint"
				p.Endpoint = map[string]string{"baseURL": "https://example.com/api", "model": "model-v1"}
				kind = "endpoint"
			}
			state.Profiles = append(state.Profiles, p)
			s.writeJSON(s.registry, state)
			before := s.read(s.registry)
			r := s.expect("", 0, "list")
			if !strings.Contains(r.stdout, "fixture\tclaude-fixture\t"+kind) {
				t.Fatal("Swift profile kind was not loaded")
			}
			s.unchanged(s.registry, before)
		})
	}
	t.Run("bad-arguments", func(t *testing.T) { s := newSandbox(t); s.expect("", 2, "list", "unexpected"); s.noRegistry() })
}

func TestAddKinds(t *testing.T) {
	for _, kind := range []string{"subscription", "apiKey", "consoleLogin", "endpoint", "codex"} {
		t.Run(kind, func(t *testing.T) {
			s := newSandbox(t)
			args := []string{"add", "work"}
			input := ""
			var request <-chan observation
			switch kind {
			case "apiKey":
				args = append(args, "--api-key")
				input = s.secret("sk-ant-api03-")
			case "consoleLogin":
				args = append(args, "--console")
			case "endpoint":
				args = append(args, "--endpoint", "HTTPS://Example.COM/api///", "--model", "model-v1[1m]", "--behaves-as", "claude-sonnet-4-6")
				input = s.secret("provider-")
			case "codex":
				args = append(args, "--codex")
			}
			if kind == "consoleLogin" {
				// Use an explicit directory so the HTTP fixture can verify the exact path.
				directory := filepath.Join(s.home, "console")
				if err := os.Mkdir(directory, 0700); err != nil {
					t.Fatal("explicit fixture directory unavailable")
				}
				args = append(args, "--directory", directory)
				request = s.attachHTTP(directory, false, 0, true)
			}
			s.expect(input, 0, args...)
			command := "claude-work"
			if kind == "codex" {
				command = "codex-work"
			}
			p := s.get(command)
			if p.ID == "" || !p.Managed || (kind == "subscription" && p.Kind != "") || (kind != "subscription" && p.Kind != kind) {
				t.Fatal("added profile has the wrong registry fields")
			}
			info, err := os.Stat(p.Directory)
			if err != nil || !info.IsDir() {
				t.Fatal("profile config directory was not prepared")
			}
			if kind == "subscription" {
				target, err := os.Readlink(filepath.Join(p.Directory, "settings.json"))
				if err != nil || target != filepath.Join(s.home, ".claude", "settings.json") {
					t.Fatal("shared settings symlink is missing")
				}
			}
			if kind == "apiKey" || kind == "endpoint" {
				index := 3
				if kind == "endpoint" {
					index = 4
				}
				if s.items()[services(p)[index]] != input {
					t.Fatal("credential was not stored under its Swift service")
				}
			}
			if kind == "endpoint" {
				if p.Endpoint["baseURL"] != "https://example.com/api" || p.Endpoint["model"] != "model-v1[1m]" {
					t.Fatal("endpoint was not normalized and pinned")
				}
				if s.state().Version != 3 {
					t.Fatal("endpoint registry must be version 3")
				}
			}
			if kind == "apiKey" && s.state().Version != 2 {
				t.Fatal("Console registry must be version 2")
			}
			if kind == "codex" {
				if s.state().Version != 1 || len(s.state().Profiles) != 1 || len(s.codexState().Profiles) != 1 {
					t.Fatal("Codex invalidated the Swift registry")
				}
			}
			if request != nil {
				assertRequest(t, request, "auth", "login", "--console")
				if s.items()[services(p)[1]] != s.secrets[len(s.secrets)-1] {
					t.Fatal("Console add did not persist the credential returned by the localhost sign-in")
				}
			}
			s.noLeaks()
		})
	}
	for _, kind := range []string{"subscription", "apiKey", "codex"} {
		t.Run("explicit-"+kind, func(t *testing.T) {
			s := newSandbox(t)
			path := s.home + "/literal/../explicit"
			if err := os.MkdirAll(s.home+"/literal", 0700); err != nil {
				t.Fatal("fixture directory unavailable")
			}
			if err := os.Mkdir(s.home+"/explicit", 0700); err != nil {
				t.Fatal("fixture directory unavailable")
			}
			s.write(filepath.Join(s.home, "explicit", "keep.txt"), []byte("untouched"))
			args := []string{"add", "work", "--directory", path}
			input := ""
			command := "claude-work"
			if kind == "apiKey" {
				args = append(args, "--api-key")
				input = s.secret("sk-ant-usr-")
			}
			if kind == "codex" {
				args = append(args, "--codex")
				command = "codex-work"
			}
			s.expect(input, 0, args...)
			if s.get(command).Directory != path {
				t.Fatal("literal config path was rewritten")
			}
			if string(s.read(s.home+"/explicit/keep.txt")) != "untouched" {
				t.Fatal("explicit directory was modified")
			}
			s.noLeaks()
		})
	}
}

func TestSetKey(t *testing.T) {
	for _, kind := range []string{"apiKey", "endpoint", "consoleLogin"} {
		t.Run(kind, func(t *testing.T) {
			s := newSandbox(t)
			key := s.secret("sk-ant-api03-")
			index := 3
			if kind == "endpoint" {
				key = s.secret("provider-")
				index = 4
				s.expect(key, 0, "add", "work", "--endpoint", "https://example.com", "--model", "x")
			} else {
				s.expect(key, 0, "add", "work", "--api-key")
			}
			p := s.get("claude-work")
			if kind == "consoleLogin" {
				state := s.state()
				for i := range state.Profiles {
					if state.Profiles[i].Command == p.Command {
						state.Profiles[i].Kind = "consoleLogin"
					}
				}
				s.writeJSON(s.registry, state)
				items := s.items()
				items[services(p)[1]] = s.secret("sk-ant-api03-")
				s.seedItems(items)
			}
			before := s.items()
			replacement := s.secret("sk-ant-usr-")
			if kind == "endpoint" {
				replacement = s.secret("provider-")
			}
			s.expect("export ANTHROPIC_API_KEY='"+replacement+"'\n", 0, "set-key", "work")
			after := s.get("claude-work")
			if after.ID != p.ID || after.Directory != p.Directory || after.Kind == "consoleLogin" {
				t.Fatal("key replacement changed profile identity or failed to switch Console kind")
			}
			items := s.items()
			if items[services(p)[index]] != replacement {
				t.Fatal("replacement key was not saved")
			}
			if kind == "consoleLogin" && items[services(p)[1]] != before[services(p)[1]] {
				t.Fatal("Console sign-in item was removed")
			}
			s.noLeaks()
		})
	}
	for _, row := range []struct {
		name, input string
		args        []string
		code        int
	}{{"unknown", "", []string{"set-key", "absent"}, 1}, {"argv-key", "", []string{"set-key", "work", "misplaced"}, 2}, {"bad-input", "invalid input", []string{"set-key", "work"}, 1}, {"OAuth", "sk-ant-oat01-invalid", []string{"set-key", "work"}, 1}, {"oversize", strings.Repeat("x", 4097), []string{"set-key", "work"}, 1}} {
		t.Run(row.name, func(t *testing.T) {
			s := newSandbox(t)
			key := s.secret("sk-ant-api03-")
			s.expect(key, 0, "add", "work", "--api-key")
			before := s.read(s.credentials)
			s.secrets = append(s.secrets, row.input)
			s.expect(row.input, row.code, row.args...)
			s.unchanged(s.credentials, before)
		})
	}
	for _, kind := range []string{"subscription", "codex"} {
		t.Run("refuse-"+kind, func(t *testing.T) {
			s := newSandbox(t)
			args := []string{"add", "work"}
			if kind == "codex" {
				args = append(args, "--codex")
			}
			s.expect("", 0, args...)
			s.expect("invalid", 1, "set-key", "work")
		})
	}
}

func TestSetEndpoint(t *testing.T) {
	t.Run("update-and-show", func(t *testing.T) {
		s := newSandbox(t)
		key := s.secret("provider-")
		s.expect(key, 0, "add", "work", "--endpoint", "https://example.com/a", "--model", "x", "--behaves-as", "claude-sonnet-4-6")
		p := s.get("claude-work")
		credentials := s.read(s.credentials)
		s.expect("", 0, "set-endpoint", "work", "--endpoint", "HTTPS://New.Example.com/v1/", "--model", "y[1m]", "--behaves-as", "none")
		changed := s.get("claude-work")
		if changed.ID != p.ID || changed.Directory != p.Directory || changed.Endpoint["baseURL"] != "https://new.example.com/v1" || changed.Endpoint["model"] != "y[1m]" || changed.Endpoint["behavesAs"] != "" {
			t.Fatal("endpoint update lost identity or did not apply options")
		}
		s.unchanged(s.credentials, credentials)
		before := s.read(s.registry)
		r := s.expect("", 0, "set-endpoint", "work")
		if !strings.Contains(r.stdout, "https://new.example.com/v1") || !strings.Contains(r.stdout, "y[1m]") {
			t.Fatal("endpoint inspection did not show stored fields")
		}
		s.unchanged(s.registry, before)
		s.noLeaks()
	})
	for _, row := range []struct {
		name string
		args []string
	}{{"HTTP", []string{"--endpoint", "http://example.com"}}, {"bad-model", []string{"--model", "two models"}}, {"bad-mapping", []string{"--behaves-as", "sonnet"}}, {"duplicate", []string{"--model", "x", "--model", "y"}}, {"missing-value", []string{"--endpoint"}}, {"unknown", []string{"--unknown", "x"}}, {"empty-model", []string{"--model", ""}}} {
		t.Run(row.name, func(t *testing.T) {
			s := newSandbox(t)
			key := s.secret("provider-")
			s.expect(key, 0, "add", "work", "--endpoint", "https://example.com", "--model", "x")
			before := s.read(s.registry)
			s.expect("", 2, append([]string{"set-endpoint", "work"}, row.args...)...)
			s.unchanged(s.registry, before)
		})
	}
	t.Run("wrong-kind", func(t *testing.T) {
		s := newSandbox(t)
		s.expect("", 0, "add", "work")
		s.expect("", 1, "set-endpoint", "work", "--model", "y")
	})
	t.Run("missing-profile", func(t *testing.T) { s := newSandbox(t); s.expect("", 1, "set-endpoint", "absent", "--model", "y") })
}

func TestSetCredit(t *testing.T) {
	for _, amount := range []string{"0", "187.42", "1000000"} {
		t.Run(amount, func(t *testing.T) {
			s := newSandbox(t)
			s.expect(s.secret("sk-ant-api03-"), 0, "add", "work", "--api-key")
			p := s.get("claude-work")
			path := filepath.Join(filepath.Dir(s.registry), "api-credit.json")
			s.writeJSON(path, map[string]any{"version": 1, "profiles": map[string]any{"other": map[string]any{"balanceUSD": 5, "asOf": "2026-01-01T00:00:00.000Z", "requests": []any{}, "adjustments": []any{}}}})
			s.expect("", 0, "set-credit", "work", amount)
			var ledger struct {
				Version  int `json:"version"`
				Profiles map[string]struct {
					Balance     float64 `json:"balanceUSD"`
					AsOf        string  `json:"asOf"`
					Requests    []any   `json:"requests"`
					Adjustments []any   `json:"adjustments"`
				} `json:"profiles"`
			}
			if json.Unmarshal(s.read(path), &ledger) != nil {
				t.Fatal("credit ledger JSON invalid")
			}
			expected := json.Number(amount)
			number, err := expected.Float64()
			if err != nil || ledger.Profiles[p.ID].Balance != number || ledger.Profiles[p.ID].AsOf == "" || ledger.Profiles[p.ID].Requests == nil || ledger.Profiles["other"].Balance != 5 {
				t.Fatal("credit ledger did not preserve or update account data")
			}
			s.noLeaks()
		})
	}
	for _, amount := range []string{"-1", "+2", "1e2", "1.234", "1000000.01", " 5", "$5", "NaN"} {
		t.Run("refuse-"+strings.ReplaceAll(amount, "/", "_"), func(t *testing.T) { s := newSandbox(t); s.expect("", 2, "set-credit", "work", amount); s.noRegistry() })
	}
	for _, kind := range []string{"subscription", "endpoint", "codex"} {
		t.Run("wrong-kind-"+kind, func(t *testing.T) {
			s := newSandbox(t)
			args := []string{"add", "work"}
			input := ""
			if kind == "endpoint" {
				args = append(args, "--endpoint", "https://example.com", "--model", "x")
				input = s.secret("provider-")
			}
			if kind == "codex" {
				args = append(args, "--codex")
			}
			s.expect(input, 0, args...)
			s.expect("", 1, "set-credit", "work", "10")
		})
	}
	t.Run("unreadable-ledger-kept", func(t *testing.T) {
		s := newSandbox(t)
		s.expect(s.secret("sk-ant-api03-"), 0, "add", "work", "--api-key")
		path := filepath.Join(filepath.Dir(s.registry), "api-credit.json")
		original := []byte("invalid ledger bytes")
		s.write(path, original)
		s.expect("", 0, "set-credit", "work", "12.34")
		matches, err := filepath.Glob(filepath.Join(filepath.Dir(s.registry), "api-credit.unreadable-*.json"))
		if err != nil || len(matches) != 1 || !bytes.Equal(s.read(matches[0]), original) {
			t.Fatal("unreadable ledger was not preserved")
		}
	})
	t.Run("missing-profile", func(t *testing.T) { s := newSandbox(t); s.expect("", 1, "set-credit", "absent", "10") })
	t.Run("bad-arity", func(t *testing.T) { s := newSandbox(t); s.expect("", 2, "set-credit", "work"); s.noRegistry() })
}

func TestTokens(t *testing.T) {
	for _, row := range []struct{ name, expiry, status string }{{"unknown", "", "pasted-unverified"}, {"future", "2099-12-31", "pasted-unverified"}, {"past", "2000-01-01", "expired"}} {
		t.Run(row.name, func(t *testing.T) {
			s := newSandbox(t)
			s.expect("", 0, "add", "work")
			p := s.get("claude-work")
			login := s.secret("oauth-")
			s.seedItems(map[string]string{services(p)[0]: login})
			token := s.secret("sk-ant-oat01-")
			args := []string{"set-token", "work"}
			if row.expiry != "" {
				args = append(args, "--expires", row.expiry)
			}
			s.expect("export CLAUDE_CODE_OAUTH_TOKEN='"+token+"'\n", 0, args...)
			items := s.items()
			var record struct {
				Version    int      `json:"version"`
				Token      string   `json:"accessToken"`
				Expiry     *float64 `json:"expiresAt"`
				Provenance string   `json:"provenance"`
			}
			if json.Unmarshal([]byte(items[services(p)[2]]), &record) != nil || record.Version != 2 || record.Token != token || record.Provenance != "pasted" {
				t.Fatal("token was not saved in Swift's token schema")
			}
			r := s.expect("", 0, "tokens")
			if !strings.Contains(r.stdout, "work\t"+row.status+"\t") {
				t.Fatal("token status was not derived from saved metadata")
			}
			s.expect("", 0, "clear-token", "work")
			items = s.items()
			if _, ok := items[services(p)[2]]; ok || items[services(p)[0]] != login {
				t.Fatal("clear-token touched the login or left the token")
			}
			s.expect("", 1, "clear-token", "work")
			s.noLeaks()
		})
	}
	t.Run("none", func(t *testing.T) {
		s := newSandbox(t)
		r := s.expect("", 0, "tokens")
		if !strings.Contains(r.stdout, "default\tnone\t-") {
			t.Fatal("missing-token row is incorrect")
		}
	})
	t.Run("unavailable", func(t *testing.T) {
		s := newSandbox(t)
		s.expect("", 0, "add", "work")
		p := s.get("claude-work")
		s.seedItems(map[string]string{services(p)[2]: "invalid saved token"})
		r := s.expect("", 1, "tokens")
		if !strings.Contains(r.stdout, "work\tunavailable\t-") {
			t.Fatal("unreadable token was silently treated as missing")
		}
	})
	for _, raw := range []string{"wrong-token", "sk-ant-oat01-", "export CLAUDE_CODE_OAUTH_TOKEN='unterminated", strings.Repeat("x", 4097)} {
		t.Run("invalid-input", func(t *testing.T) {
			s := newSandbox(t)
			s.expect("", 0, "add", "work")
			s.secrets = append(s.secrets, raw)
			s.expect(raw, 1, "set-token", "work")
		})
	}
	for _, args := range [][]string{{"set-token", "work", "misplaced"}, {"set-token", "work", "--expires", "not-a-date"}, {"clear-token", "work", "extra"}, {"tokens", "extra"}} {
		t.Run("invalid-arguments", func(t *testing.T) { s := newSandbox(t); s.expect("", 2, args...); s.noRegistry() })
	}
	for _, kind := range []string{"apiKey", "endpoint", "codex"} {
		t.Run("not-subscription-"+kind, func(t *testing.T) {
			s := newSandbox(t)
			args := []string{"add", "work"}
			input := ""
			switch kind {
			case "apiKey":
				args = append(args, "--api-key")
				input = s.secret("sk-ant-api03-")
			case "endpoint":
				args = append(args, "--endpoint", "https://example.com", "--model", "x")
				input = s.secret("provider-")
			case "codex":
				args = append(args, "--codex")
			}
			s.expect(input, 0, args...)
			s.expect(s.secret("sk-ant-oat01-"), 1, "set-token", "work")
			s.expect("", 1, "clear-token", "work")
			r := s.expect("", 0, "tokens")
			if !strings.Contains(r.stdout, "work\tn/a\t-") {
				t.Fatal("unsupported profile did not produce n/a token status")
			}
		})
	}
	t.Run("missing-profile", func(t *testing.T) { s := newSandbox(t); s.expect("", 1, "set-token", "absent") })
}

func TestRenameAndRemove(t *testing.T) {
	t.Run("identity-and-all-credential-leftovers", func(t *testing.T) {
		s := newSandbox(t)
		s.expect("", 0, "add", "work")
		p := s.get("claude-work")
		items := map[string]string{}
		for _, service := range services(p) {
			items[service] = s.secret("kept-")
		}
		s.seedItems(items)
		s.write(filepath.Join(p.Directory, "account-data.txt"), []byte("keep data"))
		credentials := s.read(s.credentials)
		s.expect("", 0, "rename", "work", "renamed")
		changed := s.get("claude-renamed")
		if changed.ID != p.ID || changed.Directory != p.Directory {
			t.Fatal("rename changed credential identity")
		}
		s.unchanged(s.credentials, credentials)
		s.expect("", 1, "login", "work")
		r := s.expect("", 0, "remove", "renamed")
		for _, service := range services(p) {
			if !strings.Contains(r.stdout, service) {
				t.Fatal("removal omitted an existing credential namespace")
			}
		}
		s.unchanged(s.credentials, credentials)
		if string(s.read(filepath.Join(p.Directory, "account-data.txt"))) != "keep data" {
			t.Fatal("removal deleted account data")
		}
		state := s.state()
		if len(state.Profiles) != 1 || !contains(state.Commands, "claude-work") || !contains(state.Commands, "claude-renamed") || !contains(state.Directories, p.Directory) {
			t.Fatal("rename and remove did not save tombstones")
		}
		s.noLeaks()
	})
	t.Run("Codex-keeps-auth", func(t *testing.T) {
		s := newSandbox(t)
		s.expect("", 0, "add", "work", "--codex")
		p := s.get("codex-work")
		auth := s.secret("codex-")
		s.write(filepath.Join(p.Directory, "auth.json"), []byte(auth))
		s.expect("", 0, "rename", "work", "renamed")
		if s.get("codex-renamed").Directory != p.Directory {
			t.Fatal("Codex rename moved CODEX_HOME")
		}
		r := s.expect("", 0, "remove", "renamed")
		if !strings.Contains(r.stdout, filepath.Join(p.Directory, "auth.json")) || string(s.read(filepath.Join(p.Directory, "auth.json"))) != auth {
			t.Fatal("Codex removal lost its auth file or did not list it")
		}
		s.noLeaks()
	})
	t.Run("same-name-read-only", func(t *testing.T) {
		s := newSandbox(t)
		s.expect("", 0, "add", "work")
		before := s.read(s.registry)
		s.expect("", 0, "rename", "work", "work")
		s.unchanged(s.registry, before)
	})
	for _, row := range []struct {
		name string
		args []string
		code int
	}{{"rename-default", []string{"rename", "default", "other"}, 1}, {"remove-default", []string{"remove", "default"}, 1}, {"rename-missing", []string{"rename", "absent", "other"}, 1}, {"remove-missing", []string{"remove", "absent"}, 1}, {"reserved-replacement", []string{"rename", "work", "auto"}, 2}, {"invalid-replacement", []string{"rename", "work", "two words"}, 2}, {"rename-arity", []string{"rename", "work"}, 2}, {"remove-arity", []string{"remove", "work", "extra"}, 2}} {
		t.Run(row.name, func(t *testing.T) {
			s := newSandbox(t)
			s.expect("", 0, "list")
			before := s.read(s.registry)
			s.expect("", row.code, row.args...)
			s.unchanged(s.registry, before)
		})
	}
	t.Run("duplicate-replacement", func(t *testing.T) {
		s := newSandbox(t)
		s.expect("", 0, "add", "one")
		s.expect("", 0, "add", "two")
		before := s.read(s.registry)
		s.expect("", 1, "rename", "one", "two")
		s.unchanged(s.registry, before)
	})
}

func contains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func TestLoginAndSetupToken(t *testing.T) {
	for _, kind := range []string{"subscription", "default", "consoleLogin", "apiKey", "codex"} {
		t.Run("login-"+kind, func(t *testing.T) {
			s := newSandbox(t)
			name := "work"
			args := []string{"add", name}
			input := ""
			if kind == "default" {
				name = "default"
				s.expect("", 0, "list")
			} else {
				switch kind {
				case "apiKey", "consoleLogin":
					args = append(args, "--api-key")
					input = s.secret("sk-ant-api03-")
				case "codex":
					args = append(args, "--codex")
				}
				s.expect(input, 0, args...)
			}
			command := "claude-work"
			if kind == "default" {
				command = "claude"
			}
			if kind == "codex" {
				command = "codex-work"
			}
			p := s.get(command)
			if kind == "consoleLogin" {
				state := s.state()
				for i := range state.Profiles {
					if state.Profiles[i].Command == p.Command {
						state.Profiles[i].Kind = "consoleLogin"
					}
				}
				s.writeJSON(s.registry, state)
			}
			directory := p.Directory
			if kind == "default" {
				directory = ""
			}
			requests := s.attachHTTP(directory, kind == "codex", 0, kind == "apiKey")
			login := []string{"login", name}
			if kind == "apiKey" {
				login = append(login, "--console")
			}
			r := s.expect("", 0, login...)
			if !strings.Contains(r.stdout, "fixture CLI completed") || !strings.Contains(r.stderr, "fixture CLI diagnostic") {
				t.Fatal("child CLI output was not forwarded")
			}
			expected := []string{"auth", "login", "--claudeai"}
			if kind == "consoleLogin" || kind == "apiKey" {
				expected = []string{"auth", "login", "--console"}
			}
			if kind == "codex" {
				expected = []string{"login"}
			}
			assertRequest(t, requests, expected...)
			if kind == "apiKey" {
				if s.get(p.Command).Kind != "consoleLogin" || s.items()[services(p)[3]] != input {
					t.Fatal("confirmed Console login did not switch kind while keeping pasted key")
				}
			}
			if kind == "codex" {
				var auth map[string]string
				if json.Unmarshal(s.read(filepath.Join(p.Directory, "auth.json")), &auth) != nil || auth["OPENAI_API_KEY"] != s.secrets[len(s.secrets)-1] {
					t.Fatal("Codex sign-in did not save the credential from its localhost API")
				}
			}
			s.noLeaks()
		})
	}
	t.Run("setup-token-launches", func(t *testing.T) {
		s := newSandbox(t)
		s.expect("", 0, "add", "work")
		p := s.get("claude-work")
		requests := s.attachHTTP(p.Directory, false, 0, false)
		s.expect("", 0, "setup-token", "work")
		assertRequest(t, requests, "setup-token")
	})
	t.Run("exit-code", func(t *testing.T) {
		s := newSandbox(t)
		s.expect("", 0, "add", "work")
		p := s.get("claude-work")
		requests := s.attachHTTP(p.Directory, false, 23, false)
		s.expect("", 23, "login", "work")
		assertRequest(t, requests, "auth", "login", "--claudeai")
	})
	t.Run("Console-without-credential-refused", func(t *testing.T) {
		s := newSandbox(t)
		s.expect(s.secret("sk-ant-api03-"), 0, "add", "work", "--api-key")
		p := s.get("claude-work")
		before := s.read(s.registry)
		requests := s.attachHTTP(p.Directory, false, 0, false)
		s.expect("", 1, "login", "work", "--console")
		assertRequest(t, requests, "auth", "login", "--console")
		s.unchanged(s.registry, before)
	})
	t.Run("failed-Console-keeps-kind", func(t *testing.T) {
		s := newSandbox(t)
		s.expect(s.secret("sk-ant-api03-"), 0, "add", "work", "--api-key")
		p := s.get("claude-work")
		before := s.read(s.registry)
		requests := s.attachHTTP(p.Directory, false, 17, false)
		s.expect("", 17, "login", "work", "--console")
		assertRequest(t, requests, "auth", "login", "--console")
		s.unchanged(s.registry, before)
	})
	t.Run("missing-executable", func(t *testing.T) {
		s := newSandbox(t)
		s.expect("", 0, "add", "work")
		s.extra = append(s.extra, "PATH="+filepath.Join(s.root, "missing-bin"))
		s.expect("", 1, "login", "work")
	})
	for _, row := range []struct {
		name      string
		add, args []string
		keyPrefix string
	}{{"subscription-console", []string{"add", "work"}, []string{"login", "work", "--console"}, ""}, {"API-without-console", []string{"add", "work", "--api-key"}, []string{"login", "work"}, "sk-ant-api03-"}, {"endpoint", []string{"add", "work", "--endpoint", "https://example.com", "--model", "x"}, []string{"login", "work"}, "provider-"}, {"Codex-console", []string{"add", "work", "--codex"}, []string{"login", "work", "--console"}, ""}, {"API-token-setup", []string{"add", "work", "--api-key"}, []string{"setup-token", "work"}, "sk-ant-api03-"}, {"Codex-token-setup", []string{"add", "work", "--codex"}, []string{"setup-token", "work"}, ""}} {
		t.Run("refuse-"+row.name, func(t *testing.T) {
			s := newSandbox(t)
			input := ""
			if row.keyPrefix != "" {
				input = s.secret(row.keyPrefix)
			}
			s.expect(input, 0, row.add...)
			requests := s.attachHTTP("", false, 0, false)
			s.expect("", 1, row.args...)
			select {
			case <-requests:
				t.Fatal("refused login still launched its CLI")
			default:
			}
			s.noLeaks()
		})
	}
	for _, args := range [][]string{{"login", "work", "--unknown"}, {"setup-token", "work", "extra"}} {
		t.Run("bad-arguments", func(t *testing.T) { s := newSandbox(t); s.expect("", 2, args...); s.noRegistry() })
	}
}

func TestAddRefusals(t *testing.T) {
	rows := []struct {
		name string
		args []string
	}{
		{"missing-name", []string{"add"}}, {"reserved-default", []string{"add", "default"}}, {"reserved-auto", []string{"add", "AUTO"}}, {"invalid-name", []string{"add", "two words"}}, {"long-name", []string{"add", strings.Repeat("a", 41)}}, {"secret-as-name", []string{"add", "sk-ant-api03-misplaced"}}, {"conflicting-kinds", []string{"add", "work", "--api-key", "--console"}}, {"codex-conflict", []string{"add", "work", "--codex", "--api-key"}}, {"repeated-option", []string{"add", "work", "--codex", "--codex"}}, {"key-in-argv", []string{"add", "work", "--api-key=misplaced"}}, {"raw-key-in-argv", []string{"add", "work", "--api-key", "misplaced"}}, {"relative-directory", []string{"add", "work", "--directory", "relative"}}, {"directory-no-value", []string{"add", "work", "--directory"}}, {"endpoint-no-model", []string{"add", "work", "--endpoint", "https://example.com"}}, {"model-no-endpoint", []string{"add", "work", "--model", "x"}}, {"bad-scheme", []string{"add", "work", "--endpoint", "http://example.com", "--model", "x"}}, {"userinfo", []string{"add", "work", "--endpoint", "https://user:pass@example.com", "--model", "x"}}, {"query", []string{"add", "work", "--endpoint", "https://example.com/?key=misplaced", "--model", "x"}}, {"bad-model", []string{"add", "work", "--endpoint", "https://example.com", "--model", "two models"}}, {"catalog-alias", []string{"add", "work", "--endpoint", "https://example.com", "--model", "x", "--behaves-as", "sonnet"}}, {"endpoint-directory", []string{"add", "work", "--endpoint", "https://example.com", "--model", "x", "--directory", "/tmp"}}, {"unknown-option", []string{"add", "work", "--unknown"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) { s := newSandbox(t); s.expect("", 2, row.args...); s.noRegistry() })
	}
	for _, row := range []struct{ name, input string }{{"empty", ""}, {"OAuth", "sk-ant-oat01-invalid"}, {"refresh", "sk-ant-ort01-invalid"}, {"admin", "sk-ant-admin01-invalid"}, {"session", "sk-ant-sid01-invalid"}, {"unquoted-export", "export ANTHROPIC_API_KEY='broken"}, {"multiple-commands", "export ANTHROPIC_API_KEY=sk-ant-api03-key\nwhoami"}, {"oversize", strings.Repeat("a", 4097)}, {"non-utf8", string([]byte{0xff})}} {
		t.Run("key-"+row.name, func(t *testing.T) {
			s := newSandbox(t)
			s.secrets = append(s.secrets, row.input)
			s.expect(row.input, 1, "add", "work", "--api-key")
			s.noRegistry()
		})
	}
	t.Run("endpoint-Anthropic-key", func(t *testing.T) {
		s := newSandbox(t)
		key := s.secret("sk-ant-api03-")
		s.expect(key, 1, "add", "work", "--endpoint", "https://example.com", "--model", "x")
		s.noRegistry()
	})
	t.Run("duplicate-name", func(t *testing.T) {
		s := newSandbox(t)
		s.expect("", 0, "add", "work")
		before := s.read(s.registry)
		s.expect("", 1, "add", "work")
		s.unchanged(s.registry, before)
	})
	t.Run("duplicate-directory", func(t *testing.T) {
		s := newSandbox(t)
		s.expect("", 0, "add", "work")
		p := s.get("claude-work")
		before := s.read(s.registry)
		s.expect("", 1, "add", "other", "--directory", p.Directory)
		s.unchanged(s.registry, before)
	})
	t.Run("missing-directory", func(t *testing.T) {
		s := newSandbox(t)
		s.expect("", 1, "add", "work", "--directory", filepath.Join(s.home, "missing"))
		s.noRegistry()
	})
	t.Run("credential-failure-rolls-back", func(t *testing.T) {
		s := newSandbox(t)
		s.expect("", 0, "list")
		before := s.read(s.registry)
		s.write(s.credentials, []byte("invalid store"))
		key := s.secret("sk-ant-api03-")
		s.expect(key, 1, "add", "work", "--api-key")
		s.unchanged(s.registry, before)
		entries, err := os.ReadDir(filepath.Join(filepath.Dir(s.registry), "accounts"))
		if !errors.Is(err, os.ErrNotExist) && (err != nil || len(entries) != 0) {
			t.Fatal("failed publication left an account folder")
		}
		s.noLeaks()
	})
}

func TestImportShell(t *testing.T) {
	t.Run("literal-functions-aliases-and-includes", func(t *testing.T) {
		s := newSandbox(t)
		s.expect("", 0, "list")
		shell := `source "$HOME/wrappers.zsh"
claude-direct() { CLAUDE_CONFIG_DIR="$HOME/direct" claude "$@"; }
function claude-native { _claude-native "$HOME/native" "$@"; }
alias claude-alias='CLAUDE_CONFIG_DIR="$HOME/alias" claude'
claude-dynamic() { CLAUDE_CONFIG_DIR="$(touch "$HOME/must-not-execute")" claude; }
claude-external() { CLAUDE_CODE_USE_BEDROCK=1 CLAUDE_CONFIG_DIR="$HOME/cloud" claude; }
claude-deleted() { CLAUDE_CONFIG_DIR="$HOME/deleted" claude; }
unfunction claude-deleted
cat <<'END'
claude-heredoc() { CLAUDE_CONFIG_DIR="$HOME/heredoc" claude; }
END
source "$HOME/.env"
`
		include := `claude-included() { env CLAUDE_CONFIG_DIR="$HOME/included" claude; }
function unrelated { source "$HOME/not-startup.zsh"; }
`
		s.write(filepath.Join(s.home, ".zshrc"), []byte(shell))
		s.write(filepath.Join(s.home, "wrappers.zsh"), []byte(include))
		s.write(filepath.Join(s.home, "not-startup.zsh"), []byte(`claude-nested() { CLAUDE_CONFIG_DIR="$HOME/nested" claude; }`))
		s.write(filepath.Join(s.home, ".env"), []byte(`claude-sensitive() { CLAUDE_CONFIG_DIR="$HOME/sensitive" claude; }`))
		s.expect("", 0, "import-shell")
		state := s.state()
		commands := map[string]profile{}
		for _, p := range state.Profiles {
			commands[p.Command] = p
		}
		for _, name := range []string{"direct", "native", "alias", "included"} {
			p, ok := commands["claude-"+name]
			if !ok || p.Directory != filepath.Join(s.home, name) || p.Managed || p.ID == "" {
				t.Fatal("literal shell wrapper was not imported correctly")
			}
		}
		if commands["claude-dynamic"].Note == "" {
			t.Fatal("dynamic shell path was not marked unresolved")
		}
		for _, name := range []string{"external", "deleted", "heredoc", "sensitive", "nested"} {
			if _, ok := commands["claude-"+name]; ok {
				t.Fatal("non-startup or external shell wrapper was imported")
			}
		}
		if _, err := os.Stat(filepath.Join(s.home, "must-not-execute")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("shell substitution was executed")
		}
		s.unchanged(filepath.Join(s.home, ".zshrc"), []byte(shell))
		s.expect("", 1, "rename", "dynamic", "renamed")
		before := s.read(s.registry)
		s.expect("", 0, "import-shell")
		s.unchanged(s.registry, before)
	})
	t.Run("tombstones-survive-import", func(t *testing.T) {
		s := newSandbox(t)
		shell := []byte(`claude-work() { CLAUDE_CONFIG_DIR="$HOME/work" claude; }`)
		s.write(filepath.Join(s.home, ".zshrc"), shell)
		s.expect("", 0, "list")
		p := s.get("claude-work")
		s.expect("", 0, "rename", "work", "renamed")
		if s.get("claude-renamed").Directory != p.Directory {
			t.Fatal("imported profile's literal directory changed")
		}
		s.expect("", 0, "import-shell")
		if len(s.state().Profiles) != 2 {
			t.Fatal("old shell wrapper was reimported after rename")
		}
		s.expect("", 0, "remove", "renamed")
		s.expect("", 0, "import-shell")
		if len(s.state().Profiles) != 1 {
			t.Fatal("removed credential identity was reimported")
		}
		s.unchanged(filepath.Join(s.home, ".zshrc"), shell)
	})
	t.Run("default-override", func(t *testing.T) {
		s := newSandbox(t)
		s.write(filepath.Join(s.home, ".zshenv"), []byte(`export CLAUDE_CONFIG_DIR="$HOME/custom"`))
		r := s.expect("", 0, "list")
		if !strings.Contains(r.stdout, "default\tclaude\tneeds-import\t") {
			t.Fatal("default override was not marked unresolved")
		}
		s.expect("", 1, "login", "default")
	})
	t.Run("external-migration", func(t *testing.T) {
		s := newSandbox(t)
		s.expect("", 0, "list")
		state := s.state()
		state.Migration = nil
		p := profile{Command: "claude-cloud", Directory: filepath.Join(s.home, "cloud"), ID: "10000000-0000-4000-8000-000000000003"}
		state.Profiles = append(state.Profiles, p)
		s.writeJSON(s.registry, state)
		s.write(filepath.Join(s.home, ".zshrc"), []byte(`claude-cloud() { CLAUDE_CODE_USE_FOUNDRY=true CLAUDE_CONFIG_DIR="$HOME/cloud" claude; }`))
		s.expect("", 0, "list")
		state = s.state()
		if len(state.Profiles) != 1 || state.Migration == nil || !contains(state.Commands, p.Command) || !contains(state.Directories, p.Directory) {
			t.Fatal("legacy external import was not retired with tombstones")
		}
	})
	t.Run("bad-arity", func(t *testing.T) { s := newSandbox(t); s.expect("", 2, "import-shell", "extra"); s.noRegistry() })
}

func TestRegistryRefusals(t *testing.T) {
	rows := []struct {
		name   string
		change func(*registry)
	}{
		{"version", func(s *registry) { s.Version = 99 }}, {"missing-default", func(s *registry) { s.Profiles = []profile{} }}, {"invalid-ID", func(s *registry) { s.Profiles[0].ID = "not-a-uuid" }}, {"relative-path", func(s *registry) { s.Profiles[0].Directory = "relative" }}, {"duplicate-command", func(s *registry) {
			p := s.Profiles[0]
			p.ID = "10000000-0000-4000-8000-000000000009"
			s.Profiles = append(s.Profiles, p)
		}}, {"unknown-kind", func(s *registry) { s.Profiles[0].Kind = "unknown" }}, {"endpoint-missing", func(s *registry) { s.Profiles[0].Kind = "endpoint" }}, {"invalid-tombstone", func(s *registry) { s.Commands = []string{"claude"} }},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			s := newSandbox(t)
			s.expect("", 0, "list")
			state := s.state()
			row.change(&state)
			s.writeJSON(s.registry, state)
			before := s.read(s.registry)
			s.expect("", 1, "list")
			s.unchanged(s.registry, before)
		})
	}
	for _, row := range []struct {
		name  string
		bytes []byte
	}{{"bad-JSON", []byte("broken registry")}, {"duplicate-key", []byte(`{"version":1,"version":2,"profiles":[]}`)}, {"trailing-JSON", []byte(`{} {}`)}, {"oversize", bytes.Repeat([]byte{'x'}, 2097153)}} {
		t.Run(row.name, func(t *testing.T) {
			s := newSandbox(t)
			s.write(s.registry, row.bytes)
			s.expect("", 1, "list")
			s.unchanged(s.registry, row.bytes)
		})
	}
	t.Run("symlink", func(t *testing.T) {
		s := newSandbox(t)
		target := filepath.Join(s.root, "target.json")
		s.write(target, []byte("keep target"))
		if err := os.Symlink(target, s.registry); err != nil {
			t.Fatal("could not create link fixture")
		}
		s.expect("", 1, "list")
		s.unchanged(target, []byte("keep target"))
	})
	t.Run("hardlink", func(t *testing.T) {
		s := newSandbox(t)
		target := filepath.Join(s.root, "target.json")
		s.write(target, []byte("keep target"))
		if err := os.Link(target, s.registry); err != nil {
			t.Fatal("could not create hardlink fixture")
		}
		s.expect("", 1, "list")
		s.unchanged(target, []byte("keep target"))
	})
	t.Run("FIFO", func(t *testing.T) {
		s := newSandbox(t)
		if err := syscall.Mkfifo(s.registry, 0600); err != nil {
			t.Fatal("could not create FIFO fixture")
		}
		s.expect("", 1, "list")
	})
	t.Run("lock-busy", func(t *testing.T) {
		s := newSandbox(t)
		s.expect("", 0, "list")
		lock, err := os.OpenFile(filepath.Join(filepath.Dir(s.registry), ".registry-lock"), os.O_RDWR, 0600)
		if err != nil {
			t.Fatal("lock fixture unavailable")
		}
		defer lock.Close()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
			t.Fatal("could not hold registry lock")
		}
		defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		s.expect("", 1, "list")
	})
}

func TestConcurrentAdds(t *testing.T) {
	s := newSandbox(t)
	s.expect("", 0, "list")
	const count = 8
	var workers sync.WaitGroup
	results := make(chan result, count)
	for index := 0; index < count; index++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, binary, "profile", "add", fmt.Sprintf("parallel%d", index))
			command.Env = s.environment()
			command.Dir = s.home
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			err := command.Run()
			code := 0
			if err != nil {
				var exit *exec.ExitError
				if errors.As(err, &exit) {
					code = exit.ExitCode()
				} else {
					code = -1
				}
			}
			s.cliResults.Record(code, stdout.String(), stderr.String())
			results <- result{code: code, stdout: stdout.String(), stderr: stderr.String()}
		}(index)
	}
	workers.Wait()
	close(results)
	for row := range results {
		if row.code != 0 {
			t.Fatal("a concurrent add failed")
		}
	}
	state := s.state()
	if len(state.Profiles) != count+1 {
		t.Fatal("concurrent adds lost a registry update")
	}
	ids := map[string]bool{}
	for _, p := range state.Profiles {
		if ids[p.ID] {
			t.Fatal("concurrent adds reused an identity")
		}
		ids[p.ID] = true
	}
	s.noLeaks()
}

func TestCredentialBackendRefusals(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "public-permissions", "bad-version"} {
		t.Run(kind, func(t *testing.T) {
			s := newSandbox(t)
			s.expect("", 0, "add", "work")
			p := s.get("claude-work")
			if kind == "bad-version" {
				s.writeJSON(s.credentials, map[string]any{"version": 99, "items": map[string]string{}})
			} else if kind == "public-permissions" {
				s.seedItems(map[string]string{})
				if err := os.Chmod(s.credentials, 0644); err != nil {
					t.Fatal("permission fixture failed")
				}
			} else {
				target := filepath.Join(s.root, "kept-credential-store.json")
				s.write(target, []byte("keep target"))
				var err error
				if kind == "symlink" {
					err = os.Symlink(target, s.credentials)
				} else {
					err = os.Link(target, s.credentials)
				}
				if err != nil {
					t.Fatal("link fixture failed")
				}
			}
			r := s.expect("", 1, "tokens")
			if !strings.Contains(r.stdout, "work\tunavailable\t-") {
				t.Fatal("unsafe credential backend did not produce an unavailable row")
			}
			s.expect(s.secret("sk-ant-oat01-"), 1, "set-token", "work")
			if len(s.state().Profiles) != 2 || s.get("claude-work").ID != p.ID {
				t.Fatal("credential failure altered registry")
			}
		})
	}
}

func TestUnicodeCredentialIdentity(t *testing.T) {
	s := newSandbox(t)
	decomposed := filepath.Join(s.home, "cafe\u0301")
	composed := filepath.Join(s.home, "caf\u00e9")
	if err := os.Mkdir(decomposed, 0700); err != nil {
		t.Fatal("Unicode fixture directory unavailable")
	}
	if err := os.Mkdir(composed, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		t.Fatal("Unicode fixture directory unavailable")
	}
	key := s.secret("sk-ant-api03-")
	s.expect(key, 0, "add", "accent", "--api-key", "--directory", decomposed)
	p := s.get("claude-accent")
	expected := p
	expected.Directory = composed
	if p.Directory != decomposed || s.items()[services(expected)[3]] != key {
		t.Fatal("literal path was rewritten or its Keychain hash was not NFC")
	}
	s.expect("", 1, "add", "duplicate", "--directory", composed)
	s.noLeaks()
}

func TestSharedSettingsRefusals(t *testing.T) {
	for _, row := range []struct {
		name     string
		settings any
	}{{"helper", map[string]any{"apiKeyHelper": "a helper"}}, {"provider", map[string]any{"env": map[string]any{"CLAUDE_CODE_USE_VERTEX": "1"}}}, {"authentication", map[string]any{"env": map[string]any{"ANTHROPIC_API_KEY": "fixture-canary"}}}, {"base-url", map[string]any{"env": map[string]any{"ANTHROPIC_BASE_URL": "https://example.com"}}}} {
		t.Run(row.name, func(t *testing.T) {
			s := newSandbox(t)
			s.writeJSON(filepath.Join(s.home, ".claude", "settings.json"), row.settings)
			before := s.read(filepath.Join(s.home, ".claude", "settings.json"))
			s.expect("", 1, "add", "work")
			s.noRegistry()
			s.unchanged(filepath.Join(s.home, ".claude", "settings.json"), before)
		})
	}
}

func TestEmptyArguments(t *testing.T) {
	for _, args := range [][]string{{"remove", ""}, {"rename", "", "other"}, {"login", ""}, {"set-key", ""}, {"set-endpoint", ""}, {"set-token", ""}, {"clear-token", ""}, {"setup-token", ""}, {"set-credit", "", "10"}, {"add", "work", "--model", ""}, {"add", "work", "--behaves-as", ""}, {"add", "work", "--endpoint", "https://example.com", "--model", "x", "--behaves-as", ""}, {"set-endpoint", "work", "--behaves-as", ""}} {
		t.Run("refuse", func(t *testing.T) { s := newSandbox(t); s.expect("", 2, args...); s.noRegistry() })
	}
}

func TestLegacyAmbiguousName(t *testing.T) {
	s := newSandbox(t)
	s.expect("", 0, "list")
	state := s.state()
	state.Profiles = append(state.Profiles, profile{Command: "claude-default", Directory: filepath.Join(s.home, "other-default"), ID: "10000000-0000-4000-8000-000000000011"})
	s.writeJSON(s.registry, state)
	s.expect("", 0, "list")
	s.expect("", 1, "rename", "default", "new")
	s.expect("", 0, "rename", "claude-default", "new")
	if s.get("claude-new").Directory != filepath.Join(s.home, "other-default") {
		t.Fatal("exact selector did not resolve the legacy ambiguous profile")
	}
}

func TestRegistryKindVersionMigration(t *testing.T) {
	for _, kind := range []string{"apiKey", "endpoint"} {
		t.Run(kind, func(t *testing.T) {
			s := newSandbox(t)
			s.expect("", 0, "list")
			state := s.state()
			p := profile{Command: "claude-legacy", Directory: filepath.Join(s.home, "legacy"), ID: "10000000-0000-4000-8000-000000000021", Managed: true, Kind: kind}
			expected := 2
			if kind == "endpoint" {
				p.Endpoint = map[string]string{"baseURL": "https://example.com", "model": "model-v1"}
				expected = 3
			}
			state.Profiles = append(state.Profiles, p)
			state.Version = 1
			s.writeJSON(s.registry, state)
			s.expect("", 0, "list")
			if s.state().Version != expected || s.get(p.Command).ID != p.ID {
				t.Fatal("registry kind migration lost its version guard or profile identity")
			}
		})
	}
}

func TestStoredBrowserTokenValidation(t *testing.T) {
	for _, invalid := range []string{"\x1b", "\x7f", "\u00e9"} {
		t.Run("invalid-character", func(t *testing.T) {
			s := newSandbox(t)
			s.expect("", 0, "add", "work")
			p := s.get("claude-work")
			account, organization := "10000000-0000-4000-8000-000000000031", "10000000-0000-4000-8000-000000000032"
			s.writeJSON(filepath.Join(p.Directory, ".claude.json"), map[string]any{"oauthAccount": map[string]string{"accountUuid": account, "organizationUuid": organization}})
			token := s.secret("browser-") + invalid
			s.secrets = append(s.secrets, token)
			record := map[string]any{"version": 2, "accessToken": token, "expiresAt": float64(time.Now().Add(time.Hour).Unix()), "identity": map[string]string{"accountUUID": account, "organizationUUID": organization}, "provenance": "browser"}
			encoded, err := json.Marshal(record)
			if err != nil {
				t.Fatal("browser token fixture could not be encoded")
			}
			s.seedItems(map[string]string{services(p)[2]: string(encoded)})
			r := s.expect("", 1, "tokens")
			if !strings.Contains(r.stdout, "work\tunavailable\t-") {
				t.Fatal("invalid browser token was accepted")
			}
			s.noLeaks()
		})
	}
}

func TestConsoleAddFailureKeepsRegistration(t *testing.T) {
	t.Run("nonzero-child-exit", func(t *testing.T) {
		s := newSandbox(t)
		directory := filepath.Join(s.home, "console")
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal("console fixture directory unavailable")
		}
		requests := s.attachHTTP(directory, false, 21, false)
		s.expect("", 21, "add", "work", "--console", "--directory", directory)
		p := s.get("claude-work")
		if p.Kind != "consoleLogin" || p.Directory != directory {
			t.Fatal("failed sign-in removed or changed the new Console registration")
		}
		assertRequest(t, requests, "auth", "login", "--console")
	})
	t.Run("missing-executable", func(t *testing.T) {
		s := newSandbox(t)
		s.extra = append(s.extra, "PATH="+filepath.Join(s.root, "missing-bin"))
		s.expect("", 1, "add", "work", "--console")
		p := s.get("claude-work")
		if p.Kind != "consoleLogin" {
			t.Fatal("missing CLI removed the new Console registration")
		}
		info, err := os.Stat(p.Directory)
		if err != nil || !info.IsDir() {
			t.Fatal("missing CLI removed the prepared account directory")
		}
	})
}

func TestBrowserTokenIdentityCase(t *testing.T) {
	s := newSandbox(t)
	s.expect("", 0, "add", "work")
	p := s.get("claude-work")
	account, organization := "abcdefab-0000-4000-8000-000000000031", "abcdefab-0000-4000-8000-000000000032"
	s.writeJSON(filepath.Join(p.Directory, ".claude.json"), map[string]any{"oauthAccount": map[string]string{"accountUuid": account, "organizationUuid": organization}})
	token := s.secret("browser-")
	record := map[string]any{"version": 2, "accessToken": token, "expiresAt": float64(time.Now().Add(time.Hour).Unix()), "identity": map[string]string{"accountUUID": strings.ToUpper(account), "organizationUUID": strings.ToUpper(organization)}, "provenance": "browser"}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal("browser token fixture could not be encoded")
	}
	s.seedItems(map[string]string{services(p)[2]: string(encoded)})
	r := s.expect("", 0, "tokens")
	if !strings.Contains(r.stdout, "work\tactive\t") {
		t.Fatal("equivalent UUID spellings did not bind the saved browser token")
	}
	s.noLeaks()
}

func TestMalformedCreditAccountIsPreserved(t *testing.T) {
	for _, bad := range []string{"negative-balance", "bad-date", "missing-requests", "negative-request-cost"} {
		t.Run(bad, func(t *testing.T) {
			s := newSandbox(t)
			s.expect(s.secret("sk-ant-api03-"), 0, "add", "work", "--api-key")
			p := s.get("claude-work")
			account := map[string]any{"balanceUSD": 5, "asOf": "2026-01-01T00:00:00.000Z", "requests": []any{}, "adjustments": []any{}}
			switch bad {
			case "negative-balance":
				account["balanceUSD"] = -1
			case "bad-date":
				account["asOf"] = "invalid date"
			case "missing-requests":
				delete(account, "requests")
			case "negative-request-cost":
				account["requests"] = []any{map[string]any{"id": "request", "sessionID": "session", "at": "2026-01-01T00:00:00.000Z", "costUSD": -1}}
			}
			path := filepath.Join(filepath.Dir(s.registry), "api-credit.json")
			s.writeJSON(path, map[string]any{"version": 1, "profiles": map[string]any{"other": account}})
			before := s.read(path)
			s.expect("", 0, "set-credit", "work", "12.34")
			var ledger struct {
				Profiles map[string]json.RawMessage `json:"profiles"`
			}
			if json.Unmarshal(s.read(path), &ledger) != nil || len(ledger.Profiles) != 1 || ledger.Profiles[p.ID] == nil {
				t.Fatal("setting a balance did not repair malformed account metadata")
			}
			backups, err := filepath.Glob(filepath.Join(filepath.Dir(s.registry), "api-credit.unreadable-*.json"))
			if err != nil || len(backups) != 1 || !bytes.Equal(s.read(backups[0]), before) {
				t.Fatal("malformed ledger bytes were not preserved")
			}
			s.noLeaks()
		})
	}
}

func TestCredentialRefusalDiagnostics(t *testing.T) {
	rows := []struct {
		name, input, kind string
		args              []string
		code              int
		item              string
	}{
		{"set-token-prefix", "sk-ant-oat01-", "subscription", []string{"set-token", "work"}, 1, "inferenceToken"},
		{"set-token-assignment", "export CLAUDE_CODE_OAUTH_TOKEN", "subscription", []string{"set-token", "work"}, 1, "inferenceToken"},
		{"set-token-argv", "sk-ant-oat01-canary", "subscription", []string{"set-token", "work", "sk-ant-oat01-canary"}, 2, "inferenceToken"},
		{"set-token-expiry", "sk-ant-oat01-canary", "subscription", []string{"set-token", "work", "--expires", "sk-ant-oat01-canary"}, 2, "inferenceToken"},
		{"set-key-prefix", "sk-ant-", "apiKey", []string{"set-key", "work"}, 1, "apiKey"},
		{"set-key-assignment", "export ANTHROPIC_API_KEY", "apiKey", []string{"set-key", "work"}, 1, "apiKey"},
		{"set-key-argv", "sk-ant-api03-canary", "apiKey", []string{"set-key", "work", "sk-ant-api03-canary"}, 2, "apiKey"},
		{"endpoint-key-refusal", "sk-ant-api03-canary", "endpoint", []string{"set-key", "work"}, 1, "endpointKey"},
		{"add-key-prefix", "sk-ant-", "subscription", []string{"add", "new", "--api-key"}, 1, "apiKey"},
		{"add-key-assignment", "export ANTHROPIC_API_KEY", "subscription", []string{"add", "new", "--api-key"}, 1, "apiKey"},
		{"add-key-argv", "sk-ant-api03-canary", "subscription", []string{"add", "new", "--api-key", "sk-ant-api03-canary"}, 2, "apiKey"},
		{"set-endpoint-url", "sk-ant-api03-canary", "endpoint", []string{"set-endpoint", "work", "--endpoint", "sk-ant-api03-canary"}, 2, "endpoint"},
		{"set-endpoint-model", "sk-ant-api03-canary", "endpoint", []string{"set-endpoint", "work", "--model", "sk-ant-api03-canary"}, 2, "endpoint"},
		{"set-endpoint-mapping", "sk-ant-oat01-canary", "endpoint", []string{"set-endpoint", "work", "--behaves-as", "sk-ant-oat01-canary"}, 2, "endpoint"},
		{"setup-token-argv", "sk-ant-oat01-canary", "subscription", []string{"setup-token", "work", "sk-ant-oat01-canary"}, 2, "inferenceToken"},
		{"setup-token-kind", "sk-ant-oat01-canary", "apiKey", []string{"setup-token", "work"}, 1, "inferenceToken"},
		{"login-argv", "sk-ant-api03-canary", "subscription", []string{"login", "work", "sk-ant-api03-canary"}, 2, "login"},
		{"login-kind", "sk-ant-api03-canary", "apiKey", []string{"login", "work"}, 1, "login"},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			s := newSandbox(t)
			args := []string{"add", "work"}
			input := ""
			if row.kind == "apiKey" {
				args = append(args, "--api-key")
				input = s.secret("sk-ant-api03-")
			}
			if row.kind == "endpoint" {
				args = append(args, "--endpoint", "https://example.com", "--model", "model-v1")
				input = s.secret("provider-")
			}
			s.expect(input, 0, args...)
			before := s.read(s.registry)
			s.secrets = append(s.secrets, row.input)
			r := s.expect(row.input, row.code, row.args...)
			for _, label := range []string{"profile ", "command ", "option ", "item " + row.item} {
				if !strings.Contains(r.stderr, label) {
					t.Fatal("refusal diagnostic omitted safe profile, option or item context")
				}
			}
			s.unchanged(s.registry, before)
			s.noLeaks()
		})
	}
}
