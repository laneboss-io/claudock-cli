package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

var sharedDirectories = []string{"projects", "plugins", "skills", "agents", "commands", "hooks"}
var commonWorkspaceNames = []string{"settings.json", "settings.local.json", "CLAUDE.md", "plugins", "skills", "agents", "commands", "hooks", "plans", "tasks", "teams", "sessions", "session-env", "ide", "image-cache", "paste-cache", "file-history", "cache", "security", "shell-snapshots", "backups", "debug", "downloads", "feedback-bundles", "jobs", "daemon"}

type workspaceEntry struct {
	path    string
	info    os.FileInfo
	content []byte
	link    string
}

type preparedWorkspace struct {
	entries   []workspaceEntry
	committed bool
}

func (w *preparedWorkspace) record(path string, content []byte, link string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("could not inspect prepared workspace")
	}
	w.entries = append(w.entries, workspaceEntry{path: path, info: info, content: content, link: link})
	return nil
}

func (w *preparedWorkspace) directory(path string, allowLink bool) error {
	info, err := os.Lstat(path)
	if err == nil {
		if info.IsDir() {
			return nil
		}
		if allowLink && info.Mode()&os.ModeSymlink != 0 {
			target, err := os.Stat(path)
			if err == nil && target.IsDir() {
				return nil
			}
		}
		return errors.New("workspace directory is unsafe")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return errors.New("workspace directory is unavailable")
	}
	if err := os.Mkdir(path, 0700); err != nil {
		return errors.New("could not create workspace directory")
	}
	return w.record(path, nil, "")
}

func (w *preparedWorkspace) file(path string, initial []byte) error {
	if _, err := os.Lstat(path); err == nil {
		fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if err != nil {
			return errors.New("shared workspace file is unsafe")
		}
		file := os.NewFile(uintptr(fd), "shared workspace")
		info, statErr := file.Stat()
		closeErr := file.Close()
		if statErr != nil || closeErr != nil || !info.Mode().IsRegular() {
			return errors.New("shared workspace file is unsafe")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("shared workspace file is unavailable")
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errors.New("could not create shared workspace file")
	}
	if err := w.record(path, append([]byte{}, initial...), ""); err != nil {
		f.Close()
		return err
	}
	_, writeErr := f.Write(initial)
	syncErr := f.Sync()
	closeErr := f.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return errors.New("could not save shared workspace file")
	}
	return nil
}

func (w *preparedWorkspace) validate() error {
	for _, entry := range w.entries {
		current, err := os.Lstat(entry.path)
		if err != nil || !os.SameFile(current, entry.info) {
			return errors.New("workspace changed while preparing the profile")
		}
		if entry.link != "" {
			target, err := os.Readlink(entry.path)
			if err != nil || target != entry.link {
				return errors.New("workspace link changed while preparing the profile")
			}
		}
	}
	return nil
}

func (w *preparedWorkspace) rollback() {
	if w.committed {
		return
	}
	for i := len(w.entries) - 1; i >= 0; i-- {
		entry := w.entries[i]
		current, err := os.Lstat(entry.path)
		if err != nil || !os.SameFile(current, entry.info) {
			continue
		}
		if entry.link != "" {
			target, err := os.Readlink(entry.path)
			if err != nil || target != entry.link {
				continue
			}
		}
		if entry.info.Mode().IsRegular() {
			data, err := readFile(entry.path, int64(len(entry.content)+1), false)
			if err != nil || !bytes.Equal(data, entry.content) {
				continue
			}
		}
		// Remove only owned entries. os.Remove preserves nonempty directories.
		_ = os.Remove(entry.path)
	}
}

func (w *preparedWorkspace) commit() { w.committed = true }

func prepareWorkspace(path string, codex bool) (*preparedWorkspace, error) {
	home, err := homeDirectory()
	if err != nil {
		return nil, err
	}
	w := &preparedWorkspace{}
	fail := func(err error) (*preparedWorkspace, error) { w.rollback(); return nil, err }
	if !codex {
		if err := validateSettings(filepath.Join(home, ".claude")); err != nil {
			return fail(err)
		}
	}
	account := filepath.Dir(path)
	accounts := filepath.Dir(account)
	if err := w.directory(accounts, false); err != nil {
		return fail(err)
	}
	if _, err := os.Lstat(account); err == nil || !errors.Is(err, os.ErrNotExist) {
		return fail(errors.New("account directory already exists or is unavailable"))
	}
	if err := w.directory(account, false); err != nil {
		return fail(err)
	}
	if err := w.directory(path, false); err != nil {
		return fail(err)
	}
	if codex {
		return w, nil
	}
	source := filepath.Join(home, ".claude")
	if err := w.directory(source, true); err != nil {
		return fail(err)
	}
	for _, name := range sharedDirectories {
		if err := w.directory(filepath.Join(source, name), true); err != nil {
			return fail(err)
		}
	}
	if err := w.file(filepath.Join(source, "history.jsonl"), []byte{}); err != nil {
		return fail(err)
	}
	if err := w.file(filepath.Join(source, "settings.json"), []byte("{}\n")); err != nil {
		return fail(err)
	}
	names := append(append([]string{}, sharedDirectories...), "history.jsonl", "settings.json")
	for _, name := range commonWorkspaceNames {
		if contains(names, name) {
			continue
		}
		if _, err := os.Lstat(filepath.Join(source, name)); err == nil {
			names = append(names, name)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fail(errors.New("shared workspace entry is unavailable"))
		}
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return fail(errors.New("shared workspace is unavailable"))
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "security_warnings_state_") && strings.HasSuffix(entry.Name(), ".json") {
			names = append(names, entry.Name())
		}
	}
	for _, name := range names {
		target, link := filepath.Join(source, name), filepath.Join(path, name)
		if err := os.Symlink(target, link); err != nil {
			return fail(errors.New("could not link shared workspace"))
		}
		if err := w.record(link, nil, target); err != nil {
			return fail(err)
		}
	}
	if err := w.validate(); err != nil {
		return fail(err)
	}
	return w, nil
}

// Settings links are intentional shared workspace links; opened targets must be bounded regular files.
func readSettingsFile(path string) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOENT) {
			return nil, os.ErrNotExist
		}
		return nil, errors.New("settings file is unavailable")
	}
	f := os.NewFile(uintptr(fd), "settings")
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maximumCredentialBytes {
		return nil, errors.New("settings file is unsafe or too large")
	}
	data := make([]byte, info.Size())
	if len(data) > 0 {
		if _, err := f.ReadAt(data, 0); err != nil {
			return nil, errors.New("could not read settings")
		}
	}
	after, err := f.Stat()
	if err != nil || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return nil, errors.New("settings changed while reading")
	}
	return data, nil
}

func validateSettings(directory string) error {
	if !validPath(directory) {
		return errors.New("config directory is invalid")
	}
	for _, name := range []string{"settings.json", "settings.local.json"} {
		path := filepath.Join(directory, name)
		data, err := readSettingsFile(path)
		if errors.Is(err, os.ErrNotExist) {
			if _, linkErr := os.Lstat(path); errors.Is(linkErr, os.ErrNotExist) {
				continue
			}
			return errors.New("settings contain an unavailable link")
		}
		if err != nil {
			return err
		}
		var root map[string]json.RawMessage
		if decodeJSON(data, &root) != nil || root == nil {
			return errors.New("settings must contain a JSON object")
		}
		if raw, ok := root["apiKeyHelper"]; ok {
			var helper string
			if json.Unmarshal(raw, &helper) != nil || helper != "" {
				return errors.New("settings cannot select an API key helper")
			}
		}
		if raw, ok := root["env"]; ok {
			var environment map[string]any
			if decodeJSON(raw, &environment) != nil || environment == nil {
				return errors.New("settings env must be an object")
			}
			for key, value := range environment {
				if strings.HasPrefix(key, "CLAUDE_CODE_USE_") {
					switch v := value.(type) {
					case string:
						if !contains([]string{"", "0", "false"}, strings.ToLower(strings.TrimSpace(v))) {
							return errors.New("settings select an external provider")
						}
					case float64:
						if v != 0 {
							return errors.New("settings select an external provider")
						}
					case bool:
						if v {
							return errors.New("settings select an external provider")
						}
					default:
						return errors.New("settings provider flag is invalid")
					}
				}
				if contains([]string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "ANTHROPIC_CUSTOM_HEADERS", "CLAUDE_CONFIG_DIR", "CLAUDE_SECURESTORAGE_CONFIG_DIR", "CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CODE_OAUTH_REFRESH_TOKEN", "CLAUDE_CODE_OAUTH_CLIENT_ID", "CLAUDE_CODE_OAUTH_SCOPES", "ANTHROPIC_PROFILE", "ANTHROPIC_FEDERATION_RULE_ID", "ANTHROPIC_IDENTITY_TOKEN_FILE", "ANTHROPIC_ORGANIZATION_ID", "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR", "CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_GATEWAY_TOKEN_FILE_DESCRIPTOR", "CLAUDE_CODE_WEBSOCKET_AUTH_FILE_DESCRIPTOR", "CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST", "CLAUDE_BG_AUTH_SNAPSHOT_PATH", "CLAUDE_CODE_SDK_HAS_HOST_AUTH_REFRESH", "CLAUDE_CODE_SDK_HAS_OAUTH_REFRESH", "ANTHROPIC_UNIX_SOCKET"}, key) {
					text, ok := value.(string)
					if !ok || text != "" {
						return errors.New("settings override profile credentials or storage")
					}
				}
				if key == "ANTHROPIC_BASE_URL" {
					text, ok := value.(string)
					if !ok || (text != "" && !contains([]string{"https://api.anthropic.com", "https://api.anthropic.com/", "https://api.anthropic.com:443", "https://api.anthropic.com:443/"}, strings.ToLower(text))) {
						return errors.New("settings override the Anthropic endpoint")
					}
				}
			}
		}
	}
	return nil
}
