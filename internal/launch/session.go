package launch

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	"github.com/laneboss-io/claudock-cli/internal/store"
)

var sessionIDShape = regexp.MustCompile("(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$")
var projectPinShape = regexp.MustCompile("^[A-Za-z0-9_-]{1,64}$")
var reservedProjectPin = regexp.MustCompile("(?i)^(con|prn|aux|nul|com[0-9]|lpt[0-9])$")

func projectFolderName(path string) string {
	units := utf16.Encode([]rune(path))
	sanitized := make([]byte, len(units))
	var hash int32
	for i, unit := range units {
		hash = hash*31 + int32(unit)
		sanitized[i] = '-'
		if unit >= 'A' && unit <= 'Z' || unit >= 'a' && unit <= 'z' || unit >= '0' && unit <= '9' {
			sanitized[i] = byte(unit)
		}
	}
	if len(sanitized) <= 200 {
		return string(sanitized)
	}
	magnitude := int64(hash)
	if magnitude < 0 {
		magnitude = -magnitude
	}
	return string(sanitized[:200]) + "-" + strconv.FormatInt(magnitude, 36)
}

type sessionContext struct {
	config   string
	cwd      string
	projects string
	folders  []string
}

func newSessionContext(config, cwd string, env map[string]string) sessionContext {
	if real, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = real
	}
	projects := filepath.Join(config, "projects")
	slug := projectFolderName(cwd)
	folders := []string{filepath.Join(projects, slug)}
	if pin := env["CLAUDE_CODE_PROJECT_DIR_NAME"]; projectPinShape.MatchString(pin) && !reservedProjectPin.MatchString(pin) {
		folders = append([]string{filepath.Join(projects, pin)}, folders...)
	}
	return sessionContext{config, cwd, projects, folders}
}

type sessionCandidate struct {
	path     string
	id       string
	modified time.Time
}

func sessionFiles(folder string) []sessionCandidate {
	entries, err := os.ReadDir(folder)
	if err != nil {
		return nil
	}
	var result []sessionCandidate
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".jsonl") || !sessionIDShape.MatchString(strings.TrimSuffix(entry.Name(), ".jsonl")) {
			continue
		}
		path := filepath.Join(folder, entry.Name())
		info, err := os.Lstat(path)
		if err == nil && info.Mode().IsRegular() {
			result = append(result, sessionCandidate{path, strings.TrimSuffix(entry.Name(), ".jsonl"), info.ModTime()})
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].modified.Equal(result[j].modified) {
			return result[i].id < result[j].id
		}
		return result[i].modified.After(result[j].modified)
	})
	return result
}

func transcriptLines(path string, each func([]byte) bool) error {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("unreadable session")
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("unreadable session")
	}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 65536), 16<<20)
	for scanner.Scan() {
		if !each(scanner.Bytes()) {
			return nil
		}
	}
	if scanner.Err() != nil {
		return fmt.Errorf("unreadable session")
	}
	return nil
}

func (c sessionContext) resumed(id string) []string {
	holds := func(path string) bool {
		found := false
		err := transcriptLines(path, func(line []byte) bool {
			var record struct {
				Type      string
				UUID      string
				Sidechain bool `json:"isSidechain"`
			}
			if json.Unmarshal(line, &record) == nil && !record.Sidechain && record.UUID != "" &&
				(record.Type == "assistant" || record.Type == "user" || record.Type == "attachment" || record.Type == "system") {
				found = true
				return false
			}
			return true
		})
		return err == nil && found
	}
	for _, folder := range c.folders {
		path := filepath.Join(folder, id+".jsonl")
		if holds(path) {
			return []string{path}
		}
	}
	var result []string
	entries, _ := os.ReadDir(c.projects)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(c.projects, entry.Name(), id+".jsonl")
		if holds(path) {
			result = append(result, path)
		}
	}
	return result
}

func (c sessionContext) latest(interactive, fork bool) string {
	var candidates []sessionCandidate
	for _, folder := range c.folders {
		candidates = sessionFiles(folder)
		if len(candidates) > 0 {
			break
		}
	}
	// The legacy layout can have a different slug. Match recorded cwd before
	// considering a transcript from such a directory.
	if len(candidates) == 0 {
		entries, _ := os.ReadDir(c.projects)
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			for _, candidate := range sessionFiles(filepath.Join(c.projects, entry.Name())) {
				if recordedCWD(candidate.path) == c.cwd {
					candidates = append(candidates, candidate)
				}
			}
		}
	}
	if aliases, err := optionalFile(filepath.Join(c.folders[0], ".session-aliases"), 1<<20, false); err == nil {
		for _, alias := range strings.Split(string(aliases), "\n") {
			if validPath(alias) {
				candidates = append(candidates, sessionFiles(alias)...)
			}
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].modified.After(candidates[j].modified) })
	live := c.liveSessions()
	for _, candidate := range candidates {
		if !fork && live[strings.ToLower(candidate.id)] {
			continue
		}
		data, err := readRegular(candidate.path, 64<<20, false)
		if err != nil {
			// The latest transcript must still be checked by the streaming
			// model reader. Skipping it could inspect an older session while
			// Claude resumes this one, including when it exceeds this bound.
			return candidate.path
		}
		if len(data) == 0 {
			return candidate.path
		}
		// Distinct real directories can sanitize to the same project slug.
		// A transcript for another existing directory must not win continue.
		if recorded := recordedCWD(candidate.path); recorded != "" && !strings.EqualFold(recorded, c.cwd) &&
			strings.EqualFold(projectFolderName(recorded), projectFolderName(c.cwd)) {
			if real, err := filepath.EvalSymlinks(recorded); err == nil && !strings.EqualFold(real, c.cwd) {
				continue
			}
		}
		if !bytes.Contains(data, []byte("\"parentUuid\":")) {
			continue
		}
		var skip bool
		var continued string
		for _, line := range bytes.Split(data, []byte("\n")) {
			var r struct {
				Type       string
				Sidechain  bool   `json:"isSidechain"`
				Team       string `json:"teamName"`
				Kind       string `json:"sessionKind"`
				Entrypoint string
				Continued  string `json:"continuedInSessionId"`
				Meta       bool   `json:"isMeta"`
				Message    struct {
					Stop any `json:"stop_reason"`
				}
			}
			if json.Unmarshal(line, &r) != nil {
				continue
			}
			if r.Sidechain || r.Team != "" || r.Kind == "daemon" || r.Kind == "daemon-worker" ||
				(interactive && (r.Entrypoint == "sdk-cli" || r.Entrypoint == "sdk-ts" || r.Entrypoint == "sdk-py")) {
				skip = true
			}
			if r.Type == "continued-in" {
				continued = r.Continued
			} else if (r.Type == "user" && !r.Meta) || (r.Type == "assistant" && r.Message.Stop != nil) {
				continued = ""
			}
		}
		if skip || (interactive && bytes.Contains(data, []byte("<command-name>/loop</command-name>"))) {
			continue
		}
		if sessionIDShape.MatchString(continued) {
			if !fork && live[strings.ToLower(continued)] {
				return ""
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(candidate.path), continued+".jsonl")); err == nil {
				continue
			}
		}
		return candidate.path
	}
	return ""
}

func recordedCWD(path string) string {
	var first, relocated string
	_ = transcriptLines(path, func(line []byte) bool {
		var r struct {
			Type      string
			CWD       string
			Relocated string `json:"relocatedCwd"`
		}
		if json.Unmarshal(line, &r) == nil {
			if first == "" {
				first = r.CWD
			}
			if r.Type == "relocated" && r.Relocated != "" {
				relocated = r.Relocated
			}
		}
		return true
	})
	if relocated != "" {
		return relocated
	}
	return first
}

func (c sessionContext) liveSessions() map[string]bool {
	result := make(map[string]bool)
	entries, _ := os.ReadDir(filepath.Join(c.config, "sessions"))
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := readRegular(filepath.Join(c.config, "sessions", entry.Name()), 1<<20, false)
		var record struct {
			PID     json.RawMessage
			Kind    string
			Session string `json:"sessionId"`
		}
		if err != nil || json.Unmarshal(data, &record) != nil || record.Kind == "interactive" {
			continue
		}
		var pid int
		if json.Unmarshal(record.PID, &pid) != nil {
			var text string
			if json.Unmarshal(record.PID, &text) != nil {
				continue
			}
			pid, _ = strconv.Atoi(text)
		}
		if pid > 0 && syscall.Kill(pid, 0) == nil {
			result[strings.ToLower(record.Session)] = true
		}
	}
	return result
}

func (c sessionContext) titled(title string) []string {
	var result []string
	for _, folder := range c.folders {
		for _, candidate := range sessionFiles(folder) {
			var custom, generated string
			err := transcriptLines(candidate.path, func(line []byte) bool {
				var r struct {
					Custom    string `json:"customTitle"`
					Generated string `json:"aiTitle"`
				}
				if json.Unmarshal(line, &r) == nil {
					if r.Custom != "" {
						custom = r.Custom
					}
					if r.Generated != "" {
						generated = r.Generated
					}
				}
				return true
			})
			if data, err := optionalFile(filepath.Join(folder, candidate.id, "custom-title.json"), 65536, false); err == nil && data != nil {
				var r struct {
					Custom string `json:"customTitle"`
				}
				if json.Unmarshal(data, &r) == nil && r.Custom != "" {
					custom = r.Custom
				}
			}
			if custom == "" {
				custom = generated
			}
			if err == nil && custom != "" && strings.EqualFold(strings.TrimSpace(custom), strings.TrimSpace(title)) {
				result = append(result, candidate.path)
			}
		}
	}
	return result
}

func checkSession(p store.Profile, args []string, cwd string, env map[string]string, errOut io.Writer) error {
	options := claudeOptions(args)
	var continued, printMode, fork, uncheckable bool
	var resume *option
	for i := range options {
		opt := &options[i]
		switch opt.name {
		case "-p", "--print":
			printMode = true
		case "-c", "--continue":
			continued = true
		case "--fork-session":
			fork = true
		case "--teleport", "--cloud", "--remote", "--from-pr":
			uncheckable = true
		case "-r", "--resume":
			resume = opt
		}
	}
	c := newSessionContext(p.ConfigDirectory, cwd, env)
	if continued && strings.ContainsFunc(c.cwd, func(r rune) bool { return r > 127 }) {
		return invalid(p.Name + ": this port cannot normalize a Unicode session directory; resume by session ID or use --allow-cross-provider-resume")
	}
	var paths []string
	switch {
	case uncheckable:
	case continued:
		if path := c.latest(!printMode, fork); path != "" {
			paths = []string{path}
		}
	case resume != nil:
		value := ""
		if resume.value != nil {
			value = strings.TrimSpace(*resume.value)
		}
		switch {
		case value == "":
			uncheckable = !printMode
		case sessionIDShape.MatchString(value):
			paths = c.resumed(value)
		case strings.HasSuffix(strings.ToLower(value), ".jsonl") && (printMode || filepath.IsAbs(value)):
			if !filepath.IsAbs(value) {
				value = filepath.Join(cwd, value)
			}
			if _, err := os.Lstat(value); err == nil {
				paths = []string{value}
			}
		case printMode && strings.Contains(value, "://"):
			uncheckable = true
		default:
			paths = c.titled(value)
			if len(paths) != 1 {
				paths = nil
				uncheckable = !printMode
			}
		}
	}
	if uncheckable {
		fmt.Fprintf(errOut, "claudock: %s: a picker or remote session cannot be checked for cross-provider replies.\n", p.Name)
	}
	for _, path := range paths {
		companions, err := sessionCompanions(path)
		if err != nil {
			return invalid(p.Name + ": cannot check the resumed session's subagents")
		}
		for _, transcript := range append([]string{path}, companions...) {
			foreign, err := otherModels(transcript, p.EndpointModel)
			if err != nil {
				return invalid(p.Name + ": cannot read the resumed session to check its models")
			}
			if foreign {
				return invalid(p.Name + ": the resumed session has replies from other models; start a new session or use --allow-cross-provider-resume")
			}
		}
	}
	return nil
}

func otherModels(path, pinned string) (bool, error) {
	plain := func(s string) string {
		if strings.HasSuffix(strings.ToLower(s), "[1m]") {
			return s[:len(s)-4]
		}
		return s
	}
	foreign := false
	err := transcriptLines(path, func(line []byte) bool {
		if !bytes.Contains(line, []byte("\"assistant\"")) {
			return true
		}
		var r struct {
			Type      string
			Requested string `json:"requestedModel"`
			Message   struct{ Model string }
		}
		if json.Unmarshal(line, &r) == nil && r.Type == "assistant" {
			model := r.Message.Model
			if model == "" {
				model = r.Requested
			}
			if model != "" && model != "<synthetic>" && plain(r.Message.Model) != plain(pinned) && plain(r.Requested) != plain(pinned) {
				foreign = true
				return false
			}
		}
		return true
	})
	return foreign, err
}

func sessionCompanions(path string) ([]string, error) {
	root := filepath.Join(filepath.Dir(path), strings.TrimSuffix(filepath.Base(path), ".jsonl"), "subagents")
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		return nil, nil
	}
	var err error
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	var result []string
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".jsonl") {
			result = append(result, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, child := range append([]string{}, result...) {
		err := transcriptLines(child, func(line []byte) bool {
			var r struct {
				Type   string
				Parent string `json:"parentSessionId"`
			}
			if json.Unmarshal(line, &r) == nil && r.Type == "fork-context-ref" && sessionIDShape.MatchString(r.Parent) {
				result = append(result, filepath.Join(filepath.Dir(path), r.Parent+".jsonl"))
			}
			return true
		})
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}
