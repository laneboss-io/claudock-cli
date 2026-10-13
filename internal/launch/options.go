package launch

import "strings"

type option struct {
	name    string
	value   *string
	indices []int
}

// Claude Code 2.1.296's Commander option arities. Required values consume even
// a leading dash, optional values do not, and short option clusters are expanded.
var optionArities = func() map[string]byte {
	table := make(map[string]byte)
	rows := []struct {
		arity byte
		names string
	}{
		{'f', "--no-session-persistence -h --help --verbose -p --print --bare --safe-mode --init --init-only --maintenance --include-hook-events --include-partial-messages --forward-subagent-text --session-mirror --await-claim --await-initialize --dangerously-skip-permissions --allow-dangerously-skip-permissions --replay-user-messages --enable-auth-status --restricted --exclude-dynamic-system-prompt-sections -c --continue --fork-session --deep-link-origin --reply-on-resume --ide --desktop --strict-mcp-config --disable-slash-commands --chrome --no-chrome --tmux --enable-auto-mode --bg --background --brief --ax-screen-reader --plan-mode-required -v --version"},
		{'o', "-d --debug --prompt-suggestions -r --resume --from-pr -w --worktree --teleport --cloud --remote --remote-control --rc"},
		{'v', "--allowedTools --allowed-tools --tools --disallowedTools --disallowed-tools --mcp-config --betas --add-dir --file --channels --dangerously-load-development-channels"},
		{'r', "--debug-file --output-format --json-schema --input-format --thinking --thinking-display --max-thinking-tokens --max-turns --max-budget-usd --task-budget --permission-prompt-tool --permission-prompts --system-prompt --system-prompt-file --append-system-prompt --append-system-prompt-file --system-prompt-snapshot --append-subagent-system-prompt --append-subagent-system-prompt-file --plan-mode-instructions --permission-mode --inherit-permission-mode --watch-artifact --watch-artifact-no-autoreact --prefill --deep-link-repo --deep-link-last-fetch --prefill-b64 --deep-link-cwd-b64 --resume-session-at --resume-drops-turn --rewind-files --model --effort --agent --fallback-model --workload --settings --client-data-url --managed-settings --project-config-root --session-id -n --name --agents --setting-sources --plugin-dir --plugin-dir-no-mcp --plugin-url --advisor --autocompact --proactivity --messaging-socket-path --agent-id --agent-name --team-name --agent-color --parent-session-id --teammate-mode --agent-type --sdk-url --forward-home-settings --attach-serve --environment --pool --correlation-id --ref --on-branch --remote-control-session-name-prefix"},
	}
	for _, row := range rows {
		for _, name := range strings.Fields(row.names) {
			table[name] = row.arity
		}
	}
	return table
}()

func claudeOptions(args []string) []option {
	type argument struct {
		text  string
		index int
	}
	pending := make([]argument, len(args))
	for i, text := range args {
		pending[i] = argument{text, i}
	}
	maybeOption := func(text string) bool { return len(text) > 1 && text[0] == '-' }
	var result []option
	variadic := false
	for cursor := 0; cursor < len(pending); {
		arg := pending[cursor]
		cursor++
		if arg.text == "--" {
			break
		}
		if variadic && !maybeOption(arg.text) {
			continue
		}
		variadic = false
		name := arg.text
		var attached *string
		if strings.HasPrefix(name, "--") {
			if before, after, ok := strings.Cut(name, "="); ok && optionArities[before] != 'f' {
				name, attached = before, &after
			}
		} else if len(name) > 2 && name[0] == '-' {
			short := name[:2]
			if arity := optionArities[short]; arity != 0 {
				rest := name[2:]
				if arity == 'f' {
					pending = append(pending, argument{})
					copy(pending[cursor+1:], pending[cursor:])
					pending[cursor] = argument{"-" + rest, arg.index}
				} else {
					attached = &rest
				}
				name = short
			}
		}
		arity := optionArities[name]
		if arity == 0 {
			continue
		}
		found := option{name: name, value: attached, indices: []int{arg.index}}
		if attached == nil && arity != 'f' && cursor < len(pending) &&
			(arity != 'o' || !maybeOption(pending[cursor].text)) {
			value := pending[cursor].text
			found.value = &value
			found.indices = append(found.indices, pending[cursor].index)
			cursor++
		}
		variadic = arity == 'v'
		result = append(result, found)
	}
	return result
}
