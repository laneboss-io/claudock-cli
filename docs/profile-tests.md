# Black-box profile tests

The source inventory in `tests/profiles_test.go` contains 190 scenarios, including parameterized
subtests. Several scenarios exercise
more than one command, particularly token and rename/removal workflows.

Every CLI invocation uses a temporary HOME, explicit temporary registry and
`CLAUDOCK_CREDENTIAL_STORE=file:<temporary path>`. The test package builds `./cmd/claudock` and a child
CLI fixture. The fixture makes real localhost HTTP requests for login and setup-token and reports
only booleans about credential/environment isolation. It never copies a secret into an observation,
fixture configuration, stdout or stderr. Generated credentials are kept in memory and temporary
credential stores. Assertions compare exit codes, output, stored records, permissions, unchanged
files, actual HTTP observations and credential containment.

| Primary command family | Written scenarios | Failure condition |
| --- | ---: | --- |
| profile list | 23 | Wrong Swift schema/kind/rows/version migration, unsafe or malformed registry accepted, read-only rewrite, or busy registry incorrectly succeeding |
| profile add | 55 | Wrong kind/path/service/version, missing workspace, bad input accepted, publication without a stored key, lost concurrent update, wrong NFC identity, settings overrides accepted, or failed Console login discarding the registration |
| profile set-key | 11 | Replacement absent, identity changed, prior Console item removed, wrong kind or invalid input accepted |
| profile set-endpoint | 12 | Wrong retained/pinned fields, key changed, read-only inspection rewrites data, or invalid options accepted |
| profile set-credit | 22 | Wrong balance/timestamp/account, another valid account lost, unreadable bytes discarded, malformed account metadata retained in the repaired ledger, or invalid amount/kind accepted |
| profile set-token / tokens / clear-token | 27 | Wrong token schema/status/expiry, invalid token characters accepted, equivalent UUID spellings rejected, invalid backend accepted, normal login changed during clearing, or bad input accepted |
| profile rename / remove | 15 | UUID or literal directory moved, credentials/data deleted, leftovers omitted, tombstones lost, or protected/ambiguous/invalid selection accepted |
| profile login / setup-token | 20 | No localhost request, wrong child arguments/environment, wrong exit propagation, unconfirmed Console transition, or a refused command still launching |
| profile import-shell | 5 | Literal declaration absent, shell code executed, sensitive/nested/heredoc/external input imported, tombstone ignored, or shell file changed |

The per-command token rows include 18 scenarios containing set-token assertions, 17 containing token
status assertions and eight containing clear-token assertions. Shared workflows account for the
overlap. Rename has 11 scenarios and remove six, with two joint workflows. Login has 15 and
setup-token five.

The initial commit's binary returns exit 70 for these commands. `CLAUDOCK_TEST_SOURCE_DIR`, which is
used only by the tests, can point to a disposable baseline checkout: the harness builds its CLI there
and keeps the candidate test harness and HTTP fixture. The baseline run must actually demonstrate
each row failing because commands are unimplemented, rather than from a compilation or setup error.
Run these checks on Linux with the project's Go toolchain and temporary credentials:

```sh
gofmt -l internal/store internal/profilecmd cmd/claudock tests
go vet ./...
CGO_ENABLED=0 go build ./...
go test -count=1 ./...
```

Linux and Darwin cross-builds with CGO_ENABLED=0 should also be recorded. The test helper is compiled
separately by TestMain because Go excludes testdata directories from ./... package discovery.

The file-backend tests do not access Keychain. Native Keychain integration needs separate macOS
verification. OAuth refresh persistence is available through the store API; the profile-command
inventory above describes command behavior rather than the complete refresh lifecycle.

## Security regressions

`internal/launch/security_test.go` covers a stale swap-endpoint override in a normal build, private
endpoint settings and cleanup after child exit or signal, prefix-based environment isolation, shared
integer token policy, unreadable policy, and public documentation/workspace hygiene. Endpoint
checks compare actual child arguments, file modes, settings canaries and post-exit file removal.
Fixtures print redacted command results on failure without changing the bytes used by assertions.

For a negative control against the implementation before these fixes, run:

```sh
CLAUDOCK_SECURITY_BASELINE=1 CGO_ENABLED=0 go test -count=1 -run '^TestSecurity' ./internal/launch
```

This mode substitutes the actual affected sources from commit `f39e347` in the black-box binary
builds; it retains the real registry and credential backend. Documentation and workspace source
checks inspect the same commit. It must fail for the targeted behavior, rather than a build or fixture
error. Then run `go test -count=1 ./...` with the current sources. The override is read only by the test
harness; normal builds cannot select a regression baseline or a test endpoint.
