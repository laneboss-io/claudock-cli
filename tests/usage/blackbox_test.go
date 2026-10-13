package usage_test

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
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/laneboss-io/claudock-cli/internal/testutil"
	"golang.org/x/text/unicode/norm"
)

var binaryPath string

const usageHeader = "PROFILE\tPLAN\tWINDOW\tUSED_PERCENT\tRESETS_UTC\n"
const availableHeader = "PROFILE\tKIND\tPLAN\tLEFT\tTIGHTEST_LIMIT\tRESETS_UTC\n"
const preferenceDomain = "io.github.claudeusage.ClaudeUsage"
const defaultNotice = "claudock: default: skipped; this profile does not support subscription usage.\n"

func TestMain(m *testing.M) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		fmt.Fprintln(os.Stderr, "could not locate black-box sources")
		os.Exit(1)
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(source), "../.."))
	buildDirectory, err := os.MkdirTemp("", "cc-go-usage-build-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not create black-box build directory")
		os.Exit(1)
	}
	binaryPath = filepath.Join(buildDirectory, "claudock")
	replacements := make(map[string]string)
	if os.Getenv("CLAUDOCK_USAGE_BASELINE") == "1" {
		replacements[filepath.Join(root, "cmd/claudock/main.go")] = filepath.Join(root, "tests/usage/testdata/baseline-main.go.txt")
	}
	overlay, err := json.Marshal(map[string]any{"Replace": replacements})
	if err == nil {
		err = os.WriteFile(filepath.Join(buildDirectory, "overlay.json"), overlay, 0600)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "could not write black-box build overlay")
		os.Exit(1)
	}
	build := exec.Command("go", "build", "-tags", "claudock_test", "-overlay", filepath.Join(buildDirectory, "overlay.json"), "-o", binaryPath, "./cmd/claudock")
	build.Dir = root
	build.Env = append(os.Environ(), "CGO_ENABLED=0", "GOTOOLCHAIN=local")
	if output, err := build.CombinedOutput(); err != nil {
		fmt.Fprintln(os.Stderr, "black-box binary build failed:")
		os.Stderr.Write(output) // Compiler output contains source names, never runtime credentials.
		os.Exit(1)
	}
	code := m.Run()
	if err := os.RemoveAll(buildDirectory); err != nil && code == 0 {
		fmt.Fprintln(os.Stderr, "could not collect black-box build directory")
		code = 1
	}
	os.Exit(code)
}

type testProfile struct {
	Command         string         `json:"command"`
	ConfigDirectory string         `json:"configDirectory"`
	RegistryID      string         `json:"registryID,omitempty"`
	Managed         bool           `json:"managed"`
	Kind            string         `json:"authKind,omitempty"`
	IsVertex        bool           `json:"isVertex"`
	DiscoveryNote   *string        `json:"discoveryNote,omitempty"`
	Endpoint        map[string]any `json:"endpoint,omitempty"`
}

func (p testProfile) name() string {
	if p.Command == "claude" {
		return "default"
	}
	return strings.TrimPrefix(strings.TrimPrefix(p.Command, "claude-"), "codex-")
}

func (p testProfile) service() string {
	if p.Command == "claude" {
		return "Claude Code-credentials"
	}
	digest := sha256.Sum256([]byte(norm.NFC.String(p.ConfigDirectory)))
	return fmt.Sprintf("Claude Code-credentials-%x", digest[:4])
}

type apiReply struct {
	status int
	body   string
	retry  string
	block  <-chan struct{}
}

type requestRecord struct {
	started  time.Time
	finished time.Time
}

type fixture struct {
	t          *testing.T
	cliResults *testutil.CLIResults
	home       string
	base       string
	store      string
	server     *httptest.Server
	profiles   []testProfile
	secrets    map[string]string
	tokens     map[string]string
	mu         sync.Mutex
	replies    map[string]apiReply
	requests   []requestRecord
	entered    chan struct{}
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	home := t.TempDir()
	// publish creates the vault with mode 0700; t.TempDir's leaf follows the umask.
	f := &fixture{t: t, cliResults: testutil.CaptureCLI(t), home: home, base: filepath.Join(home, "Library/Application Support/Claudock"), store: filepath.Join(home, "vault", "test-credentials.json"), secrets: make(map[string]string), tokens: make(map[string]string), replies: make(map[string]apiReply), entered: make(chan struct{}, 32)}
	note := "fixture default profile is unresolved"
	f.profiles = []testProfile{{Command: "claude", RegistryID: "00000000-0000-4000-8000-000000000001", ConfigDirectory: "", DiscoveryNote: &note}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/oauth/usage" || r.Header.Get("anthropic-beta") != "oauth-2025-04-20" || r.Header.Get("Accept") != "application/json" || r.Header.Get("User-Agent") != "Claudock/1.7.0" {
			t.Error("remote request method, path, or protocol headers did not match the usage API")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		index := len(f.requests)
		f.requests = append(f.requests, requestRecord{started: time.Now()})
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		name := ""
		for profileName, candidate := range f.tokens {
			if token == candidate && r.Header.Get("Authorization") == "Bearer "+candidate {
				name = profileName
				break
			}
		}
		reply, exists := f.replies[name]
		f.mu.Unlock()
		if name == "" || !exists {
			t.Error("request did not authenticate with a configured temporary credential item")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.entered <- struct{}{}
		if reply.block != nil {
			select {
			case <-reply.block:
			case <-r.Context().Done():
				return
			}
		}
		if reply.retry != "" {
			w.Header().Set("Retry-After", reply.retry)
		}
		if reply.status >= 300 && reply.status < 400 {
			w.Header().Set("Location", f.server.URL+"/redirect-must-not-be-followed")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(reply.status)
		_, err := w.Write([]byte(reply.body))
		if err != nil {
			t.Error("localhost response could not be written")
		}
		f.mu.Lock()
		f.requests[index].finished = time.Now()
		f.mu.Unlock()
	}))
	t.Cleanup(f.server.Close)
	f.publish()
	return f
}

func marshal(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal("could not encode temporary fixture")
	}
	return data
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal("could not create temporary fixture directory")
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal("could not write temporary fixture file")
	}
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("could not read temporary fixture file")
	}
	return data
}

func (f *fixture) publish() {
	f.t.Helper()
	version := 1
	mainProfiles, codexProfiles := []testProfile{}, []testProfile{}
	for _, profile := range f.profiles {
		if profile.Kind == "codex" {
			codexProfiles = append(codexProfiles, profile)
			continue
		}
		mainProfiles = append(mainProfiles, profile)
		if profile.Kind == "endpoint" {
			version = 3
		} else if (profile.Kind == "apiKey" || profile.Kind == "consoleLogin") && version < 2 {
			version = 2
		}
	}
	writeFile(f.t, filepath.Join(f.base, "profiles.json"), marshal(f.t, map[string]any{"version": version, "profiles": mainProfiles, "suppressedCommands": []string{}, "suppressedDirectories": []string{}, "subscriptionOnlyMigration": 1}))
	writeFile(f.t, filepath.Join(f.base, "codex-profiles.json"), marshal(f.t, map[string]any{"version": 1, "profiles": codexProfiles, "suppressedCommands": []string{}, "suppressedDirectories": []string{}, "subscriptionOnlyMigration": 1}))
	writeFile(f.t, f.store, marshal(f.t, map[string]any{"version": 1, "items": f.secrets}))
}

func (f *fixture) add(name, kind, plan, tier string) testProfile {
	f.t.Helper()
	command := "claude-" + name
	if name == "default" {
		command = "claude"
	} else if kind == "codex" {
		command = "codex-" + name
	}
	tokenBytes := make([]byte, 24)
	if _, err := rand.Read(tokenBytes); err != nil {
		f.t.Fatal("could not create temporary credential canary")
	}
	uuid := make([]byte, 16)
	if _, err := rand.Read(uuid); err != nil {
		f.t.Fatal("could not create a temporary profile ID")
	}
	uuid[6], uuid[8] = uuid[6]&0x0f|0x40, uuid[8]&0x3f|0x80
	p := testProfile{Command: command, ConfigDirectory: filepath.Join(f.home, "accounts", name), RegistryID: fmt.Sprintf("%x-%x-%x-%x-%x", uuid[:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:]), Managed: name != "default", Kind: kind}
	if kind == "endpoint" {
		p.Endpoint = map[string]any{"baseURL": "https://example.invalid/v1", "model": "provider-model"}
	}
	if name == "default" {
		f.profiles[0] = p
	} else {
		f.profiles = append(f.profiles, p)
	}
	token := hex.EncodeToString(tokenBytes)
	f.cliResults.Redact(token, "unused-rotation-canary")
	f.mu.Lock()
	f.tokens[name] = token
	f.replies[name] = apiReply{status: 200, body: legacyBody(f.t, 25, time.Now().Add(time.Hour))}
	f.mu.Unlock()
	f.secrets[p.service()] = string(marshal(f.t, map[string]any{"claudeAiOauth": map[string]any{"accessToken": token, "refreshToken": "unused-rotation-canary", "expiresAt": time.Now().Add(time.Hour).UnixMilli(), "subscriptionType": plan, "rateLimitTier": tier}}))
	f.publish()
	return p
}

func legacyBody(t *testing.T, used float64, reset time.Time) string {
	return string(marshal(t, map[string]any{"five_hour": map[string]any{"utilization": used, "resets_at": reset.UTC().Format(time.RFC3339)}}))
}

func (f *fixture) reply(name string, reply apiReply) {
	f.mu.Lock()
	f.replies[name] = reply
	f.mu.Unlock()
}

func (f *fixture) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

type result struct {
	code           int
	out            string
	err            string
	expectedNotice string
}

func (f *fixture) command(ctx context.Context, arguments ...string) (*exec.Cmd, *bytes.Buffer, *bytes.Buffer) {
	command := exec.CommandContext(ctx, binaryPath, arguments...)
	command.Dir = f.home
	command.Env = []string{"HOME=" + f.home, "PATH=" + os.Getenv("PATH"), "TMPDIR=" + f.home, "CLAUDOCK_CREDENTIAL_STORE=file:" + f.store, "CLAUDOCK_TEST_API_BASE_URL=" + f.server.URL}
	var out, diagnostics bytes.Buffer
	command.Stdout, command.Stderr = &out, &diagnostics
	// A failed API-arrival assertion can happen before collect is called.
	// The caller cancels its context before cleanup, so Wait also reaps that child.
	f.t.Cleanup(func() {
		if command.ProcessState != nil {
			return
		}
		if command.Process == nil {
			f.cliResults.Record(-1, out.String(), diagnostics.String())
			return
		}
		f.collect(command, &out, &diagnostics)
	})
	return command, &out, &diagnostics
}

func (f *fixture) collect(command *exec.Cmd, out, diagnostics *bytes.Buffer) result {
	f.t.Helper()
	err := command.Wait()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			f.cliResults.Record(-1, out.String(), diagnostics.String())
			f.t.Fatal("black-box command could not finish")
		}
		code = exit.ExitCode()
	}
	r := result{code: code, out: out.String(), err: diagnostics.String()}
	f.cliResults.Record(r.code, r.out, r.err)
	return r
}

func (f *fixture) run(arguments ...string) result {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	command, out, diagnostics := f.command(ctx, arguments...)
	if err := command.Start(); err != nil {
		f.t.Fatal("black-box command could not start")
	}
	r := f.collect(command, out, diagnostics)
	if len(arguments) > 0 && arguments[0] == "usage" && f.profiles[0].DiscoveryNote != nil {
		r.expectedNotice = defaultNotice
	}
	f.assertNoSecrets(r)
	return r
}

func (f *fixture) assertNoSecrets(r result) {
	f.t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, token := range f.tokens {
		f.cliResults.Redact(token)
		if strings.Contains(r.out, token) || strings.Contains(r.err, token) {
			f.t.Fatal("credential canary escaped into command output")
		}
	}
	for _, envelope := range f.secrets {
		f.cliResults.Redact(envelope)
		if envelope != "" && (strings.Contains(r.out, envelope) || strings.Contains(r.err, envelope)) {
			f.t.Fatal("credential item escaped into command output")
		}
		var credential struct {
			OAuth struct {
				Access  string `json:"accessToken"`
				Refresh string `json:"refreshToken"`
			} `json:"claudeAiOauth"`
		}
		// Some refusal rows intentionally supply malformed envelopes. The
		// entire item is checked above; valid envelopes also check each token.
		if json.Unmarshal([]byte(envelope), &credential) == nil {
			f.cliResults.Redact(credential.OAuth.Access, credential.OAuth.Refresh)
			for _, token := range []string{credential.OAuth.Access, credential.OAuth.Refresh} {
				if token != "" && (strings.Contains(r.out, token) || strings.Contains(r.err, token)) {
					f.t.Fatal("credential component escaped into command output")
				}
			}
		}
	}
	if strings.Contains(r.out, "unused-rotation-canary") || strings.Contains(r.err, "unused-rotation-canary") {
		f.t.Fatal("refresh credential canary escaped into command output")
	}
}

func expect(t *testing.T, got result, code int, stdout string, stderrPart string) {
	t.Helper()
	if got.code != code {
		t.Errorf("exit code = %d, want %d", got.code, code)
	}
	if got.out != stdout {
		t.Error("stdout did not match the expected table or names")
	}
	if stderrPart == "" && got.err != got.expectedNotice {
		t.Error("command emitted unexpected diagnostics")
	} else if stderrPart != "" && !strings.Contains(got.err, stderrPart) {
		t.Error("stderr did not contain the expected refusal or cache reason")
	}
}

func cachedWindow(id, title string, used float64, reset *time.Time) map[string]any {
	w := map[string]any{"id": id, "title": title, "percent": used}
	if reset != nil {
		w["resetsAt"] = reset.UTC().Format(time.RFC3339Nano)
	}
	return w
}

func (f *fixture) cache(entries map[string]any) {
	writeFile(f.t, filepath.Join(f.base, "usage-cache.json"), marshal(f.t, map[string]any{"version": 1, "profiles": entries}))
}

func cacheEntry(p testProfile, fetched time.Time, windows ...map[string]any) map[string]any {
	return map[string]any{"reading": map[string]any{"plan": "pro", "fetchedAt": fetched.UTC().Format(time.RFC3339Nano), "windows": windows, "extraUsageEnabled": false}}
}

func TestVersion(t *testing.T) {
	for _, command := range []string{"version", "--version"} {
		t.Run(command, func(t *testing.T) {
			f := newFixture(t)
			writeFile(t, filepath.Join(f.base, "profiles.json"), []byte("invalid registry"))
			before := readFile(t, f.store)
			expect(t, f.run(command), 0, "Claudock 1.7.0\n", "")
			if !bytes.Equal(before, readFile(t, f.store)) || f.requestCount() != 0 {
				t.Error("version touched credentials or requested an API")
			}
		})
	}
}
