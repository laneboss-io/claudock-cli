package usage_test

import (
	"crypto/sha256"
	"fmt"
	"testing"
	"time"
)

func TestUsageSharesSwiftUnicodeIdentity(t *testing.T) {
	for index, paths := range [][2]string{{"cafe\u0301", "café"}, {"\u1100\u1161\u11a8", "각"}, {"a\u0315\u0300", "à\u0315"}} {
		t.Run(fmt.Sprintf("path-%d", index), func(t *testing.T) {
			f := newFixture(t)
			p := f.add("alpha", "subscription", "pro", "")
			// The expected service hashes the independently specified composed
			// path, while the registry retains its original decomposed bytes.
			p.ConfigDirectory = f.home + "/accounts/" + paths[0]
			f.profiles[1] = p
			f.secrets = map[string]string{}
			f.publish()
			digest := sha256.Sum256([]byte(f.home + "/accounts/" + paths[1]))
			service := fmt.Sprintf("Claude Code-credentials-%x", digest[:4])
			f.cache(map[string]any{service: cacheEntry(p, time.Now().Add(-time.Minute), cachedWindow("five_hour", "5-hour session", float64(11+index), nil))})
			expect(t, f.run("usage"), 0, usageHeader+fmt.Sprintf("alpha\tPro\t5-hour session\t%.2f\tunknown\n", float64(11+index)), "")
			if f.requestCount() != 0 {
				t.Error("decomposed profile path did not reuse Swift's composed cache identity")
			}
		})
	}
}
