// Package testutil supplies diagnostics for isolated black-box CLI fixtures.
package testutil

import (
	"sort"
	"strings"
	"sync"
	"testing"
)

type cliResult struct {
	status         int
	stdout, stderr string
}

// CLIResults retains actual command results and prints them if the row fails,
// including failures in file, request or vendor assertions after a command.
// Only temporary fixture commands may be recorded here. Redaction applies to
// diagnostics; callers must keep asserting against the original output bytes.
type CLIResults struct {
	mu      sync.Mutex
	results []cliResult
	secrets map[string]struct{}
}

func CaptureCLI(t *testing.T) *CLIResults {
	t.Helper()
	log := &CLIResults{secrets: make(map[string]struct{})}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		log.mu.Lock()
		defer log.mu.Unlock()
		secrets := make([]string, 0, len(log.secrets))
		for secret := range log.secrets {
			secrets = append(secrets, secret)
		}
		sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
		redact := func(output string) string {
			for _, secret := range secrets {
				output = strings.ReplaceAll(output, secret, "[fixture credential redacted]")
			}
			return output
		}
		for index, result := range log.results {
			t.Logf("CLI invocation %d: exit status = %d; stdout = %q; stderr = %q",
				index+1, result.status, redact(result.stdout), redact(result.stderr))
		}
		if len(log.results) == 0 {
			t.Log("No fixture CLI invocation completed before this failure.")
		}
	})
	return log
}

// Redact registers fixture credentials without reading or printing environment
// values. Empty items are ignored because they contain no credential material.
func (log *CLIResults) Redact(values ...string) {
	log.mu.Lock()
	defer log.mu.Unlock()
	for _, value := range values {
		if value != "" {
			log.secrets[value] = struct{}{}
		}
	}
}

func (log *CLIResults) Record(status int, stdout, stderr string) {
	log.mu.Lock()
	defer log.mu.Unlock()
	log.results = append(log.results, cliResult{status: status, stdout: stdout, stderr: stderr})
}
