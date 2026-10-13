package launch

import (
	"bytes"
	"crypto/rand"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/laneboss-io/claudock-cli/internal/store"
)

const shellStart = "# >>> Claudock shell integration >>>"
const shellEnd = "# <<< Claudock shell integration <<<"
const shellBlock = shellStart + "\n[[ -r \"$HOME/.config/claudock/init.zsh\" ]] && source \"$HOME/.config/claudock/init.zsh\"\n" + shellEnd + "\n"

//go:embed shell_v4.zsh.tmpl
var shellV4 string

//go:embed shell_v4_legacy.zsh.tmpl
var shellV4Legacy string

//go:embed shell_v1.zsh.tmpl
var shellV1 string

//go:embed shell_v2.zsh.tmpl
var shellV2 string

//go:embed shell_v3.zsh.tmpl
var shellV3 string

type shellState struct {
	Version int    `json:"version"`
	CLIPath string `json:"cliPath"`
}

type snapshot struct {
	path string
	data []byte
	info os.FileInfo
}

func takeSnapshot(path string) (snapshot, error) {
	s := snapshot{path: path}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return s, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Nlink != 1 {
		return s, fmt.Errorf("shell integration files must be ordinary files without links")
	}
	s.info = info
	s.data, err = readRegular(path, 2<<20, true)
	if err != nil {
		return s, fmt.Errorf("cannot read shell integration files")
	}
	return s, nil
}

func (s snapshot) unchanged() bool {
	current, err := takeSnapshot(s.path)
	if err != nil || !bytes.Equal(current.data, s.data) || (current.info == nil) != (s.info == nil) {
		return false
	}
	return s.info == nil || (os.SameFile(s.info, current.info) && s.info.Mode() == current.info.Mode() && s.info.ModTime().Equal(current.info.ModTime()))
}

type installation struct {
	state *shellState
	shell snapshot
	init  snapshot
	meta  snapshot
}

func shellPaths() (string, string, error) {
	home := os.Getenv("HOME")
	if !validPath(home) {
		return "", "", fmt.Errorf("HOME must be an absolute directory")
	}
	real, err := filepath.EvalSymlinks(home)
	if err != nil {
		return "", "", fmt.Errorf("cannot resolve home directory")
	}
	base := filepath.Join(real, ".config/claudock")
	if err := validateShellDirectories(real); err != nil {
		return "", "", err
	}
	return real, base, nil
}

func validateShellDirectories(home string) error {
	for _, path := range []string{home, filepath.Join(home, ".config"), filepath.Join(home, ".config/claudock")} {
		info, err := os.Lstat(path)
		if os.IsNotExist(err) && path != home {
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("shell integration folders must be ordinary directories")
		}
	}
	return nil
}

func readInstallation(home, base string) (installation, error) {
	var result installation
	var err error
	result.shell, err = takeSnapshot(filepath.Join(home, ".zshrc"))
	if err != nil {
		return result, err
	}
	result.init, err = takeSnapshot(filepath.Join(base, "init.zsh"))
	if err != nil {
		return result, err
	}
	result.meta, err = takeSnapshot(filepath.Join(base, "integration.json"))
	if err != nil {
		return result, err
	}
	if !utf8.Valid(result.shell.data) {
		return result, fmt.Errorf(".zshrc must be readable UTF-8")
	}
	text := string(result.shell.data)
	starts, ends := strings.Count(text, shellStart), strings.Count(text, shellEnd)
	if starts == 0 && ends == 0 && result.init.info == nil && result.meta.info == nil {
		return result, nil
	}
	start := strings.Index(text, shellBlock)
	var saved shellState
	if starts != 1 || ends != 1 || start < 0 || (start > 0 && text[start-1] != '\n') ||
		json.Unmarshal(result.meta.data, &saved) != nil || !validPath(saved.CLIPath) ||
		saved.Version < 1 || saved.Version > 4 || !ownedShell(result.init.data, saved) {
		return result, fmt.Errorf("shell integration was edited or is incomplete; restore its managed block and files")
	}
	result.state = &saved
	return result, nil
}

func renderShell(state shellState) string {
	template := shellV4
	switch state.Version {
	case 1:
		template = shellV1
	case 2:
		template = shellV2
	case 3:
		template = shellV3
	}
	return renderShellTemplate(template, state)
}

func ownedShell(data []byte, state shellState) bool {
	return bytes.Equal(data, []byte(renderShell(state))) ||
		(state.Version == 4 && bytes.Equal(data, []byte(renderShellTemplate(shellV4Legacy, state))))
}

func renderShellTemplate(template string, state shellState) string {
	return strings.NewReplacer("{{CLI}}", quote(state.CLIPath),
		"{{PREFIX}}", quote("command "+quote(state.CLIPath)+" run "),
		"{{AUTO}}", quote("command "+quote(state.CLIPath)+" auto -- \"$@\"")).Replace(template)
}

var zdotdirAssignment = regexp.MustCompile("(^|[;[:space:]])ZDOTDIR[[:space:]]*=")
var zdotdirDeclaration = regexp.MustCompile("(^|[;[:space:]])(export|typeset|readonly)[[:space:]]+(-[A-Za-z]+[[:space:]]+)*ZDOTDIR([;[:space:]=]|$)")

func checkZdotdir(home string) error {
	failure := fmt.Errorf("ZDOTDIR changes the shell startup folder; manage shell integration in your own dotfiles")
	if dir := os.Getenv("ZDOTDIR"); dir != "" {
		real, err := filepath.EvalSymlinks(dir)
		if err != nil || real != home {
			return failure
		}
	}
	data, err := optionalFile(filepath.Join(home, ".zshenv"), 2<<20, false)
	if err != nil || !utf8.Valid(data) {
		return failure
	}
	var code strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		var single, double, escaped bool
		for _, r := range line {
			if escaped {
				code.WriteRune(r)
				escaped = false
				continue
			}
			if r == '\\' && !single {
				escaped = true
			} else if r == '\'' && !double {
				single = !single
			} else if r == '"' && !single {
				double = !double
			} else if r == '#' && !single && !double {
				break
			}
			code.WriteRune(r)
		}
		code.WriteByte('\n')
	}
	if zdotdirAssignment.MatchString(code.String()) || zdotdirDeclaration.MatchString(code.String()) {
		return failure
	}
	return nil
}

func shellCommand(args []string, out io.Writer) error {
	if len(args) != 1 {
		return invalid("Use: claudock shell enable|disable|status|profile-names.")
	}
	if args[0] == "profile-names" {
		profiles, err := store.List()
		if err != nil {
			return fmt.Errorf("cannot read profile names")
		}
		names := make([]string, 0, len(profiles))
		seen := make(map[string]bool)
		for _, p := range profiles {
			if !p.Managed || p.Command == "claude" {
				continue
			}
			prefix := "claude-"
			if p.Kind == store.Codex {
				prefix = "codex-"
			}
			suffix := strings.TrimPrefix(p.Command, prefix)
			if suffix == p.Command || len(suffix) < 1 || len(suffix) > 40 || !shellNameShape.MatchString(suffix) || seen[p.Command] {
				return fmt.Errorf("registry contains an unsafe shell shortcut")
			}
			seen[p.Command] = true
			names = append(names, p.Command)
		}
		if len(names) > 1000 {
			return fmt.Errorf("registry has too many shell shortcuts")
		}
		fmt.Fprintln(out, "claudock-profile-names-v1")
		for _, name := range names {
			fmt.Fprintln(out, name)
		}
		return nil
	}
	if args[0] != "enable" && args[0] != "disable" && args[0] != "status" {
		return invalid("Use: claudock shell enable|disable|status|profile-names.")
	}
	home, base, err := shellPaths()
	if err != nil {
		return err
	}
	previous, err := readInstallation(home, base)
	if err != nil {
		return err
	}
	if args[0] == "status" {
		if previous.state == nil {
			fmt.Fprintln(out, "disabled")
		} else {
			fmt.Fprintln(out, "enabled")
		}
		return nil
	}
	if args[0] == "disable" && previous.state == nil {
		fmt.Fprintln(out, "Claudock shell integration disabled. Open a new Terminal tab to unload it.")
		return nil
	}
	if err := checkZdotdir(home); err != nil {
		return err
	}
	for _, path := range []string{filepath.Join(home, ".config"), base} {
		if err := os.Mkdir(path, 0700); err != nil && !os.IsExist(err) {
			return fmt.Errorf("cannot prepare shell integration directory")
		}
		if err := validateShellDirectories(home); err != nil {
			return err
		}
	}
	fd, err := syscall.Open(filepath.Join(base, ".shell-integration-lock"),
		syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return fmt.Errorf("cannot open shell integration lock")
	}
	lock := os.NewFile(uintptr(fd), "shell-integration-lock")
	defer lock.Close()
	info, err := lock.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Sys().(*syscall.Stat_t).Nlink != 1 {
		return fmt.Errorf("shell integration lock is not an ordinary file")
	}
	if syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		return fmt.Errorf("another shell integration update is in progress")
	}
	defer syscall.Flock(fd, syscall.LOCK_UN)
	previous, err = readInstallation(home, base)
	if err != nil {
		return err
	}
	type change struct {
		old  snapshot
		data []byte
	}
	var changes []change
	if args[0] == "enable" {
		path, err := os.Executable()
		if err != nil {
			return fmt.Errorf("cannot locate the Claudock executable")
		}
		path, err = filepath.EvalSymlinks(path)
		if err != nil || !validPath(path) {
			return fmt.Errorf("cannot locate an absolute Claudock executable")
		}
		state := shellState{4, path}
		data, err := json.MarshalIndent(state, "", "  ")
		if err != nil {
			return fmt.Errorf("cannot encode shell integration state")
		}
		data = append(data, '\n')
		shell := append([]byte{}, previous.shell.data...)
		if previous.state == nil {
			if len(shell) > 0 && shell[len(shell)-1] != '\n' {
				shell = append(shell, '\n')
			}
			shell = append(shell, shellBlock...)
		}
		changes = []change{{previous.init, []byte(renderShell(state))}, {previous.meta, data}, {previous.shell, shell}}
	} else {
		shell := strings.Replace(string(previous.shell.data), shellBlock, "", 1)
		changes = []change{{previous.shell, []byte(shell)}, {previous.init, nil}, {previous.meta, nil}}
	}
	for _, s := range []snapshot{previous.shell, previous.init, previous.meta} {
		if !s.unchanged() {
			return fmt.Errorf("shell integration changed while saving")
		}
	}
	for _, c := range changes {
		if c.old.path == previous.shell.path && c.old.info != nil && !bytes.Equal(c.old.data, c.data) {
			var token [16]byte
			if _, err := rand.Read(token[:]); err != nil {
				return fmt.Errorf("cannot create a shell backup name")
			}
			backup := filepath.Join(home, ".zshrc.claudock-backup-"+hex.EncodeToString(token[:]))
			if err := atomicWrite(backup, c.old.data, 0600); err != nil {
				return fmt.Errorf("cannot back up .zshrc")
			}
		}
	}
	var written []change
	apply := func(c change) error {
		if err := validateShellDirectories(home); err != nil {
			return err
		}
		if !c.old.unchanged() {
			return fmt.Errorf("shell integration changed while saving")
		}
		if bytes.Equal(c.old.data, c.data) && (c.old.info == nil) == (c.data == nil) {
			return nil
		}
		if c.data == nil {
			return os.Remove(c.old.path)
		}
		mode := os.FileMode(0600)
		if c.old.path == previous.shell.path && c.old.info != nil {
			mode = c.old.info.Mode().Perm()
		}
		return atomicWrite(c.old.path, c.data, mode)
	}
	for _, c := range changes {
		if err := apply(c); err != nil {
			for i := len(written) - 1; i >= 0; i-- {
				ours := written[i]
				current, snapErr := takeSnapshot(ours.old.path)
				if snapErr != nil || !bytes.Equal(current.data, ours.data) || validateShellDirectories(home) != nil {
					continue // Preserve an external edit rather than replace it.
				}
				var rollbackErr error
				if ours.old.info == nil {
					rollbackErr = os.Remove(ours.old.path)
				} else {
					rollbackErr = atomicWrite(ours.old.path, ours.old.data, ours.old.info.Mode().Perm())
				}
				if rollbackErr != nil {
					return fmt.Errorf("shell integration save failed and rollback was incomplete")
				}
			}
			return fmt.Errorf("could not save shell integration")
		}
		written = append(written, c)
	}
	if args[0] == "enable" {
		fmt.Fprintln(out, "Claudock shell integration enabled. Open a new Terminal tab to use managed profile shortcuts.")
	} else {
		fmt.Fprintln(out, "Claudock shell integration disabled. Open a new Terminal tab to unload it.")
	}
	return nil
}

var shellNameShape = regexp.MustCompile("^[A-Za-z0-9_-]+$")
