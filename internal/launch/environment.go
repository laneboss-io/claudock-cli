package launch

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/laneboss-io/claudock-cli/internal/policy"
	"github.com/laneboss-io/claudock-cli/internal/store"
)

var clearedEnvironment = strings.Fields(
	"ANTHROPIC_API_KEY ANTHROPIC_AUTH_TOKEN ANTHROPIC_BASE_URL CLAUDE_CODE_OAUTH_TOKEN " +
		"CLAUDE_CONFIG_DIR CLAUDE_SECURESTORAGE_CONFIG_DIR CLAUDE_CODE_USE_VERTEX CLAUDE_CODE_USE_BEDROCK CLAUDE_CODE_USE_FOUNDRY CLAUDE_CODE_USE_GATEWAY " +
		"ANTHROPIC_VERTEX_PROJECT_ID CLOUD_ML_REGION ANTHROPIC_MODEL ANTHROPIC_SMALL_FAST_MODEL " +
		"ANTHROPIC_DEFAULT_OPUS_MODEL ANTHROPIC_DEFAULT_SONNET_MODEL ANTHROPIC_DEFAULT_HAIKU_MODEL ANTHROPIC_DEFAULT_FABLE_MODEL " +
		"CLAUDECODE ANTHROPIC_CUSTOM_HEADERS ANTHROPIC_PROFILE ANTHROPIC_FEDERATION_RULE_ID ANTHROPIC_IDENTITY_TOKEN_FILE ANTHROPIC_ORGANIZATION_ID " +
		"CLAUDE_CODE_OAUTH_REFRESH_TOKEN CLAUDE_CODE_OAUTH_SCOPES CLAUDE_CODE_OAUTH_CLIENT_ID " +
		"CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR CLAUDE_CODE_OAUTH_TOKEN_FILE_DESCRIPTOR CLAUDE_CODE_GATEWAY_TOKEN_FILE_DESCRIPTOR " +
		"CLAUDE_CODE_WEBSOCKET_AUTH_FILE_DESCRIPTOR CLAUDE_CODE_PROVIDER_MANAGED_BY_HOST CLAUDE_BG_AUTH_SNAPSHOT_PATH " +
		"CLAUDE_CODE_SDK_HAS_HOST_AUTH_REFRESH CLAUDE_CODE_SDK_HAS_OAUTH_REFRESH ANTHROPIC_UNIX_SOCKET " +
		"CLAUDE_CODE_USE_ANTHROPIC_AWS CLAUDE_CODE_USE_ANTHROPIC_GOOGLE_CLOUD CLAUDE_CODE_USE_MANTLE")

var otherModelVariables = strings.Fields(
	"ANTHROPIC_DEFAULT_MODEL CLAUDE_CODE_AUTO_MODE_MODEL CLAUDE_CODE_BG_CLASSIFIER_MODEL " +
		"CLAUDE_CODE_WORKFLOW_SUBAGENT_MODEL ANTHROPIC_CUSTOM_MODEL_OPTION ANTHROPIC_CUSTOM_MODEL_OPTION_NAME " +
		"ANTHROPIC_CUSTOM_MODEL_OPTION_DESCRIPTION ANTHROPIC_CUSTOM_MODEL_OPTION_SUPPORTED_CAPABILITIES " +
		"CLAUDE_CODE_USE_GATEWAY CLAUDE_CODE_ENABLE_EXPERIMENTAL_ADVISOR_TOOL")

var modelVariables = strings.Fields(
	"ANTHROPIC_MODEL ANTHROPIC_DEFAULT_OPUS_MODEL ANTHROPIC_DEFAULT_SONNET_MODEL " +
		"ANTHROPIC_DEFAULT_HAIKU_MODEL ANTHROPIC_DEFAULT_FABLE_MODEL ANTHROPIC_SMALL_FAST_MODEL CLAUDE_CODE_SUBAGENT_MODEL")

func isolatedEnvironment(inherited []string) map[string]string {
	result := make(map[string]string)
	for _, entry := range inherited {
		if name, value, ok := strings.Cut(entry, "="); ok {
			result[name] = value
		}
	}
	for _, name := range clearedEnvironment {
		delete(result, name)
	}
	for name := range result {
		if strings.HasPrefix(name, "OPENAI_") || strings.HasPrefix(name, "CODEX_") {
			delete(result, name)
		}
	}
	return result
}

func environmentList(env map[string]string) []string {
	names := make([]string, 0, len(env))
	for name := range env {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]string, 0, len(names))
	for _, name := range names {
		result = append(result, name+"="+env[name])
	}
	return result
}

func optionalFile(path string, limit int64, noFollow bool) ([]byte, error) {
	data, err := readRegular(path, limit, noFollow)
	if errors.Is(err, os.ErrNotExist) {
		if _, entryErr := os.Lstat(path); errors.Is(entryErr, os.ErrNotExist) {
			return nil, nil
		}
	}
	return data, err
}

func validateConfig(p store.Profile) error {
	failure := fmt.Errorf("%s: config settings contain an unsupported authentication or provider override", p.Name)
	if info, err := os.Stat(p.ConfigDirectory); err == nil {
		if !info.IsDir() {
			return failure
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return failure
	} else if _, entryErr := os.Lstat(p.ConfigDirectory); !errors.Is(entryErr, os.ErrNotExist) {
		return failure
	}
	for _, name := range []string{"settings.json", "settings.local.json"} {
		data, err := optionalFile(filepath.Join(p.ConfigDirectory, name), 1<<20, false)
		if err != nil {
			return failure
		}
		if data == nil {
			continue
		}
		object, err := jsonObject(data)
		if err != nil {
			return failure
		}
		if helper, exists := object["apiKeyHelper"]; exists && helper != "" {
			return failure
		}
		value, exists := object["env"]
		if !exists {
			continue
		}
		env, ok := value.(map[string]any)
		if !ok {
			return failure
		}
		for _, key := range clearedEnvironment {
			value, exists := env[key]
			if !exists || key == "CLAUDECODE" {
				continue
			}
			if key == "ANTHROPIC_BASE_URL" {
				text, ok := value.(string)
				if !ok {
					return failure
				}
				if text == "" {
					continue
				}
				u, err := url.Parse(text)
				if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Hostname(), "api.anthropic.com") ||
					(u.Port() != "" && u.Port() != "443") || (u.Path != "" && u.Path != "/") ||
					u.User != nil || u.RawQuery != "" || u.Fragment != "" {
					return failure
				}
				continue
			}
			if strings.HasPrefix(key, "CLAUDE_CODE_USE_") {
				switch value {
				case "", "0", "false", false, json.Number("0"):
					continue
				default:
					return failure
				}
			}
			// Model settings do not select authentication.
			if strings.Contains(key, "MODEL") || key == "CLOUD_ML_REGION" || key == "ANTHROPIC_VERTEX_PROJECT_ID" {
				continue
			}
			if value != "" {
				return failure
			}
		}
	}
	return nil
}

// Errors from a credential backend are deliberately not interpolated: backend
// error strings may contain sensitive input. ErrNoSecret or ErrNotExist means absent.
func secret(p store.Profile, item string) (string, error) {
	value, err := store.Secret(p, item)
	if errors.Is(err, store.ErrNoSecret) || errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("%s: credential item %s is unavailable", p.Name, item)
	}
	return value, nil
}

var apiKeyShape = regexp.MustCompile("^sk-ant-[a-z]+[0-9]{0,4}-[A-Za-z0-9_-]+$")
var refusedAPIKey = regexp.MustCompile("^sk-ant-(oat|ort|admin|sid|si|cc|ccsr)[0-9]{0,4}-")
var inferenceShape = regexp.MustCompile("^sk-ant-oat01-[A-Za-z0-9._~+/=-]+$")

func credentialEnvironment(p store.Profile, args []string, env map[string]string) error {
	switch p.Kind {
	case store.APIKey, store.ConsoleLogin:
		if !p.Managed {
			return fmt.Errorf("%s: Console profiles must be managed by Claudock", p.Name)
		}
		item := "apiKey"
		if p.Kind == store.ConsoleLogin {
			item = "consoleKey"
		}
		key, err := secret(p, item)
		if err != nil {
			return err
		}
		if len(key) > 512 || !apiKeyShape.MatchString(key) || refusedAPIKey.MatchString(key) {
			return fmt.Errorf("%s: credential item %s is missing or invalid", p.Name, item)
		}
		if p.Kind == store.APIKey {
			env["ANTHROPIC_API_KEY"] = key
		}
	case store.Endpoint:
		if !p.Managed {
			return fmt.Errorf("%s: endpoint profiles must be managed by Claudock", p.Name)
		}
		key, err := secret(p, "endpointKey")
		if err != nil {
			return err
		}
		if key == "" || len(key) > 512 || strings.HasPrefix(strings.ToLower(key), "sk-ant-") ||
			strings.ContainsFunc(key, func(r rune) bool { return r < 0x21 || r > 0x7e }) ||
			strings.ContainsAny(key[:1]+key[len(key)-1:], "'\"") {
			return fmt.Errorf("%s: credential item endpointKey is missing or invalid", p.Name)
		}
		env["ANTHROPIC_AUTH_TOKEN"] = key
	case store.Subscription:
		// setup-token always uses Claude's own login, even with an expired token.
		if len(args) > 0 && args[0] == "setup-token" {
			return nil
		}
		required, err := policy.Required()
		if err != nil {
			return err
		}
		raw, err := secret(p, store.InferenceTokenRecordItem)
		if err != nil {
			return err
		}
		token, err := inferenceToken(raw, p)
		if err != nil {
			return err
		}
		if token == "" && required {
			return fmt.Errorf("%s: a valid inferenceToken credential is required", p.Name)
		}
		if token != "" {
			env["CLAUDE_CODE_OAUTH_TOKEN"] = token
		}
	}
	return nil
}

func inferenceToken(raw string, p store.Profile) (string, error) {
	if raw == "" {
		return "", nil
	}
	failure := fmt.Errorf("%s: credential item inferenceToken is invalid or expired", p.Name)
	// A backend may expose a plain imported token or the complete Swift item.
	if inferenceShape.MatchString(raw) && len(raw) <= 16384 {
		return raw, nil
	}
	if len(raw) > 16384 {
		return "", failure
	}
	data := []byte(raw)
	if !strings.HasPrefix(strings.TrimSpace(raw), "{") {
		var err error
		data, err = hex.DecodeString(strings.TrimSpace(raw))
		if err != nil {
			return "", failure
		}
	}
	var saved struct {
		Version     int      `json:"version"`
		AccessToken string   `json:"accessToken"`
		ExpiresAt   *float64 `json:"expiresAt"`
		Provenance  string   `json:"provenance"`
		Identity    *struct {
			Account      string `json:"accountUUID"`
			Organization string `json:"organizationUUID"`
		} `json:"identity"`
	}
	if _, err := jsonObject(data); err != nil {
		return "", failure
	}
	if len(data) > 8192 || json.Unmarshal(data, &saved) != nil || (saved.Version != 1 && saved.Version != 2) ||
		saved.AccessToken == "" || strings.ContainsFunc(saved.AccessToken, unicode.IsSpace) ||
		strings.ContainsFunc(saved.AccessToken, unicode.IsControl) {
		return "", failure
	}
	if saved.Version == 1 {
		if saved.Provenance != "" && saved.Provenance != "browser" {
			return "", failure
		}
		saved.Provenance = "browser"
	}
	if saved.Provenance != "browser" && saved.Provenance != "pasted" {
		return "", failure
	}
	if saved.Provenance == "pasted" && !inferenceShape.MatchString(saved.AccessToken) {
		return "", failure
	}
	if saved.Provenance == "browser" && (saved.Identity == nil || saved.ExpiresAt == nil) {
		return "", failure
	}
	if saved.ExpiresAt != nil && (math.IsNaN(*saved.ExpiresAt) || math.IsInf(*saved.ExpiresAt, 0) || *saved.ExpiresAt <= 0) {
		return "", failure
	}
	if saved.Identity != nil {
		if !sessionIDShape.MatchString(saved.Identity.Account) || !sessionIDShape.MatchString(saved.Identity.Organization) {
			return "", failure
		}
		statePath := filepath.Join(p.ConfigDirectory, ".claude.json")
		if p.Command == "claude" {
			statePath = filepath.Join(os.Getenv("HOME"), ".claude.json")
		}
		data, err := readRegular(statePath, 10<<20, true)
		var state struct {
			Account struct {
				Account      string `json:"accountUuid"`
				Organization string `json:"organizationUuid"`
			} `json:"oauthAccount"`
		}
		known := err == nil && json.Unmarshal(data, &state) == nil && state.Account.Account != "" && state.Account.Organization != ""
		if (saved.Provenance == "browser" && !known) || (known &&
			(!strings.EqualFold(saved.Identity.Account, state.Account.Account) ||
				!strings.EqualFold(saved.Identity.Organization, state.Account.Organization))) {
			return "", fmt.Errorf("%s: inferenceToken account does not match the profile login", p.Name)
		}
	}
	if saved.ExpiresAt != nil && *saved.ExpiresAt <= float64(time.Now().Unix()) {
		if saved.Provenance == "browser" {
			return "", nil
		}
		return "", failure
	}
	return saved.AccessToken, nil
}

func preferences() (map[string]any, error) {
	home := os.Getenv("HOME")
	if !validPath(home) {
		return nil, fmt.Errorf("HOME must be an absolute directory")
	}
	path := filepath.Join(home, ".config/claudock/preferences.json")
	if runtime.GOOS == "darwin" {
		path = filepath.Join(home, "Library/Preferences/io.github.claudeusage.ClaudeUsage.plist")
	}
	data, err := optionalFile(path, 1<<20, true)
	if err != nil {
		return nil, fmt.Errorf("cannot read Claudock preferences")
	}
	if data == nil {
		return make(map[string]any), nil
	}
	if runtime.GOOS != "darwin" {
		object, err := jsonObject(data)
		if err != nil {
			return nil, fmt.Errorf("cannot read Claudock preferences")
		}
		return object, nil
	}
	if strings.HasPrefix(string(data), "bplist") {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "/usr/bin/plutil", "-convert", "xml1", "-o", "-", "-")
		cmd.Stdin = strings.NewReader(string(data))
		data, err = cmd.Output()
		if err != nil {
			return nil, fmt.Errorf("cannot decode Claudock preferences")
		}
	}
	d := xml.NewDecoder(strings.NewReader(string(data)))
	result := make(map[string]any)
	var key string
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("cannot decode Claudock preferences")
		}
		start, ok := token.(xml.StartElement)
		if !ok {
			continue
		}
		switch start.Name.Local {
		case "key":
			if err := d.DecodeElement(&key, &start); err != nil {
				return nil, fmt.Errorf("cannot decode Claudock preferences")
			}
		case "true", "false":
			result[key] = start.Name.Local == "true"
			key = ""
		case "string":
			var value string
			if err := d.DecodeElement(&value, &start); err != nil {
				return nil, fmt.Errorf("cannot decode Claudock preferences")
			}
			result[key] = value
			key = ""
		}
	}
	return result, nil
}
