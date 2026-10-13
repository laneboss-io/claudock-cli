package usage_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsageRefusesCredentialEcho(t *testing.T) {
	for _, echo := range []string{"access-label", "access-kind", "refresh-label"} {
		t.Run(echo, func(t *testing.T) {
			f := newFixture(t)
			f.add("alpha", "subscription", "pro", "")
			label, kind := "model", "weekly_scoped"
			switch echo {
			case "access-label":
				label = f.tokens["alpha"]
			case "access-kind":
				kind = f.tokens["alpha"]
			case "refresh-label":
				label = "unused-rotation-canary"
			}
			body := string(marshal(t, map[string]any{"limits": []any{map[string]any{"kind": kind, "percent": 20, "scope": map[string]any{"model": map[string]any{"display_name": label}}}}}))
			f.reply("alpha", apiReply{status: 200, body: body})
			expect(t, f.run("usage", "--fresh"), 1, usageHeader, "unrecognized usage response")
			cache := filepath.Join(f.base, "usage-cache.json")
			data, err := os.ReadFile(cache)
			if err != nil && !os.IsNotExist(err) {
				t.Fatal("could not inspect cache after credential-echo refusal")
			}
			if strings.Contains(string(data), f.tokens["alpha"]) || strings.Contains(string(data), "unused-rotation-canary") || f.requestCount() != 1 {
				t.Error("remote credential echo escaped its refusal or no request exercised the canary")
			}
		})
	}
}
