package usage

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/laneboss-io/claudock-cli/internal/state"
	"github.com/laneboss-io/claudock-cli/internal/store"
	"golang.org/x/text/unicode/norm"
)

const requestSpacing = 500 * time.Millisecond

type cacheEntry struct {
	Reading    *reading   `json:"reading,omitempty"`
	RetryAt    *time.Time `json:"retryAt,omitempty"`
	RateLimits int        `json:"rateLimits,omitempty"`
}

type cacheContents struct {
	Version  int                   `json:"version"`
	Profiles map[string]cacheEntry `json:"profiles"`
}

var cacheService = regexp.MustCompile(`^Claude Code-credentials(?:-[0-9a-f]{8})?$`)

func dataDirectory(home string) string {
	return filepath.Join(home, "Library", "Application Support", "Claudock")
}

func cacheKey(profile store.Profile) string {
	if profile.Command == "claude" {
		return "Claude Code-credentials"
	}
	digest := sha256.Sum256([]byte(norm.NFC.String(profile.ConfigDirectory)))
	return fmt.Sprintf("Claude Code-credentials-%x", digest[:4])
}

func readCache(home string) cacheContents {
	empty := cacheContents{Version: 1, Profiles: make(map[string]cacheEntry)}
	data, err := state.Read(filepath.Join(dataDirectory(home), "usage-cache.json"), 1_048_576)
	if err != nil {
		return empty // As in Swift, the cache is never required for a reading.
	}
	var contents cacheContents
	if json.Unmarshal(data, &contents) != nil || contents.Version != 1 || contents.Profiles == nil {
		return empty
	}
	for key, entry := range contents.Profiles {
		valid := cacheService.MatchString(key) && entry.RateLimits >= 0
		if r := entry.Reading; r != nil {
			valid = valid && !r.FetchedAt.IsZero() && len(r.Windows) > 0
			for _, w := range r.Windows {
				valid = valid && finiteNonnegative(w.Percent)
			}
			if r.ExtraUsagePercent != nil {
				valid = valid && finiteNonnegative(*r.ExtraUsagePercent)
			}
		}
		if !valid {
			delete(contents.Profiles, key)
		}
	}
	return contents
}

func finiteNonnegative(value float64) bool {
	return value >= 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func lastUse(entry cacheEntry) time.Time {
	var at time.Time
	if entry.Reading != nil {
		at = entry.Reading.FetchedAt
	}
	if entry.RetryAt != nil && entry.RetryAt.After(at) {
		at = *entry.RetryAt
	}
	return at
}

func writeCache(home string, contents cacheContents, now time.Time) error {
	for key, entry := range contents.Profiles {
		if entry.Reading != nil && now.Sub(entry.Reading.FetchedAt) > 7*24*time.Hour {
			entry.Reading = nil
		}
		if entry.RetryAt == nil || now.Sub(*entry.RetryAt) >= 24*time.Hour {
			entry.RetryAt = nil
			entry.RateLimits = 0
		}
		if entry.Reading == nil && entry.RetryAt == nil {
			delete(contents.Profiles, key)
		} else {
			contents.Profiles[key] = entry
		}
	}
	keys := make([]string, 0, len(contents.Profiles))
	for key := range contents.Profiles {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := lastUse(contents.Profiles[keys[i]]), lastUse(contents.Profiles[keys[j]])
		if a.Equal(b) {
			return keys[i] < keys[j]
		}
		return a.After(b)
	})
	kept := make(map[string]cacheEntry)
	budget := 1_048_576 - 256
	for _, key := range keys {
		if len(kept) == 256 {
			break
		}
		entry := contents.Profiles[key]
		encoded, err := json.Marshal(map[string]cacheEntry{key: entry})
		if err != nil {
			return err
		}
		if len(encoded) <= budget {
			budget -= len(encoded)
			kept[key] = entry
		}
	}
	contents.Profiles = kept
	data, err := json.Marshal(contents)
	if err != nil {
		return err
	}
	return state.AtomicWrite(filepath.Join(dataDirectory(home), "usage-cache.json"), append(data, '\n'))
}

type usageResult struct {
	reading *reading
	reason  *monitorError
}

type fetcher struct {
	home        string
	started     time.Time
	lock        *os.File
	tried       bool
	held        bool
	busy        bool
	deadline    time.Time
	lastRequest time.Time
}

func newFetcher(home string) *fetcher {
	return &fetcher{home: home, started: time.Now()}
}

func (f *fetcher) finish() {
	if f.lock != nil {
		state.Unlock(f.lock)
		f.lock = nil
	}
}

func settled(entry cacheEntry, cutoff time.Time) *usageResult {
	now := time.Now()
	if r := entry.Reading; r != nil && r.FetchedAt.After(cutoff) && !r.FetchedAt.After(now.Add(time.Minute)) {
		return &usageResult{reading: r}
	}
	if at := entry.RetryAt; at != nil && at.After(now) && !at.After(now.Add(24*time.Hour+time.Minute)) {
		return &usageResult{reading: entry.Reading, reason: &monitorError{kind: "rateLimited", retryAt: at}}
	}
	return nil
}

func (f *fetcher) turn(key string, cutoff time.Time) *usageResult {
	if f.held {
		return nil
	}
	if f.busy {
		return &usageResult{reading: readCache(f.home).Profiles[key].Reading, reason: problem("usageBusy")}
	}
	if !f.tried {
		f.tried = true
		f.deadline = time.Now().Add(30 * time.Second)
		f.lock, _ = state.OpenLock(filepath.Join(dataDirectory(f.home), ".usage-cache-lock"))
	}
	if f.lock == nil {
		return nil // Match Swift's uncoordinated request when the lock cannot be opened.
	}
	for {
		err := state.TryLock(f.lock, true)
		if err == nil {
			f.held = true
			return nil
		}
		entry := readCache(f.home).Profiles[key]
		if result := settled(entry, cutoff); result != nil {
			return result
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.lock.Close()
			f.lock = nil
			return nil
		}
		if !time.Now().Before(f.deadline) {
			f.busy = true
			f.lock.Close()
			f.lock = nil
			return &usageResult{reading: entry.Reading, reason: problem("usageBusy")}
		}
		time.Sleep(time.Duration(50+rand.IntN(51)) * time.Millisecond)
	}
}

func (f *fetcher) requestTime() time.Time {
	previous := f.lastRequest
	if f.held {
		buffer := make([]byte, 64)
		count, _ := f.lock.ReadAt(buffer, 0)
		seconds, err := strconv.ParseFloat(strings.TrimSpace(string(buffer[:count])), 64)
		if err == nil && seconds > 0 && !math.IsNaN(seconds) && !math.IsInf(seconds, 0) && seconds < 1e12 {
			recorded := time.Unix(int64(seconds), int64((seconds-math.Trunc(seconds))*1e9))
			if recorded.After(previous) {
				previous = recorded
			}
		}
	}
	return previous
}

func (f *fetcher) recordRequest(at time.Time) {
	f.lastRequest = at
	if f.held {
		data := []byte(fmt.Sprintf("%.3f\n", float64(at.UnixMilli())/1000))
		if f.lock.Truncate(0) == nil {
			// The lock timestamp, like the cache, is best effort; quota results
			// still stand when state storage runs out of space.
			_, _ = f.lock.WriteAt(data, 0)
		}
	}
}

func (f *fetcher) reading(profile store.Profile, maxAge time.Duration) usageResult {
	key := cacheKey(profile)
	cutoff := f.started.Add(-maxAge)
	entry := readCache(f.home).Profiles[key]
	if result := settled(entry, cutoff); result != nil {
		return *result
	}
	if result := f.turn(key, cutoff); result != nil {
		return *result
	}
	entry = readCache(f.home).Profiles[key]
	if result := settled(entry, cutoff); result != nil {
		return *result
	}
	if previous := f.requestTime(); !previous.IsZero() {
		pause := time.Until(previous.Add(requestSpacing))
		if pause > requestSpacing {
			pause = requestSpacing // A clock moving backward never causes a long wait.
		}
		if pause > 0 {
			time.Sleep(pause)
		}
	}
	r, failure := subscriptionReading(profile)
	finished := time.Now()
	f.recordRequest(finished)
	result := usageResult{reading: r}
	if failure == nil {
		entry = cacheEntry{Reading: r}
	} else {
		result = usageResult{reading: entry.Reading, reason: failure}
		if failure.kind == "rateLimited" {
			entry.RateLimits = min(entry.RateLimits, 1000) + 1
			until := failure.retryAt
			if until == nil {
				base := math.Min(1800, 60*math.Pow(2, float64(min(max(entry.RateLimits, 1)-1, 16))))
				seconds := math.Min(1800, base*(1+rand.Float64()/4))
				at := finished.Add(time.Duration(seconds * float64(time.Second))).UTC().Truncate(time.Millisecond)
				until = &at
			}
			entry.RetryAt = until
			result.reason = &monitorError{kind: "rateLimited", retryAt: until}
		}
	}
	contents := readCache(f.home)
	contents.Profiles[key] = entry
	// Swift explicitly treats cache write failure as non-fatal. No credential
	// material is part of this state, and the actual HTTP result is retained.
	_ = writeCache(f.home, contents, finished)
	return result
}
