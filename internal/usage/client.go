package usage

import (
	"errors"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/laneboss-io/claudock-cli/internal/store"
)

// The credentials item is Claude Code's complete claudeAiOauth credential envelope.
// Reading through store.Secret keeps backend selection inside the profiles lane.
func subscriptionReading(profile store.Profile) (*reading, *monitorError) {
	secret, err := store.Secret(profile, "credentials")
	if err != nil {
		if errors.Is(err, store.ErrNoSecret) || errors.Is(err, os.ErrNotExist) {
			return nil, problem("noCredentials")
		}
		// Backend errors may contain material supplied by a credential source.
		// Never include those errors or the envelope in output.
		return nil, problem("credentialStore")
	}
	if len(secret) > 1_048_576 {
		return nil, problem("noCredentials")
	}
	root, err := decodeObject([]byte(secret))
	if err != nil {
		return nil, problem("noCredentials")
	}
	oauth := object(root["claudeAiOauth"])
	token := textValue(oauth["accessToken"])
	if !validAccessToken(token) {
		return nil, problem("noCredentials")
	}
	if milliseconds, ok := number(oauth["expiresAt"]); ok && milliseconds > 0 && milliseconds <= float64(time.Now().UnixMilli()) {
		return nil, problem("expired")
	}
	plan := credentialPlan(textValue(oauth["subscriptionType"]), textValue(oauth["rateLimitTier"]))
	endpoint, err := usageEndpoint()
	if err != nil {
		return nil, problem("testEndpoint")
	}
	request, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, problem("network")
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("anthropic-beta", "oauth-2025-04-20")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "Claudock/"+Version)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// Requests go directly to the selected API. A proxy from an inherited shell
	// must not receive a credential or take a localhost test out to the network.
	transport.Proxy = nil
	defer transport.CloseIdleConnections()
	client := &http.Client{
		Transport: transport,
		Timeout:   20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, problem("network")
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
		data, err := io.ReadAll(io.LimitReader(response.Body, 1_048_577))
		if err != nil {
			return nil, problem("network")
		}
		if len(data) > 1_048_576 {
			return nil, problem("invalidResponse")
		}
		reading, failure := parseUsage(data, plan, time.Now())
		if failure != nil {
			return nil, failure
		}
		// Model labels are remote text. Refuse an echoed credential instead of
		// displaying it or installing it in a non-credential cache.
		refresh := textValue(oauth["refreshToken"])
		for _, window := range reading.Windows {
			if strings.Contains(window.ID, token) || strings.Contains(window.Title, token) || refresh != "" && (strings.Contains(window.ID, refresh) || strings.Contains(window.Title, refresh)) {
				return nil, problem("invalidResponse")
			}
		}
		return reading, nil
	case http.StatusUnauthorized:
		return nil, problem("unauthorized")
	case http.StatusForbidden:
		return nil, problem("permissionDenied")
	case http.StatusTooManyRequests:
		return nil, &monitorError{kind: "rateLimited", retryAt: retryDate(response.Header.Get("Retry-After"), time.Now())}
	default:
		return nil, &monitorError{kind: "server", status: response.StatusCode}
	}
}

func validAccessToken(token string) bool {
	if token == "" || len(token) > 16_384 {
		return false
	}
	for _, c := range []byte(token) {
		if c <= 32 || c >= 127 {
			return false
		}
	}
	return true
}

func credentialPlan(subscription, tier string) string {
	switch subscription {
	case "max":
		switch tier {
		case "default_claude_max_5x":
			return "max5x"
		case "default_claude_max_20x":
			return "max20x"
		default:
			return "maxUnknown"
		}
	case "team":
		if tier == "default_claude_max_5x" {
			return "teamPremium"
		}
		return "teamUnknown"
	case "pro", "enterprise":
		return subscription
	default:
		return "unknown"
	}
}

func retryDate(header string, now time.Time) *time.Time {
	if header == "" {
		return nil
	}
	seconds, err := strconv.ParseFloat(header, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		at, parseErr := http.ParseTime(header)
		if parseErr != nil {
			return nil
		}
		seconds = at.Sub(now).Seconds()
	}
	seconds = math.Min(86_400, math.Max(300, seconds))
	at := now.Add(time.Duration(seconds * float64(time.Second))).UTC().Truncate(time.Millisecond)
	return &at
}
