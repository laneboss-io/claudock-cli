package launch

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/laneboss-io/claudock-cli/internal/store"
)

func run(args []string, errOut io.Writer) error {
	if len(args) == 0 || !validName(args[0]) {
		return invalid("Use: claudock run NAME [--allow-cross-provider-resume] [-- CLAUDE_ARGS...].")
	}
	name, rest := args[0], args[1:]
	allow := len(rest) > 0 && rest[0] == "--allow-cross-provider-resume"
	if allow {
		rest = rest[1:]
	}
	if len(rest) > 0 {
		if rest[0] != "--" {
			return invalid("Separate vendor arguments with '--': claudock run NAME [--allow-cross-provider-resume] -- ARGS.")
		}
		rest = rest[1:]
	}
	p, err := store.Get(name)
	if err != nil {
		return fmt.Errorf("cannot select the requested profile")
	}
	return launchProfile(p, rest, allow, errOut)
}

func launchProfile(p store.Profile, args []string, allow bool, errOut io.Writer) (err error) {
	if !validName(p.Name) || !validPath(p.ConfigDirectory) {
		return fmt.Errorf("profile cannot be launched safely")
	}
	for _, arg := range args {
		if strings.IndexByte(arg, 0) >= 0 {
			return invalid("A vendor argument contains an invalid character.")
		}
	}
	if p.Kind != store.Subscription && p.Kind != store.APIKey && p.Kind != store.ConsoleLogin && p.Kind != store.Endpoint && p.Kind != store.Codex {
		return fmt.Errorf("%s: unsupported profile kind", p.Name)
	}
	env := isolatedEnvironment(os.Environ())
	program := "claude"
	var cleanup func() error
	if p.Kind == store.Codex {
		program = "codex"
		env["CODEX_HOME"] = p.ConfigDirectory
	} else {
		if err := validateConfig(p); err != nil {
			return err
		}
		if p.Command != "claude" {
			env["CLAUDE_CONFIG_DIR"] = p.ConfigDirectory
		}
		if p.Kind == store.Endpoint {
			cwd, cwdErr := os.Getwd()
			if cwdErr != nil {
				return fmt.Errorf("cannot resolve working directory")
			}
			var settings []byte
			args, settings, err = endpointArguments(p, args, cwd, env)
			if err != nil {
				return err
			}
			if !allow {
				if err := checkSession(p, args, cwd, env, errOut); err != nil {
					return err
				}
			}
			var settingsPath string
			settingsPath, cleanup, err = privateEndpointSettings(settings)
			if err != nil {
				return err
			}
			args = append([]string{"--settings", settingsPath}, args...)
			defer func() {
				if cleanupErr := cleanup(); cleanupErr != nil {
					if err == nil {
						err = cleanupErr
					} else {
						fmt.Fprintln(errOut, "claudock: could not remove private endpoint settings")
					}
				}
			}()
		}
		if err := credentialEnvironment(p, args, env); err != nil {
			return err
		}
	}
	executable, err := findExecutable(program, env)
	if err != nil {
		return err
	}
	if p.Kind == store.APIKey || p.Kind == store.ConsoleLogin {
		fmt.Fprintf(errOut, "claudock: %s: Console spend capture is not yet ported; this run will not update the credit estimate.\n", p.Name)
	}
	if cleanup != nil {
		return supervisedLaunch(executable, args, environmentList(env))
	}
	if err := syscall.Exec(executable, append([]string{executable}, args...), environmentList(env)); err != nil {
		return fmt.Errorf("could not launch %s", program)
	}
	return nil
}

func findExecutable(program string, env map[string]string) (string, error) {
	var candidates []string
	if program == "claude" {
		prefs, err := preferences()
		if err != nil {
			return "", err
		}
		if custom, ok := prefs["claudeExecutable"].(string); ok && custom != "" {
			candidates = append(candidates, custom)
		}
	}
	// Explicit PATH entries take priority over conventional install locations.
	// Relative/empty entries are never searched (including the current directory).
	for _, directory := range filepath.SplitList(env["PATH"]) {
		if filepath.IsAbs(directory) {
			candidates = append(candidates, filepath.Join(directory, program))
		}
	}
	if home := env["HOME"]; validPath(home) {
		candidates = append(candidates, home+"/.local/bin/"+program, home+"/.npm-global/bin/"+program, home+"/.bun/bin/"+program)
	}
	candidates = append(candidates, "/opt/homebrew/bin/"+program, "/usr/local/bin/"+program)
	self, selfErr := os.Executable()
	selfInfo, _ := os.Stat(self)
	for _, path := range candidates {
		info, err := os.Stat(path)
		if err == nil && validPath(path) && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			if selfErr == nil && selfInfo != nil && os.SameFile(selfInfo, info) {
				continue
			}
			return path, nil
		}
	}
	return "", fmt.Errorf("%s executable was not found", program)
}
