package usage_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestUsageCooldowns(t *testing.T) {
	rows := []struct {
		name, header     string
		minimum, maximum time.Duration
	}{
		{"seconds", "600", 598 * time.Second, 603 * time.Second},
		{"minimum-five-minutes", "1", 298 * time.Second, 303 * time.Second},
		{"maximum-one-day", "999999", 24*time.Hour - 2*time.Second, 24*time.Hour + 3*time.Second},
		{"http-date", time.Now().UTC().Add(12 * time.Minute).Format(http.TimeFormat), 11 * time.Minute, 13 * time.Minute},
		{"missing-backoff", "", 58 * time.Second, 78 * time.Second},
		{"invalid-backoff", "nonsense", 58 * time.Second, 78 * time.Second},
	}
	for _, test := range rows {
		t.Run(test.name, func(t *testing.T) {
			f := newFixture(t)
			p := f.add("alpha", "subscription", "pro", "")
			f.cache(map[string]any{p.service(): cacheEntry(p, time.Now().Add(-time.Hour), cachedWindow("five_hour", "5-hour session", 29, nil))})
			f.reply("alpha", apiReply{status: 429, body: `{}`, retry: test.header})
			started := time.Now()
			expect(t, f.run("usage", "--fresh"), 0, usageHeader+"alpha\tPro\t5-hour session\t29.00\tunknown\n", "rate limited until")
			var saved struct {
				Profiles map[string]struct {
					RetryAt    time.Time `json:"retryAt"`
					RateLimits int       `json:"rateLimits"`
				} `json:"profiles"`
			}
			if json.Unmarshal(readFile(t, filepath.Join(f.base, "usage-cache.json")), &saved) != nil {
				t.Fatal("cooldown state was not valid JSON")
			}
			entry := saved.Profiles[p.service()]
			wait := entry.RetryAt.Sub(started)
			if entry.RateLimits != 1 || wait < test.minimum || wait > test.maximum {
				t.Error("persisted cooldown did not apply the documented retry bounds")
			}
			// A changed server response must not be consumed before the cooldown.
			f.reply("alpha", apiReply{status: 200, body: `{"five_hour":{"utilization":70}}`})
			expect(t, f.run("usage", "--max-age", "0"), 0, usageHeader+"alpha\tPro\t5-hour session\t29.00\tunknown\n", "rate limited until")
			expect(t, f.run("available", "--names", "--fresh"), 0, "alpha\n", "rate limited until")
			if f.requestCount() != 1 {
				t.Error("--fresh or --max-age 0 bypassed a shared cooldown")
			}
		})
	}
}

func TestUsageCooldownWithoutReading(t *testing.T) {
	f := newFixture(t)
	f.add("alpha", "subscription", "pro", "")
	f.reply("alpha", apiReply{status: 429, body: `{}`, retry: "600"})
	expect(t, f.run("usage", "--fresh"), 1, usageHeader, "limiting requests")
	expect(t, f.run("usage", "--fresh"), 1, usageHeader, "limiting requests")
	expect(t, f.run("available", "--names", "--fresh"), 1, "", "limiting requests")
	if f.requestCount() != 1 {
		t.Error("a cooldown without a reading was forgotten across commands")
	}
}

func TestUsageBackoffCountAndSuccessReset(t *testing.T) {
	for _, success := range []bool{false, true} {
		t.Run(fmt.Sprintf("success-%t", success), func(t *testing.T) {
			f := newFixture(t)
			p := f.add("alpha", "subscription", "pro", "")
			entry := cacheEntry(p, time.Now().Add(-time.Hour), cachedWindow("five_hour", "5-hour session", 42, nil))
			entry["rateLimits"] = 3
			entry["retryAt"] = time.Now().Add(-time.Minute).Format(time.RFC3339Nano)
			f.cache(map[string]any{p.service(): entry})
			if success {
				f.reply("alpha", apiReply{status: 200, body: `{"five_hour":{"utilization":6}}`})
				expect(t, f.run("usage", "--fresh"), 0, usageHeader+"alpha\tPro\t5-hour session\t6.00\tunknown\n", "")
			} else {
				f.reply("alpha", apiReply{status: 429, body: `{}`})
				expect(t, f.run("usage", "--fresh"), 0, usageHeader+"alpha\tPro\t5-hour session\t42.00\tunknown\n", "rate limited until")
			}
			var saved struct {
				Profiles map[string]struct {
					RetryAt    *time.Time `json:"retryAt"`
					RateLimits int        `json:"rateLimits"`
				} `json:"profiles"`
			}
			if json.Unmarshal(readFile(t, filepath.Join(f.base, "usage-cache.json")), &saved) != nil {
				t.Fatal("backoff state was not valid JSON")
			}
			current := saved.Profiles[p.service()]
			if success {
				if current.RetryAt != nil || current.RateLimits != 0 {
					t.Error("success did not reset the rate-limit history")
				}
			} else if current.RetryAt == nil || current.RateLimits != 4 || time.Until(*current.RetryAt) < 478*time.Second || time.Until(*current.RetryAt) > 602*time.Second {
				t.Error("consecutive rate limits did not double the backoff")
			}
			if f.requestCount() != 1 {
				t.Error("an ended cooldown prevented a real request")
			}
		})
	}
}

func TestUsageRequestSpacing(t *testing.T) {
	f := newFixture(t)
	f.add("one", "subscription", "pro", "")
	f.add("two", "subscription", "pro", "")
	f.reply("one", apiReply{status: 200, body: `{"five_hour":{"utilization":15}}`})
	f.reply("two", apiReply{status: 200, body: `{"five_hour":{"utilization":25}}`})
	expect(t, f.run("usage", "--fresh"), 0, usageHeader+"one\tPro\t5-hour session\t15.00\tunknown\ntwo\tPro\t5-hour session\t25.00\tunknown\n", "")
	f.mu.Lock()
	if len(f.requests) != 2 {
		f.mu.Unlock()
		t.Fatal("spacing test did not make both requests")
	}
	gap := f.requests[1].started.Sub(f.requests[0].finished)
	f.mu.Unlock()
	if gap < 480*time.Millisecond {
		t.Error("quota requests did not keep half a second between them")
	}
	// The timestamp in the lock file also spaces separate invocations.
	expect(t, f.run("usage", "--fresh"), 0, usageHeader+"one\tPro\t5-hour session\t15.00\tunknown\ntwo\tPro\t5-hour session\t25.00\tunknown\n", "")
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 4 || f.requests[2].started.Sub(f.requests[1].finished) < 480*time.Millisecond {
		t.Error("request spacing was not shared across processes")
	}
}

func TestConcurrentUsageSharesNewReading(t *testing.T) {
	f := newFixture(t)
	f.add("alpha", "subscription", "pro", "")
	release := make(chan struct{})
	var released bool
	defer func() {
		if !released {
			close(release)
		}
	}()
	f.reply("alpha", apiReply{status: 200, body: `{"five_hour":{"utilization":61}}`, block: release})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	first, firstOut, firstErr := f.command(ctx, "usage", "--fresh")
	if first.Start() != nil {
		t.Fatal("first quota process did not start")
	}
	select {
	case <-f.entered:
	case <-ctx.Done():
		t.Fatal("first quota process did not reach the localhost API")
	}
	second, secondOut, secondErr := f.command(ctx, "usage", "--fresh")
	if second.Start() != nil {
		t.Fatal("second quota process did not start")
	}
	// Both are actual processes. Hold the first response long enough for the
	// second to wait on the same flock; no HTTP response has yet been cached.
	time.Sleep(800 * time.Millisecond)
	close(release)
	released = true
	want := usageHeader + "alpha\tPro\t5-hour session\t61.00\tunknown\n"
	one := f.collect(first, firstOut, firstErr)
	two := f.collect(second, secondOut, secondErr)
	one.expectedNotice, two.expectedNotice = defaultNotice, defaultNotice
	f.assertNoSecrets(one)
	f.assertNoSecrets(two)
	expect(t, one, 0, want, "")
	expect(t, two, 0, want, "")
	if f.requestCount() != 1 {
		t.Error("concurrent readers requested the same profile more than once")
	}
}

func TestUsageWaitTimeoutUsesCache(t *testing.T) {
	f := newFixture(t)
	p := f.add("alpha", "subscription", "pro", "")
	f.cache(map[string]any{p.service(): cacheEntry(p, time.Now().Add(-20*time.Minute), cachedWindow("five_hour", "5-hour session", 17, nil))})
	lock, err := os.OpenFile(filepath.Join(f.base, ".usage-cache-lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal("could not create held usage lock")
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		t.Fatal("could not hold usage lock")
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	started := time.Now()
	r := f.run("usage", "--fresh")
	expect(t, r, 0, usageHeader+"alpha\tPro\t5-hour session\t17.00\tunknown\n", "another Claudock process is reading usage; showing reading from")
	if time.Since(started) < 29*time.Second || time.Since(started) > 35*time.Second || f.requestCount() != 0 {
		t.Error("lock wait did not time out to cached data without a request")
	}
}

func TestUsagePrunesOldCache(t *testing.T) {
	f := newFixture(t)
	p := f.add("alpha", "subscription", "pro", "")
	now := time.Now()
	entries := map[string]any{p.service(): cacheEntry(p, now.Add(-time.Hour), cachedWindow("five_hour", "5-hour session", 20, nil))}
	for index := 0; index < 300; index++ {
		key := fmt.Sprintf("Claude Code-credentials-%08x", index)
		entry := cacheEntry(p, now.Add(-time.Duration(index+1)*time.Minute), cachedWindow("five_hour", "5-hour session", float64(index%100), nil))
		entries[key] = entry
	}
	oldKey := "Claude Code-credentials-ffffffff"
	entries[oldKey] = cacheEntry(p, now.Add(-8*24*time.Hour), cachedWindow("five_hour", "5-hour session", 100, nil))
	f.cache(entries)
	f.reply("alpha", apiReply{status: 200, body: `{"five_hour":{"utilization":9}}`})
	expect(t, f.run("usage", "--fresh"), 0, usageHeader+"alpha\tPro\t5-hour session\t9.00\tunknown\n", "")
	data := readFile(t, filepath.Join(f.base, "usage-cache.json"))
	var saved struct {
		Profiles map[string]any `json:"profiles"`
	}
	if json.Unmarshal(data, &saved) != nil || len(saved.Profiles) > 256 || len(data) > 1_048_576 || saved.Profiles[p.service()] == nil || saved.Profiles[oldKey] != nil {
		t.Error("cache pruning lost the latest result or exceeded its limits")
	}
	if f.requestCount() != 1 {
		t.Error("pruning test did not measure the current profile")
	}
}

func TestSecretsStayInCredentialStore(t *testing.T) {
	f := newFixture(t)
	f.add("alpha", "subscription", "pro", "")
	f.reply("alpha", apiReply{status: 200, body: `{"five_hour":{"utilization":31}}`})
	expect(t, f.run("usage", "--fresh"), 0, usageHeader+"alpha\tPro\t5-hour session\t31.00\tunknown\n", "")
	expect(t, f.run("available", "--names"), 0, "alpha\n", "")
	if err := filepath.WalkDir(f.home, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || path == f.store {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, token := range f.tokens {
			if strings.Contains(string(data), token) || strings.Contains(string(data), "unused-rotation-canary") {
				t.Error("credential canary was written outside its credential store")
			}
		}
		return nil
	}); err != nil {
		t.Fatal("could not inspect temporary output files for credential canaries")
	}
	if f.requestCount() != 1 {
		t.Error("canary check did not exercise an authenticated request and a cached reading")
	}
}
