package usage_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUsageResponseFormats(t *testing.T) {
	reset := time.Now().UTC().Add(3 * time.Hour).Truncate(time.Second)
	resetText := reset.Format(time.RFC3339)
	rows := []struct {
		name string
		body string
		want string
	}{
		{"legacy", `{"five_hour":{"utilization":12.345,"resets_at":"` + resetText + `"},"seven_day":{"utilization":46},"seven_day_opus":{"utilization":4},"seven_day_sonnet":{"utilization":8},"seven_day_oauth_apps":{"utilization":3},"seven_day_cowork":{"utilization":2}}`, "5-hour session\t12.35\t" + resetText + "\nWeekly · all models\t46.00\tunknown\nWeekly · Opus\t4.00\tunknown\nWeekly · Sonnet\t8.00\tunknown\nWeekly · OAuth apps\t3.00\tunknown\nWeekly · Cowork\t2.00\tunknown\n"},
		{"modern", `{"limits":[{"kind":"weekly_scoped","percent":78.9,"scope":{"model":{"display_name":"Fable 5"}}},{"kind":"session","percent":19,"resets_at":"` + resetText + `"},{"kind":"weekly_all","percent":50},{"kind":"rolling_month","percent":11,"scope":{"surface":{"display_name":"New surface"}}}]}`, "Weekly · Fable 5\t78.90\tunknown\n5-hour session\t19.00\t" + resetText + "\nWeekly · all models\t50.00\tunknown\nNew surface\t11.00\tunknown\n"},
		{"mixed-deduplicated", `{"limits":[{"kind":"session","percent":20},{"kind":"weekly_scoped","percent":22,"scope":{"model":{"display_name":"Opus"}}}],"five_hour":{"utilization":99},"seven_day":{"utilization":31},"seven_day_opus":{"utilization":95},"seven_day_sonnet":{"utilization":17}}`, "5-hour session\t20.00\tunknown\nWeekly · Opus\t22.00\tunknown\nWeekly · all models\t31.00\tunknown\nWeekly · Sonnet\t17.00\tunknown\n"},
		{"repeated-scopes-retained", `{"limits":[{"kind":"weekly_scoped","percent":17,"scope":{"model":{"display_name":"Future model"}}},{"kind":"weekly_scoped","percent":26,"scope":{"model":{"display_name":"Future model"}}}]}`, "Weekly · Future model\t17.00\tunknown\nWeekly · Future model\t26.00\tunknown\n"},
		{"invalid-numbers-ignored", `{"limits":[{"kind":"session","percent":true},{"kind":"session","percent":"88"},{"kind":"session","percent":-1},{"kind":"weekly_all","percent":1e100},{"kind":"weekly_scoped","percent":39}]}`, "Weekly · model limit\t39.00\tunknown\n"},
		{"control-characters-sanitized", `{"limits":[{"kind":"weekly_scoped","percent":14,"scope":{"model":{"display_name":"New\tmodel\nname\u001b"}}}]}`, "Weekly · New model name \t14.00\tunknown\n"},
		{"bad-date-is-unknown", `{"five_hour":{"utilization":0,"resets_at":"bad-date"}}`, "5-hour session\t0.00\tunknown\n"},
		{"offset-date-becomes-utc", `{"five_hour":{"utilization":10,"resets_at":"2030-01-01T08:00:00+08:00"}}`, "5-hour session\t10.00\t2030-01-01T00:00:00Z\n"},
	}
	for _, test := range rows {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			p := f.add("default", "subscription", "pro", "")
			f.reply(p.name(), apiReply{status: 200, body: test.body})
			var want strings.Builder
			want.WriteString(usageHeader)
			for _, line := range strings.Split(strings.TrimSuffix(test.want, "\n"), "\n") {
				want.WriteString("default\tPro\t" + line + "\n")
			}
			before := readFile(t, f.store)
			expect(t, f.run("usage", "--fresh"), 0, want.String(), "")
			if f.requestCount() != 1 {
				t.Error("usage did not make exactly one authenticated localhost request")
			}
			if !bytes.Equal(before, readFile(t, f.store)) {
				t.Error("usage changed the credential store")
			}
			cache := readFile(t, filepath.Join(f.base, "usage-cache.json"))
			var saved struct {
				Version  int            `json:"version"`
				Profiles map[string]any `json:"profiles"`
			}
			if json.Unmarshal(cache, &saved) != nil || saved.Version != 1 || saved.Profiles[p.service()] == nil {
				t.Error("usage did not save a Swift-compatible cache by credential service")
			}
			info, err := os.Stat(filepath.Join(f.base, "usage-cache.json"))
			if err != nil || info.Mode().Perm() != 0600 {
				t.Error("cache was not saved privately")
			}
		})
	}
}

func TestUsagePlans(t *testing.T) {
	rows := []struct{ subscription, tier, want string }{
		{"pro", "", "Pro"},
		{"max", "default_claude_max_5x", "Max 5×"},
		{"max", "default_claude_max_20x", "Max 20×"},
		{"max", "not-a-known-tier", "Max (tier unknown)"},
		{"team", "default_claude_max_5x", "Team Premium"},
		{"team", "default_claude_max_20x", "Team (tier unknown)"},
		{"enterprise", "", "Enterprise"},
		{"future-plan", "default_claude_max_20x", "Plan unknown"},
	}
	for index, test := range rows {
		t.Run(fmt.Sprintf("plan-%d", index), func(t *testing.T) {
			f := newFixture(t)
			f.add("alpha", "subscription", test.subscription, test.tier)
			used := float64(10 + index)
			f.reply("alpha", apiReply{status: 200, body: fmt.Sprintf(`{"five_hour":{"utilization":%g}}`, used)})
			expect(t, f.run("usage"), 0, usageHeader+fmt.Sprintf("alpha\t%s\t5-hour session\t%.2f\tunknown\n", test.want, used), "")
			if f.requestCount() != 1 {
				t.Error("plan was not read alongside an actual quota request")
			}
		})
	}
}

func TestUsageCacheAges(t *testing.T) {
	rows := []struct {
		name    string
		age     time.Duration
		options []string
		request bool
	}{
		{"default-reuses-two-minutes", 2 * time.Minute, nil, false},
		{"default-refreshes-four-minutes", 4 * time.Minute, nil, true},
		{"max-age-reuses-one-hour", time.Hour, []string{"--max-age", "86400"}, false},
		{"max-age-refreshes-older", 3 * time.Hour, []string{"--max-age", "3600"}, true},
		{"fresh-ignores-young-cache", time.Second, []string{"--fresh"}, true},
		{"zero-ignores-young-cache", time.Second, []string{"--max-age", "0"}, true},
		{"leading-zero-age", time.Minute, []string{"--max-age", "00180"}, false},
	}
	for _, test := range rows {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			p := f.add("alpha", "subscription", "pro", "")
			f.cache(map[string]any{p.service(): cacheEntry(p, time.Now().Add(-test.age), cachedWindow("five_hour", "5-hour session", 12, nil))})
			f.reply("alpha", apiReply{status: 200, body: `{"five_hour":{"utilization":47}}`})
			used, count := "12.00", 0
			if test.request {
				used, count = "47.00", 1
			} else {
				// A valid cache must be used without consulting an unreadable store.
				writeFile(t, f.store, []byte("unreadable credential store"))
			}
			expect(t, f.run(append([]string{"usage"}, test.options...)...), 0, usageHeader+"alpha\tPro\t5-hour session\t"+used+"\tunknown\n", "")
			if f.requestCount() != count {
				t.Error("max age did not govern actual requests")
			}
		})
	}
}

func TestUsageFreshAcrossInvocations(t *testing.T) {
	f := newFixture(t)
	f.add("alpha", "subscription", "pro", "")
	f.reply("alpha", apiReply{status: 200, body: `{"five_hour":{"utilization":18}}`})
	expect(t, f.run("usage"), 0, usageHeader+"alpha\tPro\t5-hour session\t18.00\tunknown\n", "")
	f.reply("alpha", apiReply{status: 200, body: `{"five_hour":{"utilization":52}}`})
	expect(t, f.run("usage"), 0, usageHeader+"alpha\tPro\t5-hour session\t18.00\tunknown\n", "")
	expect(t, f.run("usage", "--fresh"), 0, usageHeader+"alpha\tPro\t5-hour session\t52.00\tunknown\n", "")
	if f.requestCount() != 2 {
		t.Error("separate processes did not share their cache or honor --fresh")
	}
}

func TestUsageHTTPFailures(t *testing.T) {
	rows := []struct {
		name         string
		status       int
		body, reason string
	}{
		{"unauthorized", 401, `{"error":"do-not-print-response"}`, "Claude rejected"},
		{"forbidden", 403, `{"error":"do-not-print-response"}`, "cannot read usage"},
		{"limited", 429, `{"error":"do-not-print-response"}`, "rate limited"},
		{"server", 503, `{"error":"do-not-print-response"}`, "HTTP 503"},
		{"redirect", 302, `{"error":"do-not-print-response"}`, "HTTP 302"},
		{"invalid-json", 200, `not-json`, "unrecognized usage response"},
		{"no-windows", 200, `{}`, "unrecognized usage response"},
		{"boolean-percent", 200, `{"five_hour":{"utilization":true}}`, "unrecognized usage response"},
		{"oversized-response", 200, strings.Repeat("x", 1_048_577), "unrecognized usage response"},
	}
	for _, test := range rows {
		for _, cached := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cached-%t", test.name, cached), func(t *testing.T) {
				f := newFixture(t)
				p := f.add("alpha", "subscription", "pro", "")
				if cached {
					f.cache(map[string]any{p.service(): cacheEntry(p, time.Now().Add(-10*time.Minute), cachedWindow("five_hour", "5-hour session", 23, nil))})
				}
				f.reply("alpha", apiReply{status: test.status, body: test.body})
				r := f.run("usage", "--fresh")
				if cached {
					expect(t, r, 0, usageHeader+"alpha\tPro\t5-hour session\t23.00\tunknown\n", test.reason)
					if !strings.Contains(r.err, "showing reading from 10 min ago") {
						t.Error("stale reading omitted its measured age")
					}
				} else {
					reason := test.reason
					if test.status == 429 {
						reason = "limiting requests"
					}
					expect(t, r, 1, usageHeader, reason)
				}
				if strings.Contains(r.err, "do-not-print-response") || f.requestCount() != 1 {
					t.Error("HTTP failure exposed a body, retried, or followed a redirect")
				}
			})
		}
	}
}

func TestUsageCredentialFailures(t *testing.T) {
	rows := []struct {
		name, envelope, reason string
		missing, broken        bool
	}{
		{"missing-item", "", "Sign in", true, false},
		{"malformed-envelope", "not-json", "saved OAuth credential item", false, false},
		{"missing-token", `{"claudeAiOauth":{}}`, "saved OAuth credential item", false, false},
		{"invalid-token", `{"claudeAiOauth":{"accessToken":"cannot\nbe-a-header"}}`, "Sign in", false, false},
		{"expired", `{"claudeAiOauth":{"accessToken":"expired-fixture","expiresAt":1}}`, "Access token expired", false, false},
		{"broken-store", "", "saved OAuth credential item", false, true},
	}
	for _, test := range rows {
		for _, cached := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cached-%t", test.name, cached), func(t *testing.T) {
				f := newFixture(t)
				p := f.add("alpha", "subscription", "pro", "")
				f.secrets[p.service()] = test.envelope
				if test.missing {
					delete(f.secrets, p.service())
				}
				f.publish()
				if test.broken {
					writeFile(t, f.store, []byte("broken store"))
				}
				if cached {
					f.cache(map[string]any{p.service(): cacheEntry(p, time.Now().Add(-30*time.Minute), cachedWindow("five_hour", "5-hour session", 42, nil))})
				}
				before := readFile(t, f.store)
				r := f.run("usage", "--fresh")
				if cached {
					expect(t, r, 0, usageHeader+"alpha\tPro\t5-hour session\t42.00\tunknown\n", test.reason)
					if !strings.Contains(r.err, "showing reading from 30 min ago") {
						t.Error("credential failure did not report the age of its last reading")
					}
				} else {
					expect(t, r, 1, usageHeader, test.reason)
				}
				if f.requestCount() != 0 || !bytes.Equal(before, readFile(t, f.store)) {
					t.Error("invalid login triggered HTTP or rotated credentials")
				}
			})
		}
	}
}

func TestUsageNetworkFailure(t *testing.T) {
	f := newFixture(t)
	p := f.add("alpha", "subscription", "pro", "")
	f.cache(map[string]any{p.service(): cacheEntry(p, time.Now().Add(-5*time.Minute), cachedWindow("five_hour", "5-hour session", 38, nil))})
	f.server.Close()
	expect(t, f.run("usage", "--fresh"), 0, usageHeader+"alpha\tPro\t5-hour session\t38.00\tunknown\n", "Cannot reach Claude. Keeping the last reading; showing reading from 5 min ago.")
}

func TestUsageMalformedCaches(t *testing.T) {
	rows := []struct{ name, data string }{
		{"not-json", "broken"},
		{"new-version", `{"version":9,"profiles":{}}`},
		{"wrong-shape", `{"version":1,"profiles":[]}`},
		{"oversized", strings.Repeat("x", 1_048_577)},
	}
	for _, test := range rows {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			f.add("alpha", "subscription", "pro", "")
			writeFile(t, filepath.Join(f.base, "usage-cache.json"), []byte(test.data))
			f.reply("alpha", apiReply{status: 200, body: `{"five_hour":{"utilization":19}}`})
			expect(t, f.run("usage"), 0, usageHeader+"alpha\tPro\t5-hour session\t19.00\tunknown\n", "")
			if f.requestCount() != 1 {
				t.Error("invalid cache prevented an actual quota request")
			}
		})
	}
}

func TestUsageInvalidCacheEntries(t *testing.T) {
	for _, invalid := range []string{"negative-percent", "empty-windows", "future-reading", "wrong-service", "negative-count", "missing-window-fields", "missing-plan"} {
		t.Run(invalid, func(t *testing.T) {
			f := newFixture(t)
			p := f.add("alpha", "subscription", "pro", "")
			entry := cacheEntry(p, time.Now().Add(-time.Minute), cachedWindow("five_hour", "5-hour session", 12, nil))
			reading := entry["reading"].(map[string]any)
			key := p.service()
			switch invalid {
			case "negative-percent":
				reading["windows"] = []any{cachedWindow("five_hour", "5-hour session", -1, nil)}
			case "empty-windows":
				reading["windows"] = []any{}
			case "future-reading":
				reading["fetchedAt"] = time.Now().Add(time.Hour).Format(time.RFC3339Nano)
			case "wrong-service":
				key = "not-the-profile-service"
			case "negative-count":
				entry["rateLimits"] = -1
			case "missing-window-fields":
				reading["windows"] = []any{map[string]any{"id": "five_hour"}}
			case "missing-plan":
				delete(reading, "plan")
			}
			f.cache(map[string]any{key: entry})
			f.reply("alpha", apiReply{status: 200, body: `{"five_hour":{"utilization":27}}`})
			expect(t, f.run("usage"), 0, usageHeader+"alpha\tPro\t5-hour session\t27.00\tunknown\n", "")
			if f.requestCount() != 1 {
				t.Error("invalid cache entry was trusted")
			}
		})
	}
}

func TestUsageCacheSymlink(t *testing.T) {
	f := newFixture(t)
	f.add("alpha", "subscription", "pro", "")
	target := filepath.Join(f.home, "symlink-target")
	writeFile(t, target, []byte("keep this target untouched"))
	if err := os.Symlink(target, filepath.Join(f.base, "usage-cache.json")); err != nil {
		t.Fatal("could not create the cache symlink canary")
	}
	f.reply("alpha", apiReply{status: 200, body: `{"five_hour":{"utilization":64}}`})
	expect(t, f.run("usage"), 0, usageHeader+"alpha\tPro\t5-hour session\t64.00\tunknown\n", "")
	if string(readFile(t, target)) != "keep this target untouched" || f.requestCount() != 1 {
		t.Error("cache read or replacement followed a symlink")
	}
}

func TestUsageMixedSuccessAndFailure(t *testing.T) {
	f := newFixture(t)
	f.add("good", "subscription", "pro", "")
	f.add("bad", "subscription", "pro", "")
	f.reply("good", apiReply{status: 200, body: `{"five_hour":{"utilization":16}}`})
	f.reply("bad", apiReply{status: 403, body: `{}`})
	expect(t, f.run("usage", "--fresh"), 1, usageHeader+"good\tPro\t5-hour session\t16.00\tunknown\n", "bad: This credential cannot read usage")
	if f.requestCount() != 2 {
		t.Error("one failure prevented another profile from being measured")
	}
}

func TestUsageCacheWriteFailureKeepsMeasuredResult(t *testing.T) {
	f := newFixture(t)
	f.add("alpha", "subscription", "pro", "")
	if os.Mkdir(filepath.Join(f.base, "usage-cache.json"), 0700) != nil {
		t.Fatal("could not create the non-writable cache destination")
	}
	f.reply("alpha", apiReply{status: 200, body: `{"five_hour":{"utilization":57}}`})
	expect(t, f.run("usage", "--fresh"), 0, usageHeader+"alpha\tPro\t5-hour session\t57.00\tunknown\n", "")
	if f.requestCount() != 1 {
		t.Error("a cache failure replaced the measured quota result")
	}
}
