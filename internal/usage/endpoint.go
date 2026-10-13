//go:build !claudock_test

package usage

// Production builds have no environment override for the remote API.
func usageEndpoint() (string, error) {
	return "https://api.anthropic.com/api/oauth/usage", nil
}
