# claudock-cli

The Claudock command line in Go: run Claude Code under one of several profiles, read each profile's usage and
pick the one with room left. One static binary for macOS and Linux.

The CLI provides profile management, usage readings, process launch and optional shell integration.
It shares profile and credential formats with the Swift app in
[laneboss-io/claudock](https://github.com/laneboss-io/claudock). Black-box tests exercise the built binary
with isolated registries, file credentials and temporary vendor programs.

The launch implementation provides:

```text
claudock run NAME [--allow-cross-provider-resume] [-- CLAUDE_ARGS...]
claudock shell enable|disable|status|profile-names
claudock profile swap --from NAME (--session ID | --continue)
  [--cwd DIR] [--max-used PCT] [--exclude NAME] [--prefer NAME]
  [--prompt TEXT] [--dry-run] [-- CLAUDE_ARGS...]
```

Run forwards arguments literally. Subscription and Console profiles replace the
process with Claude Code; endpoint runs supervise the child to clean up their
private settings file after it exits, preserving its output and exit status.
Subscription, Console API-key, Console-login, and third-party endpoint profiles
use their own configuration and credential selection. Endpoint profiles pin the
model and refuse a checked resumed session with replies from another model unless
the caller explicitly allows it. A profile whose store kind is `codex` starts
Codex with `CODEX_HOME` set to that profile's directory.

Shell integration is optional zsh integration. It preserves user shell content,
keeps a backup before changing `.zshrc`, recognizes the Swift adapter's ownership
records, and refuses edited, linked, incomplete, or concurrently changed managed
files. Profile creation does not enable integration.

Profile swap currently selects subscription profiles using fresh usage. Every
active window must be strictly below the threshold (85% by default). The previous
profile and explicit exclusions are ignored. The destination must already have
the same current session transcript. `--continue` resolves the previous profile's
session to an explicit ID before launching. Swapping does not copy account data
or opt into Console billing.

Launch and usage tests build against the shared production store. Fixtures use
the Swift registry format, the companion Codex registry, and the versioned file
credential backend with the real service identities. Console spend capture,
MDM plist validation, and endpoint `behavesAs`
mapping remain unported. See [the launch notes](internal/launch/README.md) for
integration details and the verification procedure.

Build static binaries with the pinned Go toolchain and `CGO_ENABLED=0`:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o claudock ./cmd/claudock
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o claudock ./cmd/claudock
```

License: MIT (see [LICENSE](LICENSE)).

The profile commands cover listing, adding subscription/Console/endpoint/Codex profiles, changing keys,
endpoints and Console credit, importing and clearing inference tokens, token status, renaming, removing,
signing in and importing static shell wrappers. `claudock help` lists their syntax.

Claudock shares the Swift app's `profiles.json`, account directories and macOS Keychain item names.
Linux uses a private file credential store. Removing a profile preserves its data and credentials and
names the retained credential items and files. Codex registrations live in a companion file so current
Swift builds can continue reading their own registry.

See [the profile store contract](docs/profile-store.md) for the formats, credential access points,
integration details and differences from Swift. See [the black-box test inventory](docs/profile-tests.md)
for the test coverage and verification commands.

The usage commands are `usage [--max-age SECONDS | --fresh]`,
`available [--names] [--max-age SECONDS | --fresh]`, `require-token on|off|status`,
and `version` (also `--version`).

`usage` prints the Swift CLI's tab-separated `PROFILE`, `PLAN`, `WINDOW`,
`USED_PERCENT`, and `RESETS_UTC` columns. Readings share the app's
`Library/Application Support/Claudock/usage-cache.json` under the selected home.
The default maximum age is 180 seconds; a whole number from 0 to 86400 changes
it, and `--fresh` selects 0. HTTP 429 cooldowns remain in effect with `--fresh`.
When a request fails, an older reading is shown with its age on stderr; a
subscription without any reading makes `usage` exit 1.

`available` sorts subscriptions by room in their tightest governing limit, then
Console profiles by the share of prepaid credit left. Near-limit profiles are
included. Full accounts and accounts that need sign-in are excluded. With
`--names`, stdout contains one profile name per line. No available profile means
exit 1, with the next known reset on stderr. Third-party endpoints are noted as
billed per token and have no quota to read. Codex profiles are untracked here.

Console credit is read from the app's existing `api-credit.json`, by registry ID.
Only requests and session-total adjustments on or after the saved balance time
count. Reading usage does not change the credit ledger or any credential. Like
the Swift CLI, these commands do not rotate OAuth tokens: renewal belongs to the
resident app or Claude Code.

The token requirement shares `requireInferenceToken` in the app's
`io.github.claudeusage.ClaudeUsage` preference domain. Production macOS builds
use the system `defaults` tool to keep CFPreferences coherent. Linux and
isolated file-backend tests use an XML plist at the corresponding
`Library/Preferences` path, preserving other keys. Run and status both use
`internal/policy.Required()`: nonzero integers mean on, and unreadable or invalid
policy stops a subscription launch.

The one additional dependency is the pure-Go `golang.org/x/text/unicode/norm`,
pinned in `go.mod` and `go.sum`. Swift normalizes config paths to NFC before
hashing credential services; Go's standard library has no NFC implementation.
This dependency preserves shared cache identity for Unicode paths. Build the
static binary with `CGO_ENABLED=0 go build ./cmd/claudock`.

Black-box tests are in `tests`, `tests/usage` and `internal/launch`. They build `./cmd/claudock`, run it with
temporary homes and file credential stores, and serve all HTTP responses from
localhost. The `claudock_test` build tag enables `CLAUDOCK_TEST_API_BASE_URL`,
restricted to an HTTP base on literal `127.0.0.1`. A test build without that base
refuses remote calls. The launch tests use the same build tag for their loopback
swap endpoint. Production builds contain no test API override.

The tests always use the real registry and file credential backend. The file
fixtures store complete credential JSON envelopes by Claude credential service
name inside `{"version":1,"items":{...}}`. The usage client requests the
`credentials` item from `store.Secret`; `oauth` returns only an access token.
Missing items return `store.ErrNoSecret`, distinct from invalid or unavailable
items. See [the integration expectation changes](docs/integration-tests.md).

Endpoint settings are merged into a `0600` file inside a private `0700`
directory. Only its path enters child argv, and the files are removed after the
child exits. Supply settings containing secrets through a file rather than
inline command arguments.

Known limits include pasted inference tokens without an account binding,
partial cross-provider resume checks, and the explicit file credential override
on macOS. See [the known limits](docs/profile-store.md#known-limits).

To verify a change on Linux, run `gofmt -l cmd internal tests`, `go vet ./...`,
`CGO_ENABLED=0 go build ./...`, and `go test -count=1 ./...`. The regression
procedures are described in [the test documentation](docs/profile-tests.md).
