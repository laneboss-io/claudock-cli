package usage_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAvailableGoverningLimits(t *testing.T) {
	now := time.Now().UTC()
	future := now.Add(time.Hour).Truncate(time.Second)
	past := now.Add(-time.Hour).Truncate(time.Second)
	rows := []struct {
		name                   string
		windows                []map[string]any
		left, tightest, resets string
		code                   int
	}{
		{"tightest-main-limit", []map[string]any{cachedWindow("five_hour", "5-hour session", 23, &future), cachedWindow("seven_day", "Weekly · all models", 71, nil)}, "29.00%", "Weekly · all models", "unknown", 0},
		{"expired-limit-counts-empty", []map[string]any{cachedWindow("five_hour", "5-hour session", 100, &past), cachedWindow("seven_day", "Weekly · all models", 24, &future)}, "76.00%", "Weekly · all models", future.Format(time.RFC3339), 0},
		{"other-model-full-does-not-fill-account", []map[string]any{cachedWindow("five_hour", "5-hour session", 9, nil), cachedWindow("seven_day_opus", "Weekly · Opus", 100, &future)}, "91.00%", "5-hour session", "unknown", 0},
		{"fable-governs", []map[string]any{cachedWindow("session-0", "5-hour session", 11, nil), cachedWindow("weekly_scoped-1", "Weekly · Fable 5", 93, &future)}, "7.00%", "Weekly · Fable 5", future.Format(time.RFC3339), 0},
		{"highest-fable-governs", []map[string]any{cachedWindow("session-0", "5-hour session", 8, nil), cachedWindow("weekly_scoped-1", "Weekly · Fable5", 60, nil), cachedWindow("weekly_scoped-2", "Weekly · Fable 5.1", 82, &future)}, "18.00%", "Weekly · Fable 5.1", future.Format(time.RFC3339), 0},
		{"only-unknown-limits-all-govern", []map[string]any{cachedWindow("limit-0", "New model", 50, nil), cachedWindow("limit-1", "New surface", 78, nil)}, "22.00%", "New surface", "unknown", 0},
		{"near-limit-still-listed", []map[string]any{cachedWindow("five_hour", "5-hour session", 99.99, nil)}, "0.01%", "5-hour session", "unknown", 0},
		{"full-not-listed", []map[string]any{cachedWindow("five_hour", "5-hour session", 100, &future)}, "", "", "", 1},
		{"unknown-full-not-listed", []map[string]any{cachedWindow("limit-0", "New model", 103, nil)}, "", "", "", 1},
	}
	for _, test := range rows {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			p := f.add("alpha", "subscription", "pro", "")
			f.cache(map[string]any{p.service(): cacheEntry(p, now.Add(-time.Minute), test.windows...)})
			writeFile(t, f.store, []byte("cache-only row must not read credentials"))
			want, reason := availableHeader, ""
			if test.code == 0 {
				want += "alpha\tsubscription\tPro\t" + test.left + "\t" + test.tightest + "\t" + test.resets + "\n"
			} else {
				reason = "No profile has usage left."
			}
			expect(t, f.run("available"), test.code, want, reason)
			if f.requestCount() != 0 {
				t.Error("available requested a profile whose reading is current")
			}
		})
	}
}

func TestAvailableNamesAndOrder(t *testing.T) {
	for _, options := range [][]string{{"--names"}, {"--names", "--max-age", "86400"}, {"--max-age", "86400", "--names"}, {"--names", "--fresh"}, {"--fresh", "--names"}} {
		t.Run(strings.Join(options, "-"), func(t *testing.T) {
			f := newFixture(t)
			entries := make(map[string]any)
			for _, item := range []struct {
				name    string
				percent float64
			}{{"zebra", 35}, {"alpha", 35}, {"near", 99}, {"full", 100}, {"best", 7}} {
				p := f.add(item.name, "subscription", "pro", "")
				entries[p.service()] = cacheEntry(p, time.Now().Add(-time.Minute), cachedWindow("five_hour", "5-hour session", item.percent, nil))
				f.reply(item.name, apiReply{status: 200, body: fmt.Sprintf(`{"five_hour":{"utilization":%g}}`, item.percent)})
			}
			f.cache(entries)
			expect(t, f.run(append([]string{"available"}, options...)...), 0, "best\nalpha\nzebra\nnear\n", "")
			wantRequests := 0
			for _, option := range options {
				if option == "--fresh" {
					wantRequests = 5
				}
			}
			if f.requestCount() != wantRequests {
				t.Error("available names did not use the selected freshness policy")
			}
		})
	}
}

func TestAvailableNextFreeUsesAllFullResets(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UTC()
	soon, later, latest := now.Add(time.Hour).Truncate(time.Second), now.Add(2*time.Hour).Truncate(time.Second), now.Add(3*time.Hour).Truncate(time.Second)
	a := f.add("two-limits", "subscription", "pro", "")
	b := f.add("next", "subscription", "pro", "")
	c := f.add("unknown-reset", "subscription", "pro", "")
	f.cache(map[string]any{
		a.service(): cacheEntry(a, now.Add(-time.Minute), cachedWindow("five_hour", "5-hour session", 100, &soon), cachedWindow("seven_day", "Weekly · all models", 100, &latest)),
		b.service(): cacheEntry(b, now.Add(-time.Minute), cachedWindow("five_hour", "5-hour session", 100, &later)),
		c.service(): cacheEntry(c, now.Add(-time.Minute), cachedWindow("five_hour", "5-hour session", 100, nil)),
	})
	expect(t, f.run("available", "--names"), 1, "", "No profile has usage left. next is next free, at "+later.Format(time.RFC3339)+".")
	if f.requestCount() != 0 {
		t.Error("current full readings caused a request")
	}
}

func TestAvailableStaleReadings(t *testing.T) {
	rows := []struct {
		name    string
		status  int
		expired bool
		listed  bool
	}{
		{"server-failure-retains-room", 500, false, true},
		{"rate-limit-retains-room", 429, false, true},
		{"unauthorized-needs-signin", 401, false, false},
		{"forbidden-needs-signin", 403, false, false},
		{"expired-needs-signin", 200, true, false},
	}
	for _, test := range rows {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			p := f.add("alpha", "subscription", "pro", "")
			f.cache(map[string]any{p.service(): cacheEntry(p, time.Now().Add(-15*time.Minute), cachedWindow("five_hour", "5-hour session", 20, nil))})
			if test.expired {
				f.secrets[p.service()] = `{"claudeAiOauth":{"accessToken":"expired-fixture","expiresAt":1}}`
				f.publish()
			}
			f.reply("alpha", apiReply{status: test.status, body: `{}`})
			code, want := 1, ""
			if test.listed {
				code, want = 0, "alpha\n"
			}
			expect(t, f.run("available", "--names", "--fresh"), code, want, "using reading from 15 min ago")
			count := 1
			if test.expired {
				count = 0
			}
			if f.requestCount() != count {
				t.Error("stale availability used the wrong credential or request policy")
			}
		})
	}
}

func TestAvailableWithoutReading(t *testing.T) {
	f := newFixture(t)
	f.add("alpha", "subscription", "pro", "")
	f.reply("alpha", apiReply{status: 403, body: `{}`})
	expect(t, f.run("available", "--names", "--fresh"), 1, "", "alpha: not listed; This credential cannot read usage")
	if f.requestCount() != 1 {
		t.Error("unreadable account did not make the configured quota request")
	}
}

func TestAvailableCacheAges(t *testing.T) {
	for _, test := range []struct {
		name     string
		age      time.Duration
		options  []string
		want     string
		requests int
	}{
		{"default-reuses", time.Minute, nil, "alpha\n", 0},
		{"default-refreshes", time.Hour, nil, "", 1},
		{"large-max-age-reuses", time.Hour, []string{"--max-age", "86400"}, "alpha\n", 0},
		{"fresh-refreshes", time.Minute, []string{"--fresh"}, "", 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			p := f.add("alpha", "subscription", "pro", "")
			f.cache(map[string]any{p.service(): cacheEntry(p, time.Now().Add(-test.age), cachedWindow("five_hour", "5-hour session", 10, nil))})
			f.reply("alpha", apiReply{status: 200, body: `{"five_hour":{"utilization":100}}`})
			code, diagnostic := 0, ""
			if test.requests > 0 {
				code, diagnostic = 1, "No profile has usage left."
			}
			expect(t, f.run(append([]string{"available", "--names"}, test.options...)...), code, test.want, diagnostic)
			if f.requestCount() != test.requests {
				t.Error("availability did not apply its requested cache age")
			}
		})
	}
}

func TestUntrackedProfiles(t *testing.T) {
	f := newFixture(t)
	f.add("api", "apiKey", "", "")
	f.add("console", "consoleLogin", "", "")
	f.add("endpoint", "endpoint", "", "")
	f.add("codex", "codex", "", "")
	f.add("vertex", "subscription", "", "")
	f.add("import", "subscription", "", "")
	f.add("empty-folder", "subscription", "", "")
	f.profiles[3].Endpoint = map[string]any{"baseURL": "https://example.invalid/v1", "model": "provider-model"}
	f.profiles[5].IsVertex = true
	f.profiles[5].Managed = false
	note := "needs an import"
	f.profiles[6].DiscoveryNote = &note
	f.profiles[6].Managed = false
	f.profiles[7].ConfigDirectory = ""
	f.profiles[7].DiscoveryNote = &note
	f.profiles[7].Managed = false
	f.secrets = map[string]string{}
	f.publish()
	r := f.run("usage")
	expect(t, r, 0, usageHeader, "billed per token by the provider; no quota to read")
	for _, part := range []string{"api: skipped; Console API key", "console: skipped; Console login", "codex: skipped;", "import: skipped;", "empty-folder: skipped;"} {
		if !strings.Contains(r.err, part) {
			t.Error("usage omitted the appropriate untracked-profile note")
		}
	}
	// The real registry retires Vertex imports before listing any profiles.
	// Verify its tombstones, so an absent diagnostic cannot hide a skipped row.
	var saved struct {
		Profiles              []testProfile `json:"profiles"`
		SuppressedCommands    []string      `json:"suppressedCommands"`
		SuppressedDirectories []string      `json:"suppressedDirectories"`
	}
	if json.Unmarshal(readFile(t, filepath.Join(f.base, "profiles.json")), &saved) != nil {
		t.Fatal("could not read the real registry after Vertex retirement")
	}
	if strings.Contains(r.err, "vertex:") || !slices.Contains(saved.SuppressedCommands, f.profiles[5].Command) || !slices.Contains(saved.SuppressedDirectories, f.profiles[5].ConfigDirectory) {
		t.Error("the real registry did not retire and suppress the Vertex import")
	}
	for _, p := range saved.Profiles {
		if p.Command == f.profiles[5].Command {
			t.Error("the retired Vertex import remains registered")
		}
	}
	expect(t, f.run("available", "--names"), 1, "", "No profile has usage left.")
	if f.requestCount() != 0 {
		t.Error("an untracked profile requested subscription quota")
	}
}

func account(asOf time.Time, balance any, requests, adjustments []any) map[string]any {
	fullRequests := make([]any, 0, len(requests))
	for index, value := range requests {
		request := map[string]any{"id": fmt.Sprintf("%064x", index+1), "sessionID": fmt.Sprintf("session-%d", index+1), "model": "fixture-model"}
		for key, field := range value.(map[string]any) {
			request[key] = field
		}
		fullRequests = append(fullRequests, request)
	}
	fullAdjustments := make([]any, 0, len(adjustments))
	for index, value := range adjustments {
		adjustment := map[string]any{"kind": "session-total", "sessionID": fmt.Sprintf("session-%d", index+1), "lastCostUSD": value.(map[string]any)["amountUSD"], "capturedUSD": 0, "recordedAt": asOf.UTC().Format(time.RFC3339Nano)}
		for key, field := range value.(map[string]any) {
			adjustment[key] = field
		}
		fullAdjustments = append(fullAdjustments, adjustment)
	}
	return map[string]any{"balanceUSD": balance, "asOf": asOf.UTC().Format(time.RFC3339Nano), "requests": fullRequests, "adjustments": fullAdjustments}
}

func (f *fixture) ledger(accounts map[string]any) {
	writeFile(f.t, filepath.Join(f.base, "api-credit.json"), marshal(f.t, map[string]any{"version": 1, "profiles": accounts}))
}

func TestConsoleCredit(t *testing.T) {
	now := time.Now().UTC()
	rows := []struct {
		name                  string
		balance               any
		requests, adjustments []any
		window, percent       string
	}{
		{"count-after-asof", 200, []any{map[string]any{"costUSD": 12.34, "at": now.Add(time.Minute)}, map[string]any{"costUSD": 7.66, "at": now.Add(2 * time.Minute)}, map[string]any{"costUSD": 500, "at": now.Add(-time.Minute)}}, []any{map[string]any{"amountUSD": 5, "sessionStart": now.Add(time.Minute)}, map[string]any{"amountUSD": 500, "sessionStart": now.Add(-time.Minute)}}, "Credit · $175.00 of $200.00 left", "12.50"},
		{"zero-balance", 0, nil, nil, "Credit · $0.00 of $0.00 left", "100.00"},
		{"overspent", 100, []any{map[string]any{"costUSD": 150, "at": now}}, nil, "Credit · $0.00 of $100.00 left", "150.00"},
		{"decimal-half-up", 2000, []any{map[string]any{"costUSD": 200.235, "at": now}, map[string]any{"costUSD": 0.1, "at": now}, map[string]any{"costUSD": 0.2, "at": now}}, nil, "Credit · $1,799.47 of $2,000.00 left", "10.03"},
		{"maximum-grouped", 1_000_000, nil, nil, "Credit · $1,000,000.00 of $1,000,000.00 left", "0.00"},
	}
	for _, test := range rows {
		for _, kind := range []string{"apiKey", "consoleLogin"} {
			t.Run(test.name+"/"+kind, func(t *testing.T) {
				f := newFixture(t)
				p := f.add("renamed-account", kind, "", "")
				f.ledger(map[string]any{p.RegistryID: account(now, test.balance, test.requests, test.adjustments)})
				ledgerFile := filepath.Join(f.base, "api-credit.json")
				before, credentialBefore := readFile(t, ledgerFile), readFile(t, f.store)
				expect(t, f.run("usage", "--fresh"), 0, usageHeader+"renamed-account\tConsole API\t"+test.window+"\t"+test.percent+"\t-\n", "")
				if !bytes.Equal(before, readFile(t, ledgerFile)) || !bytes.Equal(credentialBefore, readFile(t, f.store)) || f.requestCount() != 0 {
					t.Error("credit reading changed the ledger or requested/read account credentials")
				}
			})
		}
	}
}

func TestAvailableCreditSort(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UTC()
	subscription := f.add("subscription-near", "subscription", "pro", "")
	low := f.add("large-low-share", "apiKey", "", "")
	high := f.add("small-high-share", "consoleLogin", "", "")
	full := f.add("no-credit-left", "apiKey", "", "")
	f.ledger(map[string]any{
		low.RegistryID:  account(now, 1000, []any{map[string]any{"costUSD": 900, "at": now}}, nil),
		high.RegistryID: account(now, 5, []any{map[string]any{"costUSD": 1, "at": now}}, nil),
		full.RegistryID: account(now, 50, []any{map[string]any{"costUSD": 50, "at": now}}, nil),
	})
	f.cache(map[string]any{subscription.service(): cacheEntry(subscription, now.Add(-time.Minute), cachedWindow("five_hour", "5-hour session", 99.99, nil))})
	writeFile(t, f.store, []byte("credit and cached quota must not read credentials"))
	want := availableHeader + "subscription-near\tsubscription\tPro\t0.01%\t5-hour session\tunknown\nsmall-high-share\tconsole-login\tConsole API\t$4.00\tCredit\t-\nlarge-low-share\tapi-key\tConsole API\t$100.00\tCredit\t-\n"
	expect(t, f.run("available"), 0, want, "")
	expect(t, f.run("available", "--names"), 0, "subscription-near\nsmall-high-share\nlarge-low-share\n", "")
	if f.requestCount() != 0 {
		t.Error("credit availability performed an API request")
	}
}

func TestCreditRefusesInvalidLedger(t *testing.T) {
	now := time.Now().UTC()
	rows := []struct {
		name string
		data []byte
	}{
		{"invalid-json", []byte("broken ledger")},
		{"unsupported-version", []byte(`{"version":7,"profiles":{}}`)},
		{"negative-balance", marshal(t, map[string]any{"version": 1, "profiles": map[string]any{"id-alpha": account(now, -1, nil, nil)}})},
		{"negative-spend", marshal(t, map[string]any{"version": 1, "profiles": map[string]any{"id-alpha": account(now, 100, []any{map[string]any{"costUSD": -1, "at": now}}, nil)}})},
		{"boolean-balance", marshal(t, map[string]any{"version": 1, "profiles": map[string]any{"id-alpha": account(now, true, nil, nil)}})},
		{"missing-asof", []byte(`{"version":1,"profiles":{"id-alpha":{"balanceUSD":100,"requests":[],"adjustments":[]}}}`)},
	}
	for _, test := range rows {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			f.add("alpha", "apiKey", "", "")
			file := filepath.Join(f.base, "api-credit.json")
			writeFile(t, file, test.data)
			expect(t, f.run("usage"), 1, usageHeader, "Console credit ledger")
			expect(t, f.run("available", "--names"), 1, "", "Console credit ledger")
			if !bytes.Equal(test.data, readFile(t, file)) || f.requestCount() != 0 {
				t.Error("bad credit ledger was rewritten or replaced with an API reading")
			}
		})
	}
}

func TestCreditSymlinkIsRefused(t *testing.T) {
	f := newFixture(t)
	p := f.add("alpha", "apiKey", "", "")
	target := filepath.Join(f.home, "credit-target")
	data := marshal(t, map[string]any{"version": 1, "profiles": map[string]any{p.RegistryID: account(time.Now(), 100, nil, nil)}})
	writeFile(t, target, data)
	if os.Symlink(target, filepath.Join(f.base, "api-credit.json")) != nil {
		t.Fatal("could not create ledger symlink canary")
	}
	expect(t, f.run("usage"), 1, usageHeader, "Console credit ledger")
	if !bytes.Equal(data, readFile(t, target)) || f.requestCount() != 0 {
		t.Error("ledger symlink was followed or changed")
	}
}
