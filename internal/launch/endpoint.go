package launch

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/laneboss-io/claudock-cli/internal/store"
)

var modelShape = regexp.MustCompile("^[A-Za-z0-9._:/-]+(\\[1m\\])?$")

func endpointPins(p store.Profile) (map[string]string, error) {
	u, err := url.Parse(p.EndpointURL)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" ||
		u.Fragment != "" || strings.ContainsAny(p.EndpointURL, "?#") || len(p.EndpointURL) > 2048 ||
		strings.ContainsFunc(p.EndpointURL, func(r rune) bool { return r < 0x21 || r > 0x7e }) ||
		len(p.EndpointModel) > 128 || !modelShape.MatchString(p.EndpointModel) {
		return nil, fmt.Errorf("%s: invalid endpoint configuration", p.Name)
	}
	if port := u.Port(); port != "" {
		var n int
		if _, err := fmt.Sscanf(port, "%d", &n); err != nil || n < 1 || n > 65535 {
			return nil, fmt.Errorf("%s: invalid endpoint configuration", p.Name)
		}
	}
	pins := make(map[string]string)
	for _, name := range append(append([]string{}, clearedEnvironment...), otherModelVariables...) {
		if name != "CLAUDECODE" && name != "ANTHROPIC_AUTH_TOKEN" {
			pins[name] = ""
		}
	}
	pins["CLAUDE_CONFIG_DIR"] = p.ConfigDirectory
	pins["ANTHROPIC_BASE_URL"] = p.EndpointURL
	pins["CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC"] = "1"
	pins["DISABLE_NON_ESSENTIAL_MODEL_CALLS"] = "1"
	pins["CLAUDE_CODE_DISABLE_1M_CONTEXT"] = "1"
	if strings.HasSuffix(p.EndpointModel, "[1m]") {
		pins["CLAUDE_CODE_DISABLE_1M_CONTEXT"] = ""
	}
	pins["CLAUDE_CODE_DISABLE_ADVISOR_TOOL"] = "1"
	for _, name := range modelVariables {
		pins[name] = p.EndpointModel
	}
	return pins, nil
}

func settingsConflict(object map[string]any, pins map[string]string, p store.Profile, project bool) string {
	allowed := func(value any, base bool, list bool) bool {
		var values []any
		if v, ok := value.([]any); ok {
			values = v
		} else if v, ok := value.(string); ok && !list {
			values = []any{v}
		} else {
			return false
		}
		for _, v := range values {
			if v != p.EndpointModel && (!base || v != strings.TrimSuffix(p.EndpointModel, "[1m]")) {
				return false
			}
		}
		return true
	}
	for _, name := range []string{"availableModels", "fallbackModel"} {
		if value, exists := object[name]; exists && !allowed(value, name == "availableModels", name == "availableModels") {
			return name
		}
	}
	if _, exists := object["modelOverrides"]; exists {
		return "modelOverrides"
	}
	if !project {
		for _, name := range []string{"model", "advisorModel"} {
			if value, exists := object[name]; exists && value != p.EndpointModel {
				return name
			}
		}
		if _, exists := object["modelPicker"]; exists {
			return "modelPicker"
		}
		if helper, exists := object["apiKeyHelper"]; exists && helper != "" {
			return "apiKeyHelper"
		}
	}
	value, exists := object["env"]
	if !exists {
		return ""
	}
	env, ok := value.(map[string]any)
	if !ok {
		return "env"
	}
	if token, exists := env["ANTHROPIC_AUTH_TOKEN"]; exists && token != "" {
		return "env.ANTHROPIC_AUTH_TOKEN"
	}
	if project {
		return ""
	}
	for name, expected := range pins {
		if v, exists := env[name]; exists && v != expected {
			return "env." + name
		}
	}
	if v, exists := env["CLAUDECODE"]; exists && v != "" {
		return "env.CLAUDECODE"
	}
	return ""
}

func endpointArguments(p store.Profile, args []string, cwd string, env map[string]string) ([]string, []byte, error) {
	pins, err := endpointPins(p)
	if err != nil {
		return nil, nil, err
	}
	// Match Swift's all-arguments model check, including after a vendor --.
	for i := 0; i < len(args); i++ {
		for _, option := range []string{"--model", "--fallback-model", "--advisor"} {
			var value string
			if args[i] == option {
				if i+1 == len(args) {
					continue
				}
				i++
				value = args[i]
			} else if strings.HasPrefix(args[i], option+"=") {
				value = strings.TrimPrefix(args[i], option+"=")
			} else {
				continue
			}
			for _, model := range strings.Split(value, ",") {
				if model != p.EndpointModel || (option != "--fallback-model" && strings.Contains(value, ",")) {
					return nil, nil, invalid(fmt.Sprintf("%s: %s can name only the profile's pinned model", p.Name, option))
				}
			}
			break
		}
	}
	options := claudeOptions(args)
	object := make(map[string]any)
	remove := make(map[int]bool)
	given := false
	for _, option := range options {
		switch option.name {
		case "--project-config-root", "--managed-settings", "--forward-home-settings", "--deep-link-cwd-b64", "-w", "--worktree":
			return nil, nil, invalid(p.Name + ": endpoint settings cannot be checked with " + option.name)
		case "--settings":
			if given {
				return nil, nil, invalid(p.Name + ": pass --settings only once")
			}
			given = true
			if option.value == nil {
				return nil, nil, invalid(p.Name + ": --settings requires a JSON object or a readable JSON file")
			}
			text := strings.TrimSpace(*option.value)
			data := []byte(text)
			if !strings.HasPrefix(text, "{") {
				path := text
				if !filepath.IsAbs(path) {
					path = filepath.Join(cwd, path)
				}
				data, err = readRegular(path, 1<<20, false)
			}
			if err == nil && len(data) <= 1<<20 {
				object, err = jsonObject(data)
			} else {
				err = fmt.Errorf("unreadable")
			}
			if err != nil {
				return nil, nil, invalid(p.Name + ": --settings must be one readable JSON object without duplicate names")
			}
			for _, index := range option.indices {
				remove[index] = true
			}
		}
	}
	if key := settingsConflict(object, pins, p, false); key != "" {
		return nil, nil, invalid(p.Name + ": --settings conflicts with pinned " + key)
	}
	if err := checkEndpointFiles(p, pins, cwd); err != nil {
		return nil, nil, err
	}
	settingsEnv, _ := object["env"].(map[string]any)
	if settingsEnv == nil {
		settingsEnv = make(map[string]any)
	}
	for name, value := range pins {
		settingsEnv[name] = value
		if value == "" {
			delete(env, name)
		} else {
			env[name] = value
		}
	}
	for name := range env {
		if strings.HasPrefix(name, "OTEL_") || name == "CLAUDE_CODE_ENABLE_TELEMETRY" {
			delete(env, name)
		}
	}
	object["env"] = settingsEnv
	object["apiKeyHelper"] = ""
	object["availableModels"] = []string{strings.TrimSuffix(p.EndpointModel, "[1m]")}
	data, err := json.Marshal(object)
	if err != nil {
		return nil, nil, invalid(p.Name + ": cannot merge endpoint settings")
	}
	result := make([]string, 0, len(args))
	for i, arg := range args {
		if !remove[i] {
			result = append(result, arg)
		}
	}
	return result, data, nil
}

func checkEndpointFiles(p store.Profile, pins map[string]string, cwd string) error {
	type settingsFile struct {
		path    string
		project bool
	}
	files := []settingsFile{{filepath.Join(cwd, ".claude/settings.json"), true}}
	folders := []string{cwd}
	for directory := cwd; ; directory = filepath.Dir(directory) {
		gitPath := filepath.Join(directory, ".git")
		if info, err := os.Stat(gitPath); err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			folders = append(folders, directory)
			if info.Mode().IsRegular() {
				data, err := readRegular(gitPath, 65536, false)
				if err == nil && strings.HasPrefix(string(data), "gitdir:") {
					gitDir := strings.TrimSpace(strings.TrimPrefix(string(data), "gitdir:"))
					if !filepath.IsAbs(gitDir) {
						gitDir = filepath.Join(directory, gitDir)
					}
					common, err := readRegular(filepath.Join(gitDir, "commondir"), 65536, false)
					if err == nil {
						root := strings.TrimSpace(string(common))
						if !filepath.IsAbs(root) {
							root = filepath.Join(gitDir, root)
						}
						if filepath.Base(root) == ".git" {
							root = filepath.Dir(root)
						}
						if root != os.Getenv("HOME") {
							folders = append(folders, root)
						}
					}
				}
			}
			break
		}
		if filepath.Dir(directory) == directory {
			break
		}
	}
	for _, folder := range folders {
		files = append(files, settingsFile{filepath.Join(folder, ".claude/settings.local.json"), true})
	}
	managed := "/etc/claude-code"
	if runtime.GOOS == "darwin" {
		managed = "/Library/Application Support/ClaudeCode"
		// MDM property-list validation is not ported. Fail closed when a
		// device or user policy is present rather than launch past its pins.
		mdm := "/Library/Managed Preferences"
		policies := []string{filepath.Join(mdm, "com.anthropic.claudecode.plist")}
		users, err := os.ReadDir(mdm)
		if err != nil && !os.IsNotExist(err) {
			return invalid(p.Name + ": cannot inspect managed endpoint preferences")
		}
		for _, entry := range users {
			if entry.IsDir() {
				policies = append(policies, filepath.Join(mdm, entry.Name(), "com.anthropic.claudecode.plist"))
			}
		}
		for _, path := range policies {
			if _, err := os.Lstat(path); err == nil || !os.IsNotExist(err) {
				return invalid(p.Name + ": this port cannot validate MDM endpoint preferences")
			}
		}
	}
	files = append(files, settingsFile{filepath.Join(managed, "managed-settings.json"), false})
	dropIns, err := os.ReadDir(filepath.Join(managed, "managed-settings.d"))
	if err != nil && !os.IsNotExist(err) {
		return invalid(p.Name + ": cannot inspect managed endpoint settings")
	}
	for _, entry := range dropIns {
		if strings.HasSuffix(entry.Name(), ".json") {
			files = append(files, settingsFile{filepath.Join(managed, "managed-settings.d", entry.Name()), false})
		}
	}
	for _, file := range files {
		data, err := optionalFile(file.path, 4<<20, false)
		if err != nil {
			return invalid(p.Name + ": an endpoint settings file is unreadable")
		}
		if data == nil || len(strings.TrimSpace(string(data))) == 0 {
			continue
		}
		object, err := jsonObject(data)
		if err != nil {
			return invalid(p.Name + ": an endpoint settings file is not strict JSON")
		}
		if key := settingsConflict(object, pins, p, file.project); key != "" {
			return invalid(p.Name + ": an endpoint settings file conflicts with " + key)
		}
	}
	return nil
}
