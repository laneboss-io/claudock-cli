package profilecmd

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/laneboss-io/claudock-cli/internal/store"
)

func loginEnvironment(p store.Profile) []string {
	var environment []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "ANTHROPIC_") || strings.HasPrefix(key, "CLAUDE_CODE_USE_") || strings.HasPrefix(key, "CLAUDE_CODE_OAUTH") || strings.HasPrefix(key, "CLAUDE_CODE_DEFAULT_") || strings.HasSuffix(key, "_FILE_DESCRIPTOR") || key == "CLAUDE_CONFIG_DIR" || key == "CLAUDE_SECURESTORAGE_CONFIG_DIR" || key == "CLAUDE_CODE_SUBAGENT_MODEL" || key == "CLAUDE_BG_AUTH_SNAPSHOT_PATH" || key == "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST" || strings.HasPrefix(key, "CODEX_") || strings.HasPrefix(key, "OPENAI_") {
			continue
		}
		environment = append(environment, entry)
	}
	if p.Kind == store.Codex {
		environment = append(environment, "CODEX_HOME="+p.ConfigDirectory)
	} else if p.Command != "claude" {
		environment = append(environment, "CLAUDE_CONFIG_DIR="+p.ConfigDirectory)
	}
	return environment
}

func launchLogin(p store.Profile, console, setup bool, in io.Reader, out, stderr io.Writer) (int, error) {
	if p.IsVertex || p.DiscoveryNote != "" || !absolutePath(p.ConfigDirectory) {
		return 1, errors.New("resolve the profile's config directory before signing in")
	}
	if setup && p.Kind != store.Subscription {
		return 1, errors.New("inference token setup belongs only to subscription profiles")
	}
	program := "claude"
	arguments := []string{"auth", "login", "--claudeai"}
	switch p.Kind {
	case store.Subscription:
		if console {
			return 1, errors.New("subscription profiles cannot sign in to a Console account")
		}
	case store.ConsoleLogin:
		arguments = []string{"auth", "login", "--console"}
	case store.APIKey:
		if !console {
			return 1, errors.New("API-key profiles need profile login NAME --console to switch to a Console sign-in")
		}
		arguments = []string{"auth", "login", "--console"}
	case store.Endpoint:
		return 1, errors.New("endpoint profiles use their provider key and have no Claude sign-in")
	case store.Codex:
		if console {
			return 1, errors.New("Codex profiles do not have an Anthropic Console sign-in")
		}
		program = "codex"
		arguments = []string{"login"}
	}
	if setup {
		arguments = []string{"setup-token"}
	}
	executable, err := exec.LookPath(program)
	if err != nil {
		return 1, errors.New("required CLI executable was not found in PATH")
	}
	current, err := store.Get(p.Command)
	if err != nil || !current.Equal(p) {
		return 1, errors.New("profile changed before sign-in; select it again")
	}
	cmd := exec.Command(executable, arguments...)
	cmd.Env = loginEnvironment(p)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = in, out, stderr
	err = cmd.Run()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				return 128 + int(status.Signal()), nil
			}
			return exit.ExitCode(), nil
		}
		return 1, errors.New("could not start the profile's CLI")
	}
	if p.Kind == store.APIKey && console {
		signedIn, err := store.ConsoleSignedIn(p)
		if err != nil {
			return 1, err
		}
		if !signedIn {
			return 1, errors.New("CLI exited without saving a Console sign-in; the profile still uses its pasted API key")
		}
		if _, err := store.SetConsoleKind(p, store.ConsoleLogin, ""); err != nil {
			return 1, err
		}
		service, _ := store.Service(p, store.APIKeyItem)
		_, err = io.WriteString(out, p.Name+" now uses its Console sign-in. The pasted API key was kept under service "+service+".\n")
		if err != nil {
			return 1, errors.New("could not report Console sign-in completion")
		}
	}
	return 0, nil
}
