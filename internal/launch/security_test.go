package launch_test

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func securitySource(t *testing.T, path string) []byte {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal("cannot locate the regression source")
	}
	if os.Getenv("CLAUDOCK_SECURITY_BASELINE") == "1" {
		command := exec.Command("git", "show", "f39e347:"+path)
		command.Dir = root
		data, err := command.Output()
		if err != nil {
			t.Fatal("cannot read the actual regression baseline source")
		}
		return data
	}
	data, err := os.ReadFile(filepath.Join(root, path))
	if err != nil {
		t.Fatal("cannot read the regression source")
	}
	return data
}

func TestSecurityNormalSwapIgnoresTestEndpoint(t *testing.T) {
	f := newFixture(t, "subscription")
	f.binary = normalCLIBinary
	f.addProfile("two", "subscription")
	f.transcript(0, sessionID, "claude-sonnet-4-6", nil)
	f.transcript(1, sessionID, "claude-sonnet-4-6", nil)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer "+f.secrets["two"]["oauth"] {
			t.Error("swap did not use the temporary destination credential")
		}
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"five_hour":{"utilization":0}}`)); err != nil {
			t.Error("could not serve temporary swap quota")
		}
	}))
	defer server.Close()
	f.env["CLAUDOCK_TEST_USAGE_ENDPOINT"] = server.URL
	r := f.invoke("profile", "swap", "--from", "one", "--session", sessionID, "--dry-run")
	requireStatus(t, r, 1)
	if requests.Load() != 0 || r.out != "" || !strings.Contains(r.err, "claudock_test build") {
		t.Fatal("a normal build honored the stale test endpoint or failed for an unrelated reason")
	}
	f.refused()
}

func TestSecurityEndpointPrivateSettings(t *testing.T) {
	for _, row := range []struct {
		name, signal string
		status       int
	}{{"success", "", 0}, {"child-status", "", 23}, {"child-signal", "TERM", 143}} {
		t.Run(row.name, func(t *testing.T) {
			f := newFixture(t, "endpoint")
			canary := f.randomSecret("settings-canary-")
			source := filepath.Join(f.cwd, "source-settings.json")
			f.writeJSON(source, map[string]any{"theme": "dark", "env": map[string]string{"SERVICE_ACCESS_TOKEN": canary}})
			f.env["PROBE_SETTINGS_SOURCE"] = source
			if row.status == 23 {
				f.env["PROBE_STATUS"] = "23"
			}
			if row.signal != "" {
				f.env["PROBE_SIGNAL"] = row.signal
			}
			r := f.invoke("run", "one", "--", "--settings", source, "-p", "hello")
			requireStatus(t, r, row.status)
			var record probeRecord
			if json.Unmarshal(f.read(f.record), &record) != nil || len(record.Args) != 4 || record.Args[0] != "--settings" {
				t.Fatal("endpoint did not preserve its settings option and vendor arguments")
			}
			f.launched("claude", []string{"--settings", record.Args[1], "-p", "hello"})
			for _, key := range []string{"settings", "settings-file", "settings-canary", "argv-clean"} {
				if !record.Checks[key] {
					t.Fatalf("private settings comparison failed: %s", key)
				}
			}
			for _, arg := range record.Args {
				if strings.Contains(arg, canary) || strings.Contains(arg, "SERVICE_ACCESS_TOKEN") || strings.Contains(arg, "availableModels") {
					t.Fatal("settings content appeared in child argv")
				}
			}
			if !filepath.IsAbs(record.Args[1]) || record.Args[1] == source {
				t.Fatal("endpoint did not pass the private merged settings path")
			}
			for _, path := range []string{record.Args[1], filepath.Dir(record.Args[1])} {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatal("endpoint settings survived the child's exit")
				}
			}
			if r.out != "probe stdout\n" || !strings.Contains(r.err, "probe stderr\n") {
				t.Fatal("supervised endpoint did not preserve vendor output")
			}
		})
	}
}

func TestSecurityRunEnvironment(t *testing.T) {
	for _, kind := range []string{"subscription", "apiKey", "consoleLogin", "endpoint", "codex"} {
		t.Run(kind, func(t *testing.T) {
			f := newFixture(t, kind)
			for _, name := range []string{"OPENAI_SESSION_TOKEN", "OPENAI_PROJECT_ID", "CODEX_ACCESS_TOKEN", "CODEX_CONFIG", "CLAUDE_CODE_USE_GATEWAY"} {
				f.env[name] = f.randomSecret("inherited-canary-")
			}
			requireStatus(t, f.invoke("run", "one", "--", "-p", "hello"), 0)
			var record probeRecord
			if json.Unmarshal(f.read(f.record), &record) != nil {
				t.Fatal("vendor did not record environment isolation")
			}
			program := "claude"
			if kind == "codex" {
				program = "codex"
			}
			args := []string{"-p", "hello"}
			if kind == "endpoint" {
				if len(record.Args) < 2 {
					t.Fatal("endpoint did not supply its settings option")
				}
				args = append(record.Args[:2], args...)
			}
			f.launched(program, args)
			for _, name := range record.Names {
				if strings.HasPrefix(name, "OPENAI_") || (strings.HasPrefix(name, "CODEX_") && !(kind == "codex" && name == "CODEX_HOME")) || name == "CLAUDE_CODE_USE_GATEWAY" {
					t.Fatal("an inherited credential prefix or gateway flag reached the vendor")
				}
			}
		})
	}
}

func TestSecurityRequiredPolicyShared(t *testing.T) {
	f := newFixture(t, "subscription")
	path := filepath.Join(f.home, "Library/Preferences/io.github.claudeusage.ClaudeUsage.plist")
	f.write(path, []byte("<plist><dict><key>requireInferenceToken</key><integer>1</integer></dict></plist>"), 0600)
	status := f.invoke("require-token", "status")
	requireStatus(t, status, 0)
	if status.out != "on\n" || status.err != "" {
		t.Fatal("integer requirement was not read as on")
	}
	r := f.invoke("run", "one", "--", "-p", "hello")
	requireStatus(t, r, 1)
	if !strings.Contains(r.err, "inferenceToken credential is required") || r.out != "" {
		t.Fatal("run did not enforce the policy reported by status")
	}
	f.refused()
}

func TestSecurityRequiredPolicyFailsClosed(t *testing.T) {
	for index, data := range []string{
		"not a plist",
		"<plist><dict><key>requireInferenceToken</key><array/></dict></plist>",
		"<plist><dict><key>requireInferenceToken</key><true/><key>requireInferenceToken</key><false/></dict></plist>",
		"<plist><dict><key>requireInferenceToken</key><integer>99999999999999999999999</integer></dict></plist>",
	} {
		t.Run(fmt.Sprintf("invalid-policy-%d", index), func(t *testing.T) {
			f := newFixture(t, "subscription")
			path := filepath.Join(f.home, "Library/Preferences/io.github.claudeusage.ClaudeUsage.plist")
			f.write(path, []byte(data), 0600)
			requireStatus(t, f.invoke("require-token", "status"), 1)
			r := f.invoke("run", "one", "--", "-p", "hello")
			requireStatus(t, r, 1)
			if r.out != "" || !strings.Contains(r.err, "could not read the inference token requirement") {
				t.Fatal("run bypassed an unreadable shared policy")
			}
			f.refused()
		})
	}
}

func TestSecurityEndpointForwardsParentSignal(t *testing.T) {
	f := newFixture(t, "endpoint")
	f.env["PROBE_WAIT_SIGNAL"] = "1"
	parentPID := 0
	f.afterStart = func(command *exec.Cmd) error {
		parentPID = command.Process.Pid
		deadline := time.NewTimer(3 * time.Second)
		defer deadline.Stop()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-deadline.C:
				return fmt.Errorf("vendor did not become ready")
			case <-ticker.C:
				data, err := os.ReadFile(f.record)
				if os.IsNotExist(err) {
					continue
				}
				var record probeRecord
				if err != nil || json.Unmarshal(data, &record) != nil {
					return fmt.Errorf("vendor did not publish a valid ready record")
				}
				return command.Process.Signal(syscall.SIGTERM)
			}
		}
	}
	requireStatus(t, f.invoke("run", "one", "--", "-p", "hello"), 0)
	var record probeRecord
	if json.Unmarshal(f.read(f.record), &record) != nil || len(record.Args) != 4 || record.PID == parentPID || !record.Checks["signal-forwarded"] || !record.Checks["settings-file"] {
		t.Fatal("endpoint did not retain cleanup supervision and forward the parent signal")
	}
	f.launched("claude", []string{"--settings", record.Args[1], "-p", "hello"})
	if _, err := os.Lstat(filepath.Dir(record.Args[1])); !os.IsNotExist(err) {
		t.Fatal("forwarded termination left the private settings directory")
	}
}

func TestSecurityPublicDocumentation(t *testing.T) {
	processWords := regexp.MustCompile(`(?i)\b(executor|supervisor|owner|brief|lane)s?\b`)
	for _, path := range []string{"README.md", "docs/profile-store.md", "docs/profile-tests.md"} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			data := securitySource(t, path)
			if len(strings.TrimSpace(string(data))) == 0 {
				t.Fatal("public documentation is empty")
			}
			if processWords.Match(data) {
				t.Fatal("public documentation contains internal process wording")
			}
			for _, stale := range []string{"not built", "not tested", "No build or test was executed", "has not built or run", "verification still required", "still require Linux verification"} {
				if strings.Contains(string(data), stale) {
					t.Fatal("public documentation contains a stale verification claim")
				}
			}
		})
	}
}

func TestSecurityWorkspaceCatalog(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "workspace.go", securitySource(t, "internal/store/workspace.go"), 0)
	if err != nil {
		t.Fatal("cannot parse the workspace sharing catalog")
	}
	checked := 0
	ast.Inspect(file, func(node ast.Node) bool {
		value, ok := node.(*ast.ValueSpec)
		if !ok || len(value.Names) != 1 || value.Names[0].Name != "commonWorkspaceNames" {
			return true
		}
		ast.Inspect(value, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if ok && literal.Kind == token.STRING {
				name, err := strconv.Unquote(literal.Value)
				if err != nil || strings.HasPrefix(name, ".") {
					t.Error("automatic workspace sharing includes hidden extension state")
				}
				checked++
			}
			return true
		})
		return false
	})
	if checked == 0 {
		t.Fatal("workspace sharing catalog was not inspected")
	}
}
