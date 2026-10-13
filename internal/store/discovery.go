package store

import (
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const unresolvedNote = "Config directory could not be resolved safely; use a literal CLAUDE_CONFIG_DIR or _claude-native path."
const defaultOverrideNote = "The shell overrides the default config or credential directory; import this account with an explicit config folder."

type shellReader struct {
	home         string
	functions    map[string]Profile
	aliases      map[string]Profile
	active       map[string]bool
	count        int
	bytes        int
	defaultIssue string
}

func discoverProfiles() ([]Profile, error) {
	home, err := homeDirectory()
	if err != nil {
		return nil, err
	}
	r := shellReader{home: home, functions: map[string]Profile{}, aliases: map[string]Profile{}, active: map[string]bool{}}
	for _, name := range []string{".zshenv", ".zprofile", ".zshrc"} {
		r.read(filepath.Join(home, name), 0)
	}
	for name, p := range r.aliases {
		r.functions[name] = p
	}
	path := filepath.Join(home, ".claude")
	if r.defaultIssue != "" {
		path = ""
	}
	r.functions["claude"] = Profile{Name: "default", Command: "claude", ConfigDirectory: path, Kind: Subscription, DiscoveryNote: r.defaultIssue}
	var profiles []Profile
	for _, p := range r.functions {
		if p.Command != "claude" && p.DiscoveryNote == "" && !p.IsVertex {
			if err := validateSettings(p.ConfigDirectory); err != nil {
				continue
			}
		}
		id, err := newID()
		if err != nil {
			return nil, err
		}
		p.RegistryID = id
		profiles = append(profiles, p)
	}
	sortProfiles(profiles)
	return profiles, nil
}

var sensitiveName = regexp.MustCompile(`(^|[-_.])(keys?|secrets?|credentials)([-_.]|$)`)

func sensitiveSource(path string) bool {
	for _, part := range strings.Split(filepath.Clean(path), string(filepath.Separator)) {
		name := strings.ToLower(part)
		if name == ".env" || strings.HasPrefix(name, ".env.") || strings.HasPrefix(name, ".claude") || name == ".codex" || name == ".aws" || strings.Contains(name, "private-env") || strings.Contains(name, "private_env") || sensitiveName.MatchString(name) {
			return true
		}
	}
	return false
}

func (r *shellReader) read(path string, depth int) {
	if depth > 12 || r.count >= 64 || r.bytes >= maximumRegistryBytes || sensitiveSource(path) {
		return
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || sensitiveSource(resolved) || r.active[resolved] {
		return
	}
	data, err := readFile(resolved, 524288, false)
	if err != nil || !utf8.Valid(data) {
		return
	}
	r.count++
	r.bytes += len(data)
	r.active[resolved] = true
	defer delete(r.active, resolved)
	tokens := shellTokens(string(data))
	for index := 0; index < len(tokens); {
		exportArgument := index > 0 && tokens[index-1] == "export"
		if (commandPosition(tokens, index) || exportArgument) && (strings.HasPrefix(tokens[index], "CLAUDE_CONFIG_DIR=") || strings.HasPrefix(tokens[index], "CLAUDE_SECURESTORAGE_CONFIG_DIR=")) {
			r.defaultIssue = defaultOverrideNote
		}
		if !commandPosition(tokens, index) {
			index++
			continue
		}
		name, body, end, ok := functionDeclaration(tokens, index)
		if ok {
			if profileCommand(name) {
				r.functions[name] = r.profile(name, body)
			} else if name == "claude" {
				for _, token := range body {
					if strings.HasPrefix(token, "CLAUDE_CONFIG_DIR=") || strings.HasPrefix(token, "CLAUDE_SECURESTORAGE_CONFIG_DIR=") || token == "_claude-native" {
						r.defaultIssue = defaultOverrideNote
					}
				}
			}
			index = end
			continue
		}
		token := tokens[index]
		if token == "alias" {
			next := index + 1
			for next < len(tokens) && !separator(tokens[next]) {
				name, raw, found := strings.Cut(tokens[next], "=")
				if found && profileCommand(name) {
					if body, ok := shellLiteral(raw, r.home); ok {
						r.aliases[name] = r.profile(name, shellTokens(body))
					} else {
						r.aliases[name] = unresolved(name, false)
					}
				}
				next++
			}
			index = next
			continue
		}
		if token == "unalias" || token == "unfunction" || token == "unset" {
			next := index + 1
			functions := token == "unfunction" || (token == "unset" && next < len(tokens) && tokens[next] == "-f")
			for next < len(tokens) && !separator(tokens[next]) {
				if token == "unalias" {
					delete(r.aliases, tokens[next])
				}
				if functions {
					delete(r.functions, tokens[next])
				}
				next++
			}
			index = next
			continue
		}
		if (token == "source" || token == ".") && index+1 < len(tokens) {
			if target, ok := shellLiteral(tokens[index+1], r.home); ok && target != "" {
				if !filepath.IsAbs(target) {
					target = filepath.Join(filepath.Dir(resolved), target)
				}
				r.read(target, depth+1)
			}
			index += 2
			continue
		}
		index++
	}
}

func profileCommand(name string) bool {
	return strings.HasPrefix(name, "claude-") && len(name) > 7 && selectorPattern.MatchString(name)
}
func unresolved(name string, external bool) Profile {
	return Profile{Name: profileName(name), Command: name, Kind: Subscription, IsVertex: external, DiscoveryNote: unresolvedNote}
}

func (r *shellReader) profile(name string, body []string) Profile {
	paths := map[string]bool{}
	dynamic, external := false, false
	for index, token := range body {
		for _, flag := range []string{"CLAUDE_CODE_USE_VERTEX=", "CLAUDE_CODE_USE_BEDROCK=", "CLAUDE_CODE_USE_FOUNDRY="} {
			if strings.HasPrefix(token, flag) && assignmentPosition(body, index) {
				value, ok := shellLiteral(strings.TrimPrefix(token, flag), r.home)
				if ok && (strings.EqualFold(value, "true") || value == "1") {
					external = true
				}
			}
		}
	}
	for index, token := range body {
		if strings.HasPrefix(token, "CLAUDE_SECURESTORAGE_CONFIG_DIR=") {
			p := unresolved(name, external)
			p.DiscoveryNote = "Wrapper overrides separate credential storage; its config directory cannot be resolved safely."
			return p
		}
		raw, found := "", false
		if strings.HasPrefix(token, "CLAUDE_CONFIG_DIR=") && assignmentPosition(body, index) {
			raw, found = strings.TrimPrefix(token, "CLAUDE_CONFIG_DIR="), true
		}
		if token == "_claude-native" && commandPosition(body, index) {
			found = true
			if index+1 < len(body) {
				raw = body[index+1]
			}
		}
		if found {
			value, ok := shellLiteral(raw, r.home)
			if ok && validPath(value) {
				paths[value] = true
			} else {
				dynamic = true
			}
		}
	}
	if dynamic || len(paths) != 1 {
		return unresolved(name, external)
	}
	for path := range paths {
		return Profile{Name: profileName(name), Command: name, ConfigDirectory: path, Kind: Subscription, IsVertex: external}
	}
	return unresolved(name, external)
}

func separator(token string) bool {
	return contains([]string{"\n", ";", "&&", "||", "|", "&", "}", ")"}, token)
}
func commandPosition(tokens []string, index int) bool {
	return index == 0 || separator(tokens[index-1]) || contains([]string{"{", "(", "then", "else", "do"}, tokens[index-1])
}

var assignmentPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

func assignmentPosition(tokens []string, index int) bool {
	start := index
	for start > 0 && !commandPosition(tokens, start) {
		start--
	}
	if start == index || tokens[start] == "export" || tokens[start] == "env" {
		return true
	}
	for _, token := range tokens[start:index] {
		if !assignmentPattern.MatchString(token) {
			return false
		}
	}
	return true
}

func functionDeclaration(tokens []string, start int) (string, []string, int, bool) {
	index := start
	keyword := tokens[index] == "function"
	if keyword {
		index++
	}
	if index >= len(tokens) || !selectorPattern.MatchString(tokens[index]) {
		return "", nil, start, false
	}
	name := tokens[index]
	index++
	if index+1 < len(tokens) && tokens[index] == "(" && tokens[index+1] == ")" {
		index += 2
	} else if !keyword {
		return "", nil, start, false
	}
	for index < len(tokens) && tokens[index] == "\n" {
		index++
	}
	if index >= len(tokens) || (tokens[index] != "{" && tokens[index] != "(") {
		return "", nil, start, false
	}
	opening, closing := tokens[index], "}"
	if opening == "(" {
		closing = ")"
	}
	bodyStart, depth := index+1, 1
	index++
	for index < len(tokens) {
		if tokens[index] == opening {
			depth++
		}
		if tokens[index] == closing {
			depth--
		}
		if depth == 0 {
			return name, tokens[bodyStart:index], index + 1, true
		}
		index++
	}
	return name, nil, len(tokens), true
}

func shellLiteral(raw, home string) (string, bool) {
	characters := []rune(raw)
	var result strings.Builder
	var quote rune
	for index := 0; index < len(characters); index++ {
		c := characters[index]
		if c == '\'' && quote != '"' {
			if quote == '\'' {
				quote = 0
			} else {
				quote = '\''
			}
			continue
		}
		if c == '"' && quote != '\'' {
			if quote == '"' {
				quote = 0
			} else {
				quote = '"'
			}
			continue
		}
		if c == '\\' && quote != '\'' {
			index++
			if index >= len(characters) {
				return "", false
			}
			if quote == '"' && !strings.ContainsRune("$`\"\\\n", characters[index]) {
				result.WriteRune('\\')
			}
			if characters[index] != '\n' {
				result.WriteRune(characters[index])
			}
			continue
		}
		if c == '$' && quote != '\'' {
			remainder := string(characters[index:])
			if strings.HasPrefix(remainder, "${HOME}") {
				result.WriteString(home)
				index += 6
				continue
			}
			if strings.HasPrefix(remainder, "$HOME") && (index+5 == len(characters) || (!unicode.IsLetter(characters[index+5]) && !unicode.IsDigit(characters[index+5]) && characters[index+5] != '_')) {
				result.WriteString(home)
				index += 4
				continue
			}
			return "", false
		}
		if c == '`' && quote != '\'' {
			return "", false
		}
		if quote == 0 && strings.ContainsRune("*?[]", c) {
			return "", false
		}
		if c == '~' && index == 0 && quote == 0 {
			if len(characters) > 1 && characters[1] != '/' {
				return "", false
			}
			result.WriteString(home)
		} else {
			result.WriteRune(c)
		}
	}
	return result.String(), quote == 0
}

type heredoc struct {
	delimiter string
	stripTabs bool
}

func shellTokens(source string) []string {
	characters := []rune(source)
	var tokens []string
	var pending []heredoc
	expects, strip := false, false
	for index := 0; index < len(characters); {
		c := characters[index]
		if c == ' ' || c == '\t' || c == '\r' {
			index++
			continue
		}
		if c == '\\' && index+1 < len(characters) && characters[index+1] == '\n' {
			index += 2
			continue
		}
		if c == '#' {
			for index < len(characters) && characters[index] != '\n' {
				index++
			}
			continue
		}
		if c == '\n' {
			tokens = append(tokens, "\n")
			index++
			for _, document := range pending {
				for index < len(characters) {
					start := index
					for index < len(characters) && characters[index] != '\n' {
						index++
					}
					line := string(characters[start:index])
					if document.stripTabs {
						line = strings.TrimLeft(line, "\t")
					}
					if index < len(characters) {
						index++
					}
					if line == document.delimiter {
						break
					}
				}
			}
			pending = nil
			continue
		}
		if strings.ContainsRune("{}();&|<>", c) {
			symbol := string(c)
			index++
			if index < len(characters) && characters[index] == c && strings.ContainsRune("&|<>", c) {
				symbol += string(c)
				index++
			}
			if symbol == "<<" && index < len(characters) && characters[index] == '<' {
				symbol = "<<<"
				index++
			}
			if symbol == "<<" && index < len(characters) && characters[index] == '-' {
				symbol = "<<-"
				index++
			}
			if symbol == "<<" || symbol == "<<-" {
				expects, strip = true, symbol == "<<-"
			}
			tokens = append(tokens, symbol)
			continue
		}
		start := index
		var quote rune
		var closers []rune
		for index < len(characters) {
			current := characters[index]
			if current == '\\' && quote != '\'' {
				index += 2
				if index > len(characters) {
					index = len(characters)
				}
				continue
			}
			if current == '\'' && quote != '"' && quote != '`' {
				if quote == '\'' {
					quote = 0
				} else {
					quote = '\''
				}
				index++
				continue
			}
			if current == '"' && quote != '\'' && quote != '`' {
				if quote == '"' {
					quote = 0
				} else {
					quote = '"'
				}
				index++
				continue
			}
			if current == '`' && quote != '\'' {
				if quote == '`' {
					quote = 0
				} else {
					quote = '`'
				}
				index++
				continue
			}
			if current == '$' && quote != '\'' && index+1 < len(characters) && (characters[index+1] == '(' || characters[index+1] == '{') {
				closer := ')'
				if characters[index+1] == '{' {
					closer = '}'
				}
				closers = append(closers, closer)
				index += 2
				continue
			}
			if len(closers) > 0 && current == closers[len(closers)-1] {
				closers = closers[:len(closers)-1]
				index++
				continue
			}
			if quote == 0 && len(closers) == 0 && (unicode.IsSpace(current) || strings.ContainsRune("{}();&|<>", current)) {
				break
			}
			index++
		}
		token := string(characters[start:index])
		tokens = append(tokens, token)
		if expects {
			delimiter := strings.NewReplacer("'", "", "\"", "", "\\", "").Replace(token)
			pending = append(pending, heredoc{delimiter: delimiter, stripTabs: strip})
			expects = false
		}
	}
	return tokens
}
