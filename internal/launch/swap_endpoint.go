//go:build !claudock_test

package launch

import (
	"fmt"
	"os"
	"strings"
)

func swapUsageEndpoint() (string, error) {
	// An explicitly selected fixture backend must never send its canaries to
	// the public API. Production has no test-endpoint override.
	if strings.HasPrefix(os.Getenv("CLAUDOCK_CREDENTIAL_STORE"), "file:") {
		return "", fmt.Errorf("a file credential test requires a claudock_test build")
	}
	return "https://api.anthropic.com/api/oauth/usage", nil
}
