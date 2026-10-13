# Launch integration

`Command` handles `run`, `shell` and `profile swap`, returning whether it handled
the command and its exit status. The command entry point dispatches the other
profile and usage commands separately.

Launches read profiles and credentials through `internal/store`. Subscription,
Console and Codex runs use direct exec, preserving the vendor's terminal,
working directory, arguments, exit status and signals. Endpoint runs retain a
parent process to supervise the vendor and remove private settings after exit.
The child inherits standard input, output, error and working directory.
Termination signals addressed to the parent are forwarded to the child.

| Credential item | Consumer | Returned value |
| --- | --- | --- |
| `apiKey` | Console API-key run | Saved Console API key. |
| `consoleKey` | Console-login run | Claude Code's Console sign-in key, checked before launch. |
| `endpointKey` | Endpoint run | Provider key, excluding Anthropic credentials. |
| `inferenceTokenRecord` | Subscription run | Validated inference JSON with expiry, identity and provenance. |
| `oauth` | Profile swap | Extracted OAuth access token. |

`store.ErrNoSecret` means a missing optional item. Invalid or unavailable storage
is refused. Backend error details are not interpolated into launch diagnostics.
Inference binding and expiry are checked before launch; an expired browser
token can fall back to normal login only when the shared token policy allows it.
Run uses `internal/policy.Required()`, the same backend and integer rules as
`require-token status`, and propagates policy read errors.

All launches clear inherited `OPENAI_*`, `CODEX_*` and Claude gateway settings.
Codex then receives only its selected `CODEX_HOME`. Claude authentication comes
from the chosen profile. Keys and inference tokens enter the child environment;
OAuth tokens enter request authorization headers.

Endpoint settings retain supported user options and enforce provider/model pins.
The merged JSON is written to a `0600` file in a newly created `0700` directory.
Child argv contains only the path. The file is removed after child exit,
including nonzero exit or a terminating signal. Failures before child launch
also remove the file. Settings containing secrets should be supplied by file.

The zsh adapter preserves unrelated shell content and recognizes exact owned
version 1–4 installations, including the earlier version 4 template. Managed
Claude and Codex shortcuts use data-only `shell profile-names` output. Existing
commands and edited installations are preserved.

Profile swap reads fresh quota only for supported subscriptions, excludes the
source and explicit exclusions, and requires a matching current session history
at the destination. It does not copy account data. Normal builds use the fixed
HTTPS usage endpoint and contain no test-endpoint lookup. A `claudock_test`
build requires a loopback HTTP endpoint and explicit file credentials.

## Known limits

Console spend capture, endpoint MDM plist validation and `behavesAs` mapping
remain incomplete. An existing MDM policy causes endpoint launch refusal.
Resume checks cover readable transcripts and resolved session IDs; interactive,
remote and some path-resolution cases remain partial. Pasted inference tokens
can lack an account binding, and an explicit file credential override on macOS
changes backend selection. See [the store limits](../../docs/profile-store.md#known-limits).

## Black-box verification

All registry, credential, vendor and settings inputs are temporary. The CLI
always compiles the real store; optional regression overlays replace affected
production sources with their actual earlier versions. No overlay supplies a
store adapter. Vendor probes compare settings, credentials and environment in
memory and persist only names, paths, arguments and boolean verdicts. Settings
canaries must never appear in child arguments or diagnostic output.

Run formatting, vet, builds and the complete test suite on Linux. The security
negative-control mode and its targeted failure conditions are described in
[the test documentation](../../docs/profile-tests.md#security-regressions).
