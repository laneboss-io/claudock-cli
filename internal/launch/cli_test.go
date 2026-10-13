package launch_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/laneboss-io/claudock-cli/internal/testutil"
	"golang.org/x/text/unicode/norm"
)

var cliBinary, normalCLIBinary, probeBinary, buildDirectory string

func TestMain(m *testing.M) {
	var err error
	buildDirectory, err = os.MkdirTemp("", "cc-go-run-build-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot create black-box build directory")
		os.Exit(1)
	}
	buildDirectory, err = filepath.EvalSymlinks(buildDirectory)
	if err != nil {
		fmt.Fprintln(os.Stderr, "cannot resolve black-box build directory")
		os.Exit(1)
	}
	status := func() int {
		defer os.RemoveAll(buildDirectory)
		root, err := filepath.Abs("../..")
		if err != nil {
			fmt.Fprintln(os.Stderr, "cannot locate repository")
			return 1
		}
		overlay := struct {
			Replace map[string]string
		}{Replace: make(map[string]string)}
		if os.Getenv("CLAUDOCK_SECURITY_BASELINE") == "1" {
			// Only test builds can select the exact implementation before these fixes.
			for _, path := range []string{"internal/launch/swap.go", "internal/launch/run.go", "internal/launch/endpoint.go", "internal/launch/environment.go", "internal/store/workspace.go"} {
				command := exec.Command("git", "show", "f39e347:"+path)
				command.Dir = root
				data, err := command.Output()
				replacement := filepath.Join(buildDirectory, strings.ReplaceAll(path, "/", "-"))
				if err != nil || os.WriteFile(replacement, data, 0600) != nil {
					fmt.Fprintln(os.Stderr, "cannot prepare the security regression baseline")
					return 1
				}
				overlay.Replace[filepath.Join(root, path)] = replacement
			}
		}
		// Build against the real baseline command to collect the required red
		// phase. A baseline run still exercises these exact fixtures and probes.
		if os.Getenv("CLAUDOCK_RUN_BASELINE") == "1" {
			cmd := exec.Command("git", "show", "6cd267e:cmd/claudock/main.go")
			cmd.Dir = root
			data, err := cmd.Output()
			if err != nil || os.WriteFile(filepath.Join(buildDirectory, "baseline.go"), data, 0600) != nil {
				fmt.Fprintln(os.Stderr, "cannot read the baseline command")
				return 1
			}
			overlay.Replace[filepath.Join(root, "cmd/claudock/main.go")] = filepath.Join(buildDirectory, "baseline.go")
		}
		data, err := json.Marshal(overlay)
		overlayPath := filepath.Join(buildDirectory, "overlay.json")
		if err != nil || os.WriteFile(overlayPath, data, 0600) != nil {
			fmt.Fprintln(os.Stderr, "cannot prepare baseline command overlay")
			return 1
		}
		cliBinary = filepath.Join(buildDirectory, "claudock")
		normalCLIBinary = filepath.Join(buildDirectory, "claudock-normal")
		probeBinary = filepath.Join(buildDirectory, "probe")
		for _, args := range [][]string{
			{"build", "-tags", "claudock_test", "-overlay", overlayPath, "-o", cliBinary, "./cmd/claudock"},
			{"build", "-overlay", overlayPath, "-o", normalCLIBinary, "./cmd/claudock"},
			{"build", "-o", probeBinary, "./internal/launch/testdata/probe"},
		} {
			cmd := exec.Command("go", args...)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
			output, err := cmd.CombinedOutput()
			if err != nil {
				os.Stderr.Write(output) // Compiler diagnostics, never a CLI environment.
				return 1
			}
		}
		return m.Run()
	}()
	os.Exit(status)
}

type fixtureProfile struct {
	Command         string           `json:"command"`
	ConfigDirectory string           `json:"configDirectory"`
	Kind            string           `json:"authKind,omitempty"`
	Managed         bool             `json:"managed"`
	RegistryID      string           `json:"registryID"`
	IsVertex        bool             `json:"isVertex"`
	DiscoveryNote   string           `json:"discoveryNote,omitempty"`
	Endpoint        *fixtureEndpoint `json:"endpoint,omitempty"`
}

func (p fixtureProfile) name() string {
	if p.Command == "claude" {
		return "default"
	}
	return strings.TrimPrefix(strings.TrimPrefix(p.Command, "claude-"), "codex-")
}

// These identities reproduce the profile fixtures' Swift file-backend format.
func (p fixtureProfile) service(item string) string {
	suffix := ""
	if p.Command != "claude" {
		digest := sha256.Sum256([]byte(norm.NFC.String(p.ConfigDirectory)))
		suffix = "-" + hex.EncodeToString(digest[:4])
	}
	login := "Claude Code-credentials" + suffix
	digest := sha256.Sum256([]byte(login))
	id := hex.EncodeToString(digest[:])
	switch item {
	case "oauth":
		return login
	case "consoleKey":
		return "Claude Code" + suffix
	case "apiKey":
		return "Claudock-apikey-" + id
	case "endpointKey":
		return "Claudock-endpointkey-" + id
	case "inferenceToken":
		return "Claudock-inference-" + id
	default:
		panic("unknown fixture credential item")
	}
}

type fixtureEndpoint struct {
	URL   string `json:"baseURL"`
	Model string `json:"model"`
}

type fixture struct {
	t           *testing.T
	cliResults  *testutil.CLIResults
	binary      string
	afterStart  func(*exec.Cmd) error
	home        string
	cwd         string
	registry    string
	credentials string
	record      string
	profiles    []fixtureProfile
	secrets     map[string]map[string]string
	env         map[string]string
}

func newFixture(t *testing.T, kind string) *fixture {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal("cannot resolve temporary home")
	}
	f := &fixture{t: t, cliResults: testutil.CaptureCLI(t), binary: cliBinary, home: home, cwd: filepath.Join(home, "work"),
		registry: filepath.Join(home, "Library/Application Support/Claudock/profiles.json"), credentials: filepath.Join(home, "vault", "credentials.json"),
		record: filepath.Join(home, "record.json"), secrets: make(map[string]map[string]string), env: make(map[string]string)}
	// t.TempDir's leaf follows the umask; the real backend requires a 0700 parent.
	for _, path := range []string{f.cwd, filepath.Join(home, "bin"), filepath.Dir(f.credentials)} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal("cannot create temporary fixture directory")
		}
	}
	probe, err := os.ReadFile(probeBinary)
	if err != nil {
		t.Fatal("cannot read built vendor probe")
	}
	f.write(filepath.Join(home, "bin/claude"), probe, 0700)
	f.write(filepath.Join(home, "bin/codex"), probe, 0700)
	f.addProfile("one", kind)
	f.env["PROBE_PROFILE"] = "one"
	f.env["PROBE_STATUS"] = "0"
	return f
}

func (f *fixture) write(path string, data []byte, mode os.FileMode) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil || os.WriteFile(path, data, mode) != nil {
		f.t.Fatal("cannot write temporary fixture file")
	}
}

func (f *fixture) writeJSON(path string, value any) {
	f.t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		f.t.Fatal("cannot encode temporary fixture")
	}
	f.write(path, data, 0600)
}

func (f *fixture) addProfile(name, kind string) {
	f.t.Helper()
	config := filepath.Join(f.home, "profiles", name)
	if err := os.MkdirAll(config, 0700); err != nil {
		f.t.Fatal("cannot create temporary profile")
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		f.t.Fatal("cannot create temporary profile identity")
	}
	id[6], id[8] = id[6]&0x0f|0x40, id[8]&0x3f|0x80
	command := "claude-" + name
	if kind == "codex" {
		command = "codex-" + name
	}
	p := fixtureProfile{Command: command, ConfigDirectory: config, Kind: kind, Managed: true,
		RegistryID: fmt.Sprintf("%x-%x-%x-%x-%x", id[:4], id[4:6], id[6:8], id[8:10], id[10:])}
	if kind == "endpoint" {
		p.Endpoint = &fixtureEndpoint{"https://api.example.test/anthropic", "provider-model"}
	}
	f.profiles = append(f.profiles, p)
	f.secrets[name] = make(map[string]string)
	switch kind {
	case "apiKey":
		f.secrets[name]["apiKey"] = f.randomSecret("sk-ant-api03-")
	case "consoleLogin":
		f.secrets[name]["consoleKey"] = f.randomSecret("sk-ant-api03-")
	case "endpoint":
		f.secrets[name]["endpointKey"] = f.randomSecret("provider-")
	case "", "subscription":
		f.secrets[name]["oauth"] = f.randomSecret("sk-ant-oat01-")
	}
}

func (f *fixture) randomSecret(prefix string) string {
	f.t.Helper()
	var token [24]byte
	if _, err := rand.Read(token[:]); err != nil {
		f.t.Fatal("cannot create canary credential")
	}
	secret := prefix + hex.EncodeToString(token[:])
	f.cliResults.Redact(secret)
	return secret
}

type result struct {
	status int
	out    string
	err    string
}

func (f *fixture) invoke(args ...string) result {
	f.t.Helper()
	mainProfiles, codexProfiles := []fixtureProfile{}, []fixtureProfile{}
	items := make(map[string]string)
	version, hasDefault := 1, false
	for _, p := range f.profiles {
		if p.Kind == "codex" {
			codexProfiles = append(codexProfiles, p)
			continue
		}
		mainProfiles = append(mainProfiles, p)
		hasDefault = hasDefault || p.Command == "claude"
		if p.Kind == "endpoint" {
			version = 3
		} else if (p.Kind == "apiKey" || p.Kind == "consoleLogin") && version < 2 {
			version = 2
		}
		for item, value := range f.secrets[p.name()] {
			switch item {
			case "oauth":
				value = string(f.json(map[string]any{"claudeAiOauth": map[string]any{
					"accessToken": value, "expiresAt": time.Now().Add(time.Hour).UnixMilli(),
				}}))
			case "inferenceToken":
				if !strings.HasPrefix(value, "{") {
					value = string(f.json(map[string]any{"version": 2, "accessToken": value, "provenance": "pasted"}))
				}
			}
			f.cliResults.Redact(value)
			items[p.service(item)] = value
		}
	}
	if !hasDefault {
		mainProfiles = append(mainProfiles, fixtureProfile{Command: "claude", Kind: "subscription",
			RegistryID: "00000000-0000-4000-8000-000000000001", DiscoveryNote: "fixture default profile is unresolved"})
	}
	for _, registry := range []struct {
		path     string
		version  int
		profiles []fixtureProfile
	}{{f.registry, version, mainProfiles}, {filepath.Join(filepath.Dir(f.registry), "codex-profiles.json"), 1, codexProfiles}} {
		f.writeJSON(registry.path, map[string]any{"version": registry.version, "profiles": registry.profiles,
			"suppressedCommands": []string{}, "suppressedDirectories": []string{}, "subscriptionOnlyMigration": 1})
	}
	f.writeJSON(f.credentials, map[string]any{"version": 1, "items": items})
	before := f.read(f.credentials)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.binary, args...)
	cmd.Dir = f.cwd
	cmd.Env = []string{
		"HOME=" + f.home, "PATH=" + filepath.Join(f.home, "bin"), "TMPDIR=" + f.home,
		"CLAUDOCK_REGISTRY=" + f.registry, "CLAUDOCK_CREDENTIAL_STORE=file:" + f.credentials,
		"PROBE_RECORD=" + f.record, "LANG=C",
	}
	for key, value := range f.env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Start()
	var controlErr error
	if err == nil {
		if f.afterStart != nil {
			controlErr = f.afterStart(cmd)
			if controlErr != nil {
				if signalErr := cmd.Process.Signal(syscall.SIGTERM); signalErr != nil && !errors.Is(signalErr, os.ErrProcessDone) {
					f.t.Error("could not terminate the controlled fixture CLI")
				}
			}
		}
		err = cmd.Wait()
	}
	status := 0
	if err != nil {
		var exited *exec.ExitError
		if !errors.As(err, &exited) {
			f.cliResults.Record(-1, stdout.String(), stderr.String())
			f.t.Fatal("cannot start the built CLI")
		}
		status = exited.ExitCode()
		if wait, ok := exited.Sys().(syscall.WaitStatus); ok && wait.Signaled() {
			status = 128 + int(wait.Signal())
		}
	}
	f.cliResults.Record(status, stdout.String(), stderr.String())
	if controlErr != nil {
		f.t.Fatal("could not observe the controlled fixture CLI")
	}
	if ctx.Err() != nil {
		f.t.Fatal("CLI did not finish within the bounded test time")
	}
	if !bytes.Equal(before, f.read(f.credentials)) {
		f.t.Fatal("a command rewrote its credential store")
	}
	f.noSecrets(stdout.Bytes(), stderr.Bytes())
	if data, err := os.ReadFile(f.record); err == nil {
		f.noSecrets(data)
	}
	return result{status, stdout.String(), stderr.String()}
}

func (f *fixture) json(value any) []byte {
	f.t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		f.t.Fatal("cannot encode temporary credential item")
	}
	return data
}

func (f *fixture) read(path string) []byte {
	f.t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		f.t.Fatal("expected fixture file is missing or unreadable")
	}
	return data
}

func (f *fixture) noSecrets(outputs ...[]byte) {
	f.t.Helper()
	for _, items := range f.secrets {
		for _, secret := range items {
			values := []string{secret}
			var payload struct {
				Access string `json:"accessToken"`
			}
			if json.Unmarshal([]byte(secret), &payload) == nil && payload.Access != "" {
				values = append(values, payload.Access)
			}
			for _, value := range values {
				for _, output := range outputs {
					if value != "" && bytes.Contains(output, []byte(value)) {
						f.t.Fatal("a canary credential escaped its store or child environment")
					}
				}
			}
		}
	}
}

type probeRecord struct {
	PID     int             `json:"pid"`
	Program string          `json:"program"`
	Args    []string        `json:"args"`
	Names   []string        `json:"names"`
	Checks  map[string]bool `json:"checks"`
	CWD     string          `json:"cwd"`
}

func (f *fixture) launched(program string, args []string) probeRecord {
	f.t.Helper()
	var record probeRecord
	if json.Unmarshal(f.read(f.record), &record) != nil {
		f.t.Fatal("the launched vendor did not write a valid record")
	}
	if record.Program != program || record.CWD != f.cwd || !reflect.DeepEqual(record.Args, args) {
		f.t.Fatal("the vendor executable, argv, or working directory changed")
	}
	for _, key := range []string{"config", "credential"} {
		if !record.Checks[key] {
			f.t.Fatalf("the vendor failed its %s comparison", key)
		}
	}
	return record
}

func (f *fixture) refused() {
	f.t.Helper()
	if _, err := os.Lstat(f.record); !os.IsNotExist(err) {
		f.t.Fatal("a refused command started the vendor program")
	}
}

func requireStatus(t *testing.T, got result, want int) {
	t.Helper()
	if got.status != want {
		t.Fatalf("CLI exit status = %d, want %d", got.status, want)
	}
}

func TestRun(t *testing.T) {
	rows := []struct {
		name, kind string
		args       []string
		status     int
		setup      func(*fixture)
	}{
		{"subscription-literal-argv", "subscription", []string{"space prompt", "\"quotes\"", "$(touch unexpected)", ""}, 0, nil},
		{"default-config", "subscription", []string{"-p", "hello"}, 0, func(f *fixture) {
			f.profiles[0].Command = "claude"
			f.profiles[0].Managed = false
			f.env["PROBE_PROFILE"] = "default"
			f.secrets["default"] = f.secrets["one"]
		}},
		{"subscription-inference", "subscription", []string{"-p", "hello"}, 0, func(f *fixture) {
			f.secrets["one"]["inferenceToken"] = f.randomSecret("sk-ant-oat01-")
		}},
		{"setup-token-bypasses-saved-token", "subscription", []string{"setup-token"}, 0, func(f *fixture) {
			f.secrets["one"]["inferenceToken"] = f.randomSecret("sk-ant-oat01-")
			f.preferences(true)
		}},
		{"api-key", "apiKey", []string{"-p", "hello"}, 0, nil},
		{"console-login", "consoleLogin", []string{"-p", "hello"}, 0, nil},
		{"codex-isolation", "codex", []string{"exec", "literal prompt"}, 0, func(f *fixture) {
			f.env["OPENAI_API_KEY"] = f.randomSecret("inherited-")
			f.env["CODEX_HOME"] = filepath.Join(f.home, "wrong")
		}},
		{"child-status", "subscription", []string{"-p", "hello"}, 23, func(f *fixture) { f.env["PROBE_STATUS"] = "23" }},
		{"child-signal", "subscription", []string{"-p", "hello"}, 143, func(f *fixture) { f.env["PROBE_SIGNAL"] = "TERM" }},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := newFixture(t, row.kind)
			if row.setup != nil {
				row.setup(f)
			}
			// All inherited provider/authentication sources must be removed.
			for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL",
				"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_REFRESH_TOKEN", "CLAUDECODE", "CLAUDE_CODE_USE_BEDROCK",
				"CLAUDE_CONFIG_DIR", "ANTHROPIC_CUSTOM_HEADERS", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR"} {
				f.env[key] = "inherited"
			}
			selector := "one"
			if row.name == "default-config" {
				selector = "default"
			}
			args := append([]string{"run", selector, "--"}, row.args...)
			got := f.invoke(args...)
			requireStatus(t, got, row.status)
			program := "claude"
			if row.kind == "codex" {
				program = "codex"
			}
			record := f.launched(program, row.args)
			for _, key := range []string{"ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_BASE_URL", "CLAUDE_CODE_OAUTH_REFRESH_TOKEN",
				"CLAUDECODE", "CLAUDE_CODE_USE_BEDROCK", "ANTHROPIC_CUSTOM_HEADERS", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR"} {
				for _, name := range record.Names {
					if name == key {
						t.Fatalf("inherited %s reached the vendor", key)
					}
				}
			}
			if got.out != "probe stdout\n" || !strings.Contains(got.err, "probe stderr\n") {
				t.Fatal("vendor stdout or stderr was not inherited")
			}
		})
	}
}

func (f *fixture) preferences(required bool) {
	f.t.Helper()
	boolean := "<false/>"
	if required {
		boolean = "<true/>"
	}
	f.write(filepath.Join(f.home, "Library/Preferences/io.github.claudeusage.ClaudeUsage.plist"),
		[]byte("<plist><dict><key>requireInferenceToken</key>"+boolean+"</dict></plist>"), 0600)
}

func TestRunRefusals(t *testing.T) {
	rows := []struct {
		name, kind string
		args       []string
		status     int
		setup      func(*fixture)
	}{
		{"missing-name", "subscription", []string{"run"}, 2, nil},
		{"invalid-name", "subscription", []string{"run", "../one"}, 2, nil},
		{"unknown-profile", "subscription", []string{"run", "missing"}, 1, nil},
		{"missing-boundary", "subscription", []string{"run", "one", "-p"}, 2, nil},
		{"repeated-override", "subscription", []string{"run", "one", "--allow-cross-provider-resume", "--allow-cross-provider-resume"}, 2, nil},
		{"key-as-name", "subscription", []string{"run", "sk-" + "ant-api03-not-a-profile"}, 2, nil},
		{"missing-api-key", "apiKey", []string{"run", "one"}, 1, func(f *fixture) { delete(f.secrets["one"], "apiKey") }},
		{"oauth-as-api-key", "apiKey", []string{"run", "one"}, 1, func(f *fixture) { f.secrets["one"]["apiKey"] = f.randomSecret("sk-ant-oat01-") }},
		{"console-not-signed-in", "consoleLogin", []string{"run", "one"}, 1, func(f *fixture) { delete(f.secrets["one"], "consoleKey") }},
		{"require-token-missing", "subscription", []string{"run", "one"}, 1, func(f *fixture) { f.preferences(true) }},
		{"relative-config", "subscription", []string{"run", "one"}, 1, func(f *fixture) { f.profiles[0].ConfigDirectory = "relative" }},
		{"unknown-kind", "invalid", []string{"run", "one"}, 1, nil},
		{"provider-config", "subscription", []string{"run", "one"}, 1, func(f *fixture) {
			f.writeJSON(filepath.Join(f.profiles[0].ConfigDirectory, "settings.json"), map[string]any{"env": map[string]string{"CLAUDE_CODE_USE_VERTEX": "1"}})
		}},
		{"helper-config", "subscription", []string{"run", "one"}, 1, func(f *fixture) {
			f.writeJSON(filepath.Join(f.profiles[0].ConfigDirectory, "settings.local.json"), map[string]string{"apiKeyHelper": "exit 0"})
		}},
		{"malformed-config", "subscription", []string{"run", "one"}, 1, func(f *fixture) {
			f.write(filepath.Join(f.profiles[0].ConfigDirectory, "settings.json"), []byte("{"), 0600)
		}},
		{"settings-fifo", "subscription", []string{"run", "one"}, 1, func(f *fixture) {
			if syscall.Mkfifo(filepath.Join(f.profiles[0].ConfigDirectory, "settings.json"), 0600) != nil {
				f.t.Fatal("cannot create temporary FIFO")
			}
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := newFixture(t, row.kind)
			if row.setup != nil {
				row.setup(f)
			}
			got := f.invoke(row.args...)
			requireStatus(t, got, row.status)
			if got.out != "" || got.err == "" {
				t.Fatal("a refusal must have only an error diagnostic")
			}
			f.refused()
		})
	}
}

const sessionID = "11111111-2222-4333-8444-555555555555"

func (f *fixture) transcript(profile int, id, model string, extra map[string]any) string {
	f.t.Helper()
	canonical, err := filepath.EvalSymlinks(f.cwd)
	if err != nil {
		f.t.Fatal("cannot resolve session cwd")
	}
	slug := strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, canonical)
	path := filepath.Join(f.profiles[profile].ConfigDirectory, "projects", slug, id+".jsonl")
	record := map[string]any{"type": "assistant", "uuid": id, "parentUuid": nil, "cwd": canonical,
		"message": map[string]string{"model": model}}
	for key, value := range extra {
		record[key] = value
	}
	data, err := json.Marshal(record)
	if err != nil {
		f.t.Fatal("cannot encode session fixture")
	}
	f.write(path, append(data, '\n'), 0600)
	return path
}

func TestEndpoint(t *testing.T) {
	rows := []struct {
		name   string
		args   []string
		status int
		setup  func(*fixture) []string
	}{
		{"pinned", []string{"-p", "hello"}, 0, nil},
		{"pinned-model-options", []string{"--model=provider-model", "--fallback-model", "provider-model,provider-model"}, 0, nil},
		{"settings-merge", []string{"--settings", "{\"theme\":\"dark\",\"env\":{\"EDITOR\":\"vi\"}}", "-p"}, 0, nil},
		{"settings-file", nil, 0, func(f *fixture) []string {
			f.writeJSON(filepath.Join(f.cwd, "extra.json"), map[string]string{"theme": "dark"})
			return []string{"--settings=extra.json", "-p"}
		}},
		{"one-million-pin", []string{"-p"}, 0, func(f *fixture) []string { f.profiles[0].Endpoint.Model += "[1m]"; return nil }},
		{"different-model", []string{"--model", "other-model"}, 2, nil},
		{"different-model-after-boundary", []string{"--", "--model=other-model"}, 2, nil},
		{"different-fallback", []string{"--fallback-model=provider-model,other-model"}, 2, nil},
		{"different-advisor", []string{"--advisor", "other-model"}, 2, nil},
		{"settings-auth-conflict", []string{"--settings", "{\"env\":{\"ANTHROPIC_AUTH_TOKEN\":\"disallowed\"}}"}, 2, nil},
		{"duplicate-settings", []string{"--settings", "{}", "--settings", "{}"}, 2, nil},
		{"duplicate-json-key", []string{"--settings", "{\"theme\":\"dark\",\"theme\":\"light\"}"}, 2, nil},
		{"unreadable-settings", []string{"--settings", "missing.json"}, 2, nil},
		{"unsupported-worktree", []string{"--worktree"}, 2, nil},
		{"project-token-override", nil, 2, func(f *fixture) []string {
			f.writeJSON(filepath.Join(f.cwd, ".claude/settings.local.json"), map[string]any{"env": map[string]string{"ANTHROPIC_AUTH_TOKEN": "disallowed"}})
			return nil
		}},
		{"missing-endpoint-key", nil, 1, func(f *fixture) []string { delete(f.secrets["one"], "endpointKey"); return nil }},
		{"anthropic-endpoint-key", nil, 1, func(f *fixture) []string {
			f.secrets["one"]["endpointKey"] = f.randomSecret("sk-ant-api03-")
			return nil
		}},
		{"cross-provider-id", []string{"--resume", sessionID}, 2, func(f *fixture) []string { f.transcript(0, sessionID, "claude-sonnet-4-6", nil); return nil }},
		{"cross-provider-continue", []string{"-pc"}, 2, func(f *fixture) []string { f.transcript(0, sessionID, "claude-sonnet-4-6", nil); return nil }},
		{"matching-resume", []string{"--resume=" + sessionID}, 0, func(f *fixture) []string { f.transcript(0, sessionID, "provider-model", nil); return nil }},
		{"requested-pin", []string{"-r" + sessionID}, 0, func(f *fixture) []string {
			f.transcript(0, sessionID, "claude-sonnet-4-6", map[string]any{"requestedModel": "provider-model"})
			return nil
		}},
		{"synthetic-resume", []string{"--resume", sessionID}, 0, func(f *fixture) []string { f.transcript(0, sessionID, "<synthetic>", nil); return nil }},
		{"required-value-is-not-continue", []string{"--append-system-prompt", "-c"}, 0, func(f *fixture) []string { f.transcript(0, sessionID, "claude-sonnet-4-6", nil); return nil }},
		{"title-resume", []string{"--resume", "my title"}, 2, func(f *fixture) []string {
			f.transcript(0, sessionID, "claude-sonnet-4-6", map[string]any{"customTitle": "my title"})
			return nil
		}},
		{"path-resume", nil, 2, func(f *fixture) []string {
			return []string{"--resume", f.transcript(0, sessionID, "claude-sonnet-4-6", nil)}
		}},
		{"subagent-resume", []string{"--resume", sessionID}, 2, func(f *fixture) []string {
			path := f.transcript(0, sessionID, "provider-model", nil)
			f.writeJSON(filepath.Join(filepath.Dir(path), sessionID, "subagents/agent.jsonl"),
				map[string]any{"type": "assistant", "message": map[string]string{"model": "claude-sonnet-4-6"}})
			return nil
		}},
		{"shared-subagent-resume", []string{"--resume", sessionID}, 2, func(f *fixture) []string {
			path := f.transcript(0, sessionID, "provider-model", nil)
			target := filepath.Join(f.home, "shared-subagents")
			f.writeJSON(filepath.Join(target, "agent.jsonl"),
				map[string]any{"type": "assistant", "message": map[string]string{"model": "claude-sonnet-4-6"}})
			parent := filepath.Join(filepath.Dir(path), sessionID)
			if os.MkdirAll(parent, 0700) != nil || os.Symlink(target, filepath.Join(parent, "subagents")) != nil {
				f.t.Fatal("cannot create shared subagent fixture")
			}
			return nil
		}},
		{"large-continue-transcript", []string{"-pc"}, 2, func(f *fixture) []string {
			path := f.transcript(0, sessionID, "claude-sonnet-4-6", nil)
			first := f.read(path)
			// The foreign assistant record remains a real JSONL entry even
			// when following user records make the transcript larger than
			// the metadata reader's bounded whole-file read.
			line := []byte("{\"type\":\"user\",\"message\":\"padding\"}\n")
			padding := bytes.Repeat(line, (65<<20)/len(line)+1)
			f.write(path, append(first, padding...), 0600)
			return nil
		}},
		{"allow-cross-provider", []string{"--resume", sessionID}, 0, func(f *fixture) []string {
			f.transcript(0, sessionID, "claude-sonnet-4-6", nil)
			f.env["TEST_ALLOW"] = "1"
			return nil
		}},
		{"picker-warning", []string{"--resume"}, 0, nil},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := newFixture(t, "endpoint")
			args := row.args
			if row.setup != nil {
				if replacement := row.setup(f); replacement != nil {
					args = replacement
				}
			}
			f.env["OTEL_EXPORTER_OTLP_ENDPOINT"] = "http://127.0.0.1:1"
			prefix := []string{"run", "one"}
			if f.env["TEST_ALLOW"] == "1" {
				prefix = append(prefix, "--allow-cross-provider-resume")
			}
			got := f.invoke(append(append(prefix, "--"), args...)...)
			requireStatus(t, got, row.status)
			if row.status != 0 {
				f.refused()
				if got.out != "" || got.err == "" {
					t.Fatal("endpoint refusal did not produce an error")
				}
				return
			}
			var record probeRecord
			if json.Unmarshal(f.read(f.record), &record) != nil || len(record.Args) < 2 {
				t.Fatal("endpoint vendor did not run with pinned settings")
			}
			rest := args
			if strings.HasPrefix(row.name, "settings-") {
				rest = []string{"-p"}
			}
			f.launched("claude", append(record.Args[:2], rest...))
			for _, key := range []string{"endpoint", "model", "settings", "settings-file"} {
				if !record.Checks[key] {
					t.Fatalf("endpoint comparison failed: %s", key)
				}
			}
			for _, name := range record.Names {
				if name == "OTEL_EXPORTER_OTLP_ENDPOINT" || name == "ANTHROPIC_API_KEY" || name == "CLAUDE_CODE_OAUTH_TOKEN" {
					t.Fatal("endpoint received an inherited credential or telemetry setting")
				}
			}
			if row.name == "picker-warning" && !strings.Contains(got.err, "cannot be checked") {
				t.Fatal("an uncheckable picker did not warn")
			}
		})
	}
}

func TestInferenceTokenStates(t *testing.T) {
	rows := []struct {
		name, provenance            string
		expired, required, mismatch bool
		status                      int
	}{
		{"pasted-active", "pasted", false, false, false, 0},
		{"pasted-expired", "pasted", true, false, false, 1},
		{"pasted-expired-required", "pasted", true, true, false, 1},
		{"browser-active", "browser", false, false, false, 0},
		{"browser-expired-fallback", "browser", true, false, false, 0},
		{"browser-expired-required", "browser", true, true, false, 1},
		{"browser-account-mismatch", "browser", false, false, true, 1},
		{"pasted-account-mismatch", "pasted", false, false, true, 1},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := newFixture(t, "subscription")
			expiry := time.Now().Add(time.Hour).Unix()
			if row.expired {
				expiry = time.Now().Add(-time.Hour).Unix()
			}
			account := "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee"
			organization := "bbbbbbbb-cccc-4ddd-8eee-ffffffffffff"
			item := map[string]any{"version": 2, "accessToken": f.randomSecret("sk-ant-oat01-"),
				"expiresAt": expiry, "provenance": row.provenance,
				"identity": map[string]string{"accountUUID": account, "organizationUUID": organization}}
			data, err := json.Marshal(item)
			if err != nil {
				t.Fatal("cannot encode a token fixture")
			}
			f.secrets["one"]["inferenceToken"] = string(data)
			if row.mismatch {
				account = "cccccccc-dddd-4eee-8fff-aaaaaaaaaaaa"
			}
			f.writeJSON(filepath.Join(f.profiles[0].ConfigDirectory, ".claude.json"),
				map[string]any{"oauthAccount": map[string]string{"accountUuid": account, "organizationUuid": organization}})
			f.preferences(row.required)
			got := f.invoke("run", "one", "--", "-p", "hello")
			requireStatus(t, got, row.status)
			if row.status == 0 {
				f.launched("claude", []string{"-p", "hello"})
			} else {
				f.refused()
				if got.err == "" || got.out != "" {
					t.Fatal("token refusal did not produce an error")
				}
			}
		})
	}
}

func TestShellLifecycle(t *testing.T) {
	f := newFixture(t, "subscription")
	base := filepath.Join(f.home, ".config/claudock")
	got := f.invoke("shell", "status")
	requireStatus(t, got, 0)
	if got.out != "disabled\n" || got.err != "" {
		t.Fatal("initial shell status is incorrect")
	}
	if _, err := os.Lstat(base); !os.IsNotExist(err) {
		t.Fatal("status created a shell integration directory")
	}
	original := []byte("# my shell settings\nalias useful='pwd'\n")
	shell := filepath.Join(f.home, ".zshrc")
	f.write(shell, original, 0644)
	got = f.invoke("shell", "enable")
	requireStatus(t, got, 0)
	if !strings.Contains(got.out, "enabled") || got.err != "" {
		t.Fatal("enable did not report success")
	}
	enabled := f.read(shell)
	if !bytes.HasPrefix(enabled, original) || bytes.Count(enabled, []byte("# >>> Claudock shell integration >>>")) != 1 {
		t.Fatal("enable did not preserve and mark shell content")
	}
	for _, path := range []string{filepath.Join(base, "init.zsh"), filepath.Join(base, "integration.json")} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("managed shell file does not have private permissions")
		}
	}
	info, err := os.Stat(shell)
	if err != nil || info.Mode().Perm() != 0644 {
		t.Fatal("enable changed .zshrc permissions")
	}
	backups, err := filepath.Glob(filepath.Join(f.home, ".zshrc.claudock-backup-*"))
	if err != nil || len(backups) != 1 || !bytes.Equal(f.read(backups[0]), original) {
		t.Fatal("enable did not keep an exact backup of shell bytes")
	}
	var state struct {
		Version int
		CLIPath string
	}
	if json.Unmarshal(f.read(filepath.Join(base, "integration.json")), &state) != nil || state.Version != 4 || state.CLIPath != cliBinary {
		t.Fatal("shell ownership record is not compatible with the Swift adapter")
	}
	initBefore := f.read(filepath.Join(base, "init.zsh"))
	got = f.invoke("shell", "enable")
	requireStatus(t, got, 0)
	if !bytes.Equal(enabled, f.read(shell)) || !bytes.Equal(initBefore, f.read(filepath.Join(base, "init.zsh"))) {
		t.Fatal("repeated enable changed managed bytes")
	}
	got = f.invoke("shell", "status")
	requireStatus(t, got, 0)
	if got.out != "enabled\n" || got.err != "" {
		t.Fatal("installed shell status is incorrect")
	}
	tail := []byte("# later user edit\n")
	f.write(shell, append(enabled, tail...), 0644)
	got = f.invoke("shell", "disable")
	requireStatus(t, got, 0)
	if !strings.Contains(got.out, "disabled") || got.err != "" || !bytes.Equal(f.read(shell), append(original, tail...)) {
		t.Fatal("disable did not preserve user shell settings")
	}
	for _, path := range []string{filepath.Join(base, "init.zsh"), filepath.Join(base, "integration.json")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatal("disable did not remove the owned shell file")
		}
	}
	got = f.invoke("shell", "disable")
	requireStatus(t, got, 0)
	got = f.invoke("shell", "status")
	requireStatus(t, got, 0)
	if got.out != "disabled\n" {
		t.Fatal("disabled shell still reports enabled")
	}
	f.refused() // Shell commands must never start a vendor.
}

func TestShellRefusals(t *testing.T) {
	rows := []struct {
		name    string
		command []string
		status  int
		setup   func(*fixture)
	}{
		{"missing-subcommand", []string{"shell"}, 2, nil},
		{"unknown-subcommand", []string{"shell", "unknown"}, 2, nil},
		{"extra-argument", []string{"shell", "enable", "extra"}, 2, nil},
		{"linked-shell", []string{"shell", "enable"}, 1, func(f *fixture) {
			target := filepath.Join(f.home, "target")
			f.write(target, []byte("# target\n"), 0600)
			if os.Symlink(target, filepath.Join(f.home, ".zshrc")) != nil {
				f.t.Fatal("cannot create shell symlink")
			}
		}},
		{"dangling-shell", []string{"shell", "enable"}, 1, func(f *fixture) {
			if os.Symlink(filepath.Join(f.home, "missing"), filepath.Join(f.home, ".zshrc")) != nil {
				f.t.Fatal("cannot create dangling shell symlink")
			}
		}},
		{"hardlinked-shell", []string{"shell", "enable"}, 1, func(f *fixture) {
			target := filepath.Join(f.home, "target")
			f.write(target, []byte("# target\n"), 0600)
			if os.Link(target, filepath.Join(f.home, ".zshrc")) != nil {
				f.t.Fatal("cannot create shell hardlink")
			}
		}},
		{"linked-config", []string{"shell", "enable"}, 1, func(f *fixture) {
			target := filepath.Join(f.home, "config-target")
			if os.Mkdir(target, 0700) != nil || os.Symlink(target, filepath.Join(f.home, ".config")) != nil {
				f.t.Fatal("cannot create config symlink")
			}
		}},
		{"declared-zdotdir", []string{"shell", "enable"}, 1, func(f *fixture) {
			f.write(filepath.Join(f.home, ".zshenv"), []byte("export ZDOTDIR='/custom'\n"), 0600)
		}},
		{"environment-zdotdir", []string{"shell", "enable"}, 1, func(f *fixture) { f.env["ZDOTDIR"] = filepath.Join(f.home, "custom") }},
		{"incomplete-markers", []string{"shell", "enable"}, 1, func(f *fixture) {
			f.write(filepath.Join(f.home, ".zshrc"), []byte("# >>> Claudock shell integration >>>\n"), 0600)
		}},
		{"shell-fifo", []string{"shell", "status"}, 1, func(f *fixture) {
			if syscall.Mkfifo(filepath.Join(f.home, ".zshrc"), 0600) != nil {
				f.t.Fatal("cannot create shell FIFO")
			}
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := newFixture(t, "subscription")
			if row.setup != nil {
				row.setup(f)
			}
			var before []byte
			path := filepath.Join(f.home, ".zshrc")
			info, err := os.Stat(path)
			readable := err == nil && info.Mode().IsRegular()
			if readable {
				before = f.read(path)
			}
			got := f.invoke(row.command...)
			requireStatus(t, got, row.status)
			if got.out != "" || got.err == "" {
				t.Fatal("shell refusal did not produce only an error")
			}
			if readable && !bytes.Equal(before, f.read(path)) {
				t.Fatal("shell refusal replaced user bytes")
			}
			f.refused()
		})
	}
}

func TestEditedIntegration(t *testing.T) {
	for _, command := range []string{"enable", "disable", "status"} {
		t.Run(command, func(t *testing.T) {
			f := newFixture(t, "subscription")
			requireStatus(t, f.invoke("shell", "enable"), 0)
			path := filepath.Join(f.home, ".config/claudock/init.zsh")
			edited := append(f.read(path), []byte("# user edit\n")...)
			f.write(path, edited, 0600)
			shellBefore := f.read(filepath.Join(f.home, ".zshrc"))
			got := f.invoke("shell", command)
			requireStatus(t, got, 1)
			if got.out != "" || got.err == "" || !bytes.Equal(edited, f.read(path)) ||
				!bytes.Equal(shellBefore, f.read(filepath.Join(f.home, ".zshrc"))) {
				t.Fatal("edited integration was not preserved on refusal")
			}
			f.refused()
		})
	}
}

func TestShellProfileNames(t *testing.T) {
	f := newFixture(t, "subscription")
	f.addProfile("two", "codex")
	f.addProfile("imported", "subscription")
	f.profiles[2].Managed = false
	got := f.invoke("shell", "profile-names")
	requireStatus(t, got, 0)
	if got.out != "claudock-profile-names-v1\nclaude-one\ncodex-two\n" || got.err != "" {
		t.Fatal("shell profile names do not follow managed registry entries")
	}
	f.refused()
}

func TestSwap(t *testing.T) {
	rows := []struct {
		name                                                        string
		two, three                                                  float64
		options                                                     []string
		chosen                                                      string
		status                                                      int
		httpStatus                                                  int
		modern, expiredReset, missingHistory, differentHistory, dry bool
	}{
		{name: "most-room", two: 60, three: 20, chosen: "three"},
		{name: "preference-breaks-tie", two: 20, three: 20, options: []string{"--prefer", "three"}, chosen: "three"},
		{name: "name-breaks-tie", two: 20, three: 20, chosen: "three"},
		{name: "excluded-profile", two: 40, three: 20, options: []string{"--exclude", "three"}, chosen: "two"},
		{name: "strict-threshold", two: 85, three: 86, status: 3},
		{name: "zero-threshold", two: 0, three: 0, options: []string{"--max-used", "0"}, status: 3},
		{name: "expired-window-reset", two: 95, three: 90, expiredReset: true, chosen: "three"},
		{name: "modern-limit-array", two: 90, three: 20, modern: true, chosen: "three"},
		{name: "unshared-history", two: 10, three: 20, missingHistory: true, status: 3},
		{name: "stale-history", two: 10, three: 20, differentHistory: true, status: 3},
		{name: "unauthorized-usage", two: 10, three: 20, httpStatus: 401, status: 1},
		{name: "rate-limited-usage", two: 10, three: 20, httpStatus: 429, status: 1},
		{name: "dry-run", two: 10, three: 20, chosen: "two", dry: true},
		{name: "continue-same-id", two: 10, three: 20, chosen: "two", options: []string{"--continue"}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			f := newFixture(t, "subscription")
			f.addProfile("two", "subscription")
			f.addProfile("three", "subscription")
			f.addProfile("billed", "apiKey")
			sourcePath := f.transcript(0, sessionID, "claude-sonnet-4-6", nil)
			if !row.missingHistory {
				for i := 1; i <= 2; i++ {
					model := "claude-sonnet-4-6"
					if row.differentHistory {
						model = "claude-opus-4-6"
					}
					f.transcript(i, sessionID, model, nil)
				}
			}
			before := f.read(sourcePath)
			var mu sync.Mutex
			requests := make(map[string]int)
			validRequests := true
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				name := ""
				for _, profile := range []string{"one", "two", "three"} {
					if r.Header.Get("Authorization") == "Bearer "+f.secrets[profile]["oauth"] {
						name = profile
					}
				}
				mu.Lock()
				requests[name]++
				validRequests = validRequests && name != "" && r.Method == http.MethodGet &&
					r.Header.Get("anthropic-beta") == "oauth-2025-04-20" && r.Header.Get("Accept") == "application/json"
				mu.Unlock()
				if name == "" {
					w.WriteHeader(401)
					return
				}
				if row.httpStatus != 0 {
					w.WriteHeader(row.httpStatus)
					return
				}
				used := row.two
				if name == "three" {
					used = row.three
				}
				reset := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
				if row.expiredReset {
					reset = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
				}
				var body any = map[string]any{"five_hour": map[string]any{"utilization": used, "resets_at": reset},
					"seven_day": map[string]any{"utilization": used / 2, "resets_at": reset}}
				if row.modern {
					body = map[string]any{"limits": []any{map[string]any{"kind": "session", "percent": used, "resets_at": reset}}}
				}
				w.Header().Set("Content-Type", "application/json")
				if json.NewEncoder(w).Encode(body) != nil {
					mu.Lock()
					validRequests = false
					mu.Unlock()
				}
			}))
			defer server.Close()
			f.env["CLAUDOCK_TEST_USAGE_ENDPOINT"] = server.URL
			if row.chosen != "" {
				f.env["PROBE_PROFILE"] = row.chosen
			}
			args := []string{"profile", "swap", "--from", "one"}
			continued := false
			for _, option := range row.options {
				if option == "--continue" {
					continued = true
				}
			}
			if !continued {
				args = append(args, "--session", sessionID)
			}
			args = append(args, row.options...)
			if row.dry {
				args = append(args, "--dry-run")
			}
			args = append(args, "--", "--output-format", "json")
			got := f.invoke(args...)
			requireStatus(t, got, row.status)
			mu.Lock()
			requestCopy := make(map[string]int)
			for name, count := range requests {
				requestCopy[name] = count
			}
			valid := validRequests
			mu.Unlock()
			if !valid || len(requestCopy) == 0 || requestCopy["one"] != 0 || requestCopy["billed"] != 0 {
				t.Fatal("swap did not request authenticated usage only for eligible subscriptions")
			}
			if row.name == "excluded-profile" && requestCopy["three"] != 0 {
				t.Fatal("swap requested an excluded profile")
			}
			if !bytes.Equal(before, f.read(sourcePath)) {
				t.Fatal("swap rewrote the source transcript")
			}
			if row.status != 0 {
				f.refused()
				if got.out != "" || got.err == "" {
					t.Fatal("swap failure did not produce only an error")
				}
			} else if row.dry {
				f.refused()
				var preview struct {
					Argv []string
					CWD  string
				}
				if json.Unmarshal([]byte(got.out), &preview) != nil || preview.CWD != f.cwd ||
					len(preview.Argv) < 8 || preview.Argv[2] != row.chosen || preview.Argv[6] != sessionID {
					t.Fatal("swap dry run did not preserve selected profile and session")
				}
			} else {
				f.launched("claude", []string{"-p", "--resume", sessionID,
					"Continue the same session from where it stopped.", "--output-format", "json"})
				if !strings.Contains(got.err, "on "+row.chosen+":") {
					t.Fatal("swap did not report the selected profile")
				}
			}
		})
	}
}

func TestSwapBadArguments(t *testing.T) {
	for i, options := range [][]string{
		{}, {"--from", "one"}, {"--session", sessionID},
		{"--from", "one", "--session", sessionID, "--continue"},
		{"--from", "one", "--session", "invalid"},
		{"--from", "one", "--session", sessionID, "--max-used", "NaN"},
		{"--from", "one", "--session", sessionID, "--max-used", "101"},
		{"--from", "one", "--session", sessionID, "--max-used"},
		{"--from", "one", "--session", sessionID, "--unknown"},
		{"--from", "one", "--session", sessionID, "--", "--resume", sessionID},
	} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			f := newFixture(t, "subscription")
			got := f.invoke(append([]string{"profile", "swap"}, options...)...)
			requireStatus(t, got, 2)
			if got.out != "" || got.err == "" {
				t.Fatal("bad swap arguments did not produce only an error")
			}
			f.refused()
		})
	}
}
