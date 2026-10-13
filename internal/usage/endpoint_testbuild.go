//go:build claudock_test

package usage

import (
	"errors"
	"net/url"
	"os"
)

// A test build is deliberately incapable of using a real remote endpoint.
// With no valid literal loopback base URL, the request is refused.
func usageEndpoint() (string, error) {
	u, err := url.Parse(os.Getenv("CLAUDOCK_TEST_API_BASE_URL"))
	if err != nil || u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" || u.Path != "" && u.Path != "/" {
		return "", errors.New("test API base must be HTTP on literal loopback")
	}
	u.Path = "/api/oauth/usage"
	return u.String(), nil
}
