package store

import (
	"errors"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

var modelPattern = regexp.MustCompile(`^[A-Za-z0-9._:/-]+(?:\[1m\])?$`)
var behavesPattern = regexp.MustCompile(`^claude-[a-z0-9][a-z0-9.-]*$`)

func ValidateEndpoint(endpoint EndpointConfiguration) (EndpointConfiguration, error) {
	if strings.HasPrefix(strings.ToLower(endpoint.Model), "sk-ant-") {
		return endpoint, errors.New("model selection cannot contain a credential")
	}
	raw := endpoint.BaseURL
	if len(raw) > 2048 || !strings.HasPrefix(strings.ToLower(raw), "https://") || strings.ContainsAny(raw, "?#") {
		return endpoint, errors.New("use an HTTPS endpoint with a host and no user information, query or fragment")
	}
	for _, r := range raw {
		if r < 33 || r > 126 {
			return endpoint, errors.New("endpoint URL must contain printable ASCII without whitespace")
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery || strings.ContainsAny(u.Hostname(), "\\%") {
		return endpoint, errors.New("use an HTTPS endpoint with a host and no user information, query or fragment")
	}
	if port := u.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return endpoint, errors.New("endpoint port must be from 1 to 65535")
		}
	}
	if strings.HasSuffix(u.Host, ":") {
		return endpoint, errors.New("endpoint port is invalid")
	}
	u.Scheme = "https"
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	if u.RawPath != "" {
		u.RawPath = strings.TrimRight(u.RawPath, "/")
	}
	endpoint.BaseURL = u.String()
	if len(endpoint.Model) > 128 || !modelPattern.MatchString(endpoint.Model) {
		return endpoint, errors.New("model must be one id of at most 128 letters, digits and . _ : / -, optionally ending in [1m]")
	}
	if endpoint.BehavesAs != "" && (len(endpoint.BehavesAs) > 128 || !behavesPattern.MatchString(endpoint.BehavesAs)) {
		return endpoint, errors.New("behaves-as requires a full Claude catalog model id")
	}
	return endpoint, nil
}
