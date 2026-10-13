//go:build claudock_test

package launch

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

func swapUsageEndpoint() (string, error) {
	endpoint := os.Getenv("CLAUDOCK_TEST_USAGE_ENDPOINT")
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Fragment != "" || u.RawQuery != "" ||
		!strings.HasPrefix(os.Getenv("CLAUDOCK_CREDENTIAL_STORE"), "file:") {
		return "", fmt.Errorf("the test usage endpoint must be a loopback HTTP URL with a file credential store")
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return "", fmt.Errorf("the test usage endpoint must be a loopback HTTP URL with a file credential store")
	}
	return endpoint, nil
}
