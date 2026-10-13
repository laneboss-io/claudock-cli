# Profile store and command integration

`internal/store` implements the original `List`, `Get`, `Secret`, `Profile` and `Kind` contract. Command
implementations should use those functions, and should never log a credential returned by `Secret`. `Profile` retains
the original flat `EndpointURL` and `EndpointModel` fields and adds registry identity, discovery metadata
and the Swift endpoint configuration, including `behavesAs`.

`internal/profilecmd.Execute` receives the arguments after `profile` and the input/output streams and
returns an exit status. The main command dispatches its `profile` branch to that function. The other
commands have their own dispatch branches. Login and setup-token use the CLI found in PATH;
they do not inject a saved key or inference token. API-key-to-Console transitions require a successful
child exit and a stored Console sign-in before the registry changes.

## Registry formats

The registry directory defaults to `$HOME/Library/Application Support/Claudock` on macOS and Linux.
`CLAUDOCK_REGISTRY` selects an explicit registry file; `CLAUDOCK_REGISTRY_DIR` selects its directory when
the file override is absent. These overrides allow isolated tests and explicit migrations. Reads share
`.registry-lock`; mutations take the same lock exclusively and wait at most five seconds.

`profiles.json` is the Swift format, with `version`, `profiles`, `suppressedCommands`,
`suppressedDirectories` and `subscriptionOnlyMigration`. Each record stores `command`,
`configDirectory`, `isVertex`, `managed`, `registryID`, optional `discoveryNote`, optional `authKind`
and optional `endpoint`. Subscription records omit `authKind`. Console records require registry version
2; endpoint records require version 3. The default placeholder is present exactly once. The UUID,
literal config path and credential service stay unchanged on rename. Removal writes tombstones and
never deletes account data, credentials or shell files. Unknown registry and profile fields survive
mutations.

Codex profiles use `codex-profiles.json` in the same directory, with the same record shape, version 1,
`authKind: "codex"` and `codex-NAME` selectors. `List` appends the Codex records to the Claude registry
order; `Get` searches both registries. A Codex profile gets `accounts/UUID/codex`, or the existing literal
directory supplied with `--directory`. The current Swift enum cannot decode `codex`, so those records
stay in this companion registry. Sign-in launches `codex login` with that directory as CODEX_HOME.

New Claude account folders contain links to the default Claude workspace's sessions, history,
settings and the allowlisted shared work files. Account authentication files are never linked. Explicit
directories remain untouched. Failed adds remove only unchanged entries created by that attempt;
nonempty or replaced directories are retained. A registry publication followed by a directory-sync
failure reports the failure and preserves the already-published record's dependencies.

Shell discovery reads `.zshenv`, `.zprofile`, `.zshrc` and bounded literal includes. It parses functions,
aliases, `_claude-native` declarations, removals and heredocs without executing a shell. Dynamic paths
remain unresolved. Sensitive source names are excluded. Existing registry records and tombstones take
precedence over imports. External-provider imports are retired without changing their data.

## Credentials

On macOS, the default backend invokes `/usr/bin/security`. Queries select the current user and exact
service. Missing status 44 is distinct from unavailable or malformed credentials. An unavailable
Keychain never triggers a plaintext fallback. Writes use `security -i` with a bounded hex-encoded
command on stdin; the value never enters argv. The item is reread to verify a successful write.

Linux defaults to `credentials.json` in the registry directory. Tests set
`CLAUDOCK_CREDENTIAL_STORE=file:<absolute path>`. That backend has the format
`{"version":1,"items":{"SERVICE":"STORED_VALUE"}}`, with private owned directories, 0600 files,
`.credentials-lock`, bounded reads, duplicate-key validation and atomic replacement. Secret content
belongs only in that store or Claude's existing per-profile credential file.

Credential services match Swift exactly. The default login uses `Claude Code-credentials` and the
default Console sign-in uses `Claude Code`. Other profiles append `-` plus the first eight hexadecimal
digits of SHA-256 of the NFC-normalized literal config path. Claudock's API keys, endpoint keys and
inference tokens use `Claudock-apikey-`, `Claudock-endpointkey-` and `Claudock-inference-`, respectively,
followed by the complete SHA-256 of the OAuth credential service. Renaming a command changes none of
these names. `golang.org/x/text v0.23.0` supplies NFC normalization because the standard library has no
equivalent. It is a pure Go dependency; the port remains compatible with CGO_ENABLED=0.

| Secret item | Returned value |
| --- | --- |
| `oauth`, `accessToken`, `login` | Claude OAuth access token |
| `refreshToken` | OAuth refresh token, or `ErrNoSecret` |
| `credentials` | Complete OAuth JSON, for a refresh that preserves other fields |
| `apiKey` | The managed API-key profile's Console API key |
| `endpointKey` | The endpoint profile's provider key |
| `inferenceToken` | The subscription profile's validated, unexpired inference token |
| `inferenceTokenRecord` | Structurally validated inference JSON, including expiry, identity and provenance, for the launch policy to check |
| `consoleKey` | The existing Console sign-in item, when a caller needs its stored value |

`Secret` distinguishes `ErrNoSecret` from unavailable or invalid data. Credential errors contain the
profile and item names without secret values. Unknown item arguments are shown as "unknown" so a
misplaced credential is not echoed in a diagnostic. A missing OAuth item can fall back to that profile's
existing `.credentials.json`. The fallback refuses files belonging to another user, public secret files, links, hardlinks,
devices and oversized input. This fallback does not apply to an inaccessible Keychain item.

`ReplaceCredentials` compares the original OAuth JSON with the current item, preserves unmentioned
root and OAuth fields, and updates the original backend under Claude Code's `.storage-write.lock`
directory lease. An unchanged OAuth record is left in place, preserving the original plaintext file's
inode. It never steals another process's lease. File-store updates also compare under the
credential file lock. The macOS implementation uses security's stdin route and cooperative locking;
it cannot reproduce Swift's native Security persistent-reference checks. Per-profile plaintext writes
use guarded atomic replacement, without Swift's Darwin-only exchange primitive. A process bypassing
the cooperative lock may race a Keychain upsert or plaintext replacement. This is a suspected limitation
from source inspection. Processes that do not use the shared lease require separate coordination.

Inference tokens use Swift's version 2 record: `accessToken`, optional expiration in Unix seconds,
optional account/organization UUID identity and `provenance: "pasted"`. Browser-created version 1/2
records can be read when their saved identity matches local account metadata. Expiry and identity
checks happen before returning an extracted token. Launch reads `inferenceTokenRecord` from the same
namespace and checks identity before expiry, allowing an expired browser token to fall back to the
profile's normal login only when the inference-token requirement is off. An expired pasted token is
refused. `clear-token` deletes only the inference-token namespace.
Stored browser tokens follow Swift's printable-ASCII rule, and UUID identities compare without regard
to hexadecimal letter case.

Console credit is metadata in `api-credit.json`, version 1, keyed by registry UUID. Setting a balance
resets that account's requests and adjustments and uses Swift's ISO 8601 millisecond timestamp format.
Other account entries survive. An unreadable regular ledger is preserved under an unreadable backup
name before replacement. Unsafe ledger files are refused.
Validation includes account balances, timestamps, request costs and adjustment records so malformed
account metadata is preserved in a backup before the ledger is repaired.

## Credential read and write inventory

| Location | Reads | Writes |
| --- | --- | --- |
| `profilecmd.readSecretInput` | Bounded piped input or hidden terminal input for add, set-key and set-token | None |
| `store.fileCredentials` | The selected private credential file; existence checks also read this file into memory | Locked atomic replacement for saving, rollback and deleting a token |
| `store.keychainCredentials` | Exact service lookups; status checks discard output | Bounded stdin writes, readback verification and exact-item deletion |
| `store.rawItem` / `Secret` | Selected item; existing per-profile OAuth file only after a confirmed missing item | None |
| `store.saveItemWithUndo`, `SetKey`, `SetToken` | Prior item for rollback; optional token identity metadata | API/endpoint key or inference-token item; rollback restores the prior item or removes a newly-created item |
| `store.cachedIdentity` / token validation | The profile's `.claude.json`, or the default home's `.claude.json`, for UUID metadata | None |
| `store.ReplaceCredentials` | Original and current OAuth data and source identity checks | Existing OAuth item or original plaintext credential file |
| `store.ConsoleSignedIn` | Keychain attributes; file-backend item presence; Linux/test Claude credential JSON fields `apiKey` or `primaryApiKey` | None |
| `store.CredentialItems`, `CredentialFiles` / remove | Attribute-only Keychain lookups, file-store membership and file metadata | None |
| `profilecmd.launchLogin` | None directly; the launched vendor CLI owns its sign-in flow | Vendor CLI sign-in storage, isolated by CLAUDE_CONFIG_DIR or CODEX_HOME |
| Black-box test sandbox and child fixture | Generated test values and temporary stores only | Temporary file stores and localhost response data; no secret is copied into fixture configuration or logs |

## Deliberate differences

Codex uses the companion registry to keep existing Swift readers working. Linux gets a private file
backend and can recognize a Console key in the known plaintext fields after sign-in. Command text
says "credential store" across platforms. Invalid token records produce an unavailable row, a stderr
diagnostic and exit 1, while Swift displays unavailable with exit 0. `tokens` does not reproduce the
UserDefaults require-token footer; that policy belongs to the launch/shell part of the port. Sorting on
mutations uses deterministic natural ordering instead of the host locale. Config paths reject control
characters rather than sending them to terminal output.

## Known limits

- A pasted inference token can lack an account binding when local account metadata is unavailable.
  Its token shape and expiry are checked, but those checks alone do not prove which account issued it.
  Browser-created tokens require a matching saved account identity.
- Cross-provider resume checks are partial. They inspect readable transcript models and resolved
  session IDs, but cannot verify every interactive picker, remote resume or transcript-resolution
  path. `--allow-cross-provider-resume` bypasses the check explicitly.
- On macOS, an explicit `CLAUDOCK_CREDENTIAL_STORE=file:<absolute path>` selects file credentials
  instead of Keychain. This setting is honored in a normal build and also selects the isolated file
  preference backend. Scope it to the processes that should use that store; a stale inherited override
  changes credential selection.

Endpoint launches write merged settings to a private `0600` file in a `0700` directory and supervise
the child so the file is removed after exit. Other profile launches use direct exec. All launches clear
inherited `OPENAI_*`, `CODEX_*` and gateway authentication settings before selecting profile credentials.
The token requirement is read through the same policy backend as `require-token status`; integer
values are interpreted consistently and invalid policy prevents launch.

The file backend and localhost vendor fixtures cover platform-independent behavior. Native Keychain
integration and interactive terminal job control require platform-specific verification.
