# Real-store integration expectations

Launch and usage black-box builds always compile the production `internal/store`.
Neither suite replaces a store source file. Their optional baseline overlays
replace only the command entry point. The former store adapters have been removed.

All existing test rows remain. Valid fixture registries include UUID identities,
exactly one unmanaged Claude default entry, schema versions 1–3, suppression arrays
and the subscription migration marker. Codex records use `codex-NAME` selectors in
`codex-profiles.json`. Credential fixtures use `{"version":1,"items":{...}}` with
the Swift credential services, complete OAuth envelopes and versioned inference
records. Every runtime command gets a temporary HOME and a file credential backend.
HTTP usage comes from localhost stubs, and launches use temporary vendor probes.
Credential files live in explicit vault directories created with mode `0700`.
Go creates `t.TempDir()` leaves with mode `0777` before applying the umask, so
placing the credential file directly in that directory can cause the real
backend to refuse its parent before it reads any item.

All three black-box suites retain each CLI invocation's exit status, stdout and
stderr and print them during cleanup when a row fails, including failures in
later file, HTTP or vendor assertions. Known temporary credential canaries are
redacted only in those diagnostics; assertions still use the original bytes.
Usage also collects an unfinished child during cleanup when an API-arrival
assertion fails before its usual result collection. No environment values or
command arguments are included in the diagnostic recorder.

The vault and diagnostic corrections change no row expectations. A refused
credential directory maps to the usage client's saved-credential error before
HTTP status, expiry or missing-item handling can run. A valid private parent
allows those existing error paths and assertions to be exercised.

The shell data validator and current adapter accept both Claude and Codex
selectors. Ownership validation also recognizes the exact previous version 4
adapter, so an existing installation can be refreshed or disabled safely.

The expectation changes are:

| Existing row | Changed expectation | Reason |
| --- | --- | --- |
| `TestShellProfileNames` | The second shortcut is `codex-two`, previously `claude-two`. | The real store keeps Codex selectors in the companion registry with the Codex prefix. |
| `TestUsageCredentialFailures/malformed-envelope/cached-false` | The diagnostic contains `saved OAuth credential item`, previously `Sign in`. | The real store rejects malformed stored OAuth JSON before returning the complete credentials item. |
| `TestUsageCredentialFailures/malformed-envelope/cached-true` | The diagnostic contains `saved OAuth credential item`, previously `Sign in`. | The same store validation fails; the existing cached-reading and age assertions remain. |
| `TestUsageCredentialFailures/missing-token/cached-false` | The diagnostic contains `saved OAuth credential item`, previously `Sign in`. | The real store rejects an OAuth envelope without an access token. |
| `TestUsageCredentialFailures/missing-token/cached-true` | The diagnostic contains `saved OAuth credential item`, previously `Sign in`. | The same store validation fails; the existing cached-reading and age assertions remain. |
| `TestUntrackedProfiles` (Vertex entry) | The Vertex import is absent from the listing and recorded in both suppression arrays, previously a `vertex: skipped;` diagnostic. | The real registry retires Vertex imports during its read. The test checks both tombstones, absence of the registered entry and diagnostic, and the existing zero-request assertion. |

No exit-code, quota-table, credential-immutability, request-authentication, vendor
argument, launch-isolation, refusal, shell-ownership or session-history assertion
was relaxed. The default-launch fixture is unmanaged, as required by the registry;
its expected launch behavior is unchanged.

Launch consumes the complete inference record through `inferenceTokenRecord` so
its account binding, pasted-token expiry refusal, browser-token fallback and
required-token behavior remain covered by the original token-state rows. The
ordinary `inferenceToken` item still returns only a validated unexpired token.
Usage requests the complete OAuth `credentials` item rather than the extracted
`oauth` access token. Both callers recognize `store.ErrNoSecret` as a missing item
while refusing malformed or unavailable credential storage.
