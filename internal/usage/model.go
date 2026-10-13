package usage

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"
	"time"
)

type window struct {
	ID       string     `json:"id"`
	Title    string     `json:"title"`
	Percent  float64    `json:"percent"`
	ResetsAt *time.Time `json:"resetsAt,omitempty"`
}

func (w *window) UnmarshalJSON(data []byte) error {
	var fields struct {
		ID       *string    `json:"id"`
		Title    *string    `json:"title"`
		Percent  *float64   `json:"percent"`
		ResetsAt *time.Time `json:"resetsAt"`
	}
	if err := json.Unmarshal(data, &fields); err != nil || fields.ID == nil || fields.Title == nil || fields.Percent == nil {
		return errors.New("invalid cached usage window")
	}
	*w = window{ID: *fields.ID, Title: *fields.Title, Percent: *fields.Percent, ResetsAt: fields.ResetsAt}
	return nil
}

type reading struct {
	Plan              string    `json:"plan"`
	FetchedAt         time.Time `json:"fetchedAt"`
	Windows           []window  `json:"windows"`
	ExtraUsageEnabled bool      `json:"extraUsageEnabled"`
	ExtraUsagePercent *float64  `json:"extraUsagePercent,omitempty"`
}

func (r *reading) UnmarshalJSON(data []byte) error {
	var fields struct {
		Plan              *string    `json:"plan"`
		FetchedAt         *time.Time `json:"fetchedAt"`
		Windows           *[]window  `json:"windows"`
		ExtraUsageEnabled bool       `json:"extraUsageEnabled"`
		ExtraUsagePercent *float64   `json:"extraUsagePercent"`
	}
	if err := json.Unmarshal(data, &fields); err != nil || fields.Plan == nil || fields.FetchedAt == nil || fields.Windows == nil {
		return errors.New("invalid cached usage reading")
	}
	*r = reading{Plan: *fields.Plan, FetchedAt: *fields.FetchedAt, Windows: *fields.Windows, ExtraUsageEnabled: fields.ExtraUsageEnabled, ExtraUsagePercent: fields.ExtraUsagePercent}
	return nil
}

type monitorError struct {
	kind    string
	status  int
	retryAt *time.Time
}

func (e *monitorError) Error() string {
	switch e.kind {
	case "noCredentials":
		return "Sign in to this Claude profile to see usage."
	case "credentialStore":
		return "Could not read the saved OAuth credential item."
	case "expired":
		return "Access token expired. Open Claudock or this Claude profile to renew it."
	case "unauthorized":
		return "Claude rejected the renewed access token. Open this profile to check its login."
	case "permissionDenied":
		return "This credential cannot read usage. Check the account's access in Claude Code."
	case "rateLimited":
		return "Claude is limiting requests. Refresh will retry after a cooldown."
	case "usageBusy":
		return "Another Claudock process is reading usage. Try again in a moment."
	case "network":
		return "Cannot reach Claude. Keeping the last reading."
	case "testEndpoint":
		return "The test API base must use HTTP on literal 127.0.0.1."
	case "server":
		return fmt.Sprintf("Claude usage is unavailable (HTTP %d).", e.status)
	default:
		return "Claude returned an unrecognized usage response."
	}
}

func problem(kind string) *monitorError { return &monitorError{kind: kind} }

func needsSignIn(e *monitorError) bool {
	if e == nil {
		return false
	}
	switch e.kind {
	case "noCredentials", "expired", "unauthorized", "permissionDenied":
		return true
	default:
		return false
	}
}

func number(value any) (float64, bool) {
	n, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	v, err := n.Float64()
	return v, err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v < float64(math.MaxInt64)
}

func object(value any) map[string]any {
	m, _ := value.(map[string]any)
	return m
}

func textValue(value any) string {
	s, _ := value.(string)
	return s
}

func parseDate(value any) *time.Time {
	s, ok := value.(string)
	if !ok {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil
	}
	t = t.UTC()
	return &t
}

func decodeObject(data []byte) (map[string]any, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	var root map[string]any
	if err := d.Decode(&root); err != nil || root == nil {
		return nil, errors.New("invalid JSON object")
	}
	// Reject a second JSON value or trailing garbage.
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("invalid JSON suffix")
	}
	return root, nil
}

func parseUsage(data []byte, plan string, now time.Time) (*reading, *monitorError) {
	root, err := decodeObject(data)
	if err != nil {
		return nil, problem("invalidResponse")
	}
	r := &reading{Plan: plan, FetchedAt: now.UTC().Truncate(time.Millisecond)}
	limits, _ := root["limits"].([]any)
	for index, raw := range limits {
		entry := object(raw)
		percent, ok := number(entry["percent"])
		if !ok {
			continue
		}
		kind, knownKind := entry["kind"].(string)
		if !knownKind {
			kind = "limit"
		}
		scope := object(entry["scope"])
		label, knownLabel := object(scope["model"])["display_name"].(string)
		if !knownLabel {
			label, knownLabel = object(scope["surface"])["display_name"].(string)
		}
		var title string
		switch kind {
		case "session":
			title = "5-hour session"
		case "weekly_all":
			title = "Weekly · all models"
		case "weekly_scoped":
			if !knownLabel {
				label = "model limit"
			}
			title = "Weekly · " + label
		default:
			title = label
			if !knownLabel {
				words := strings.Split(strings.ReplaceAll(kind, "_", " "), " ")
				for i, word := range words {
					if word != "" {
						letters := []rune(word)
						words[i] = strings.ToUpper(string(letters[:1])) + strings.ToLower(string(letters[1:]))
					}
				}
				title = strings.Join(words, " ")
			}
		}
		r.Windows = append(r.Windows, window{ID: fmt.Sprintf("%s-%d", kind, index), Title: title, Percent: percent, ResetsAt: parseDate(entry["resets_at"])})
	}
	legacy := [][3]string{
		{"five_hour", "5-hour session", "session"}, {"seven_day", "Weekly · all models", "weekly_all"},
		{"seven_day_opus", "Weekly · Opus", "weekly_scoped"}, {"seven_day_sonnet", "Weekly · Sonnet", "weekly_scoped"},
		{"seven_day_oauth_apps", "Weekly · OAuth apps", "weekly_scoped"}, {"seven_day_cowork", "Weekly · Cowork", "weekly_scoped"},
	}
	for _, item := range legacy {
		duplicate := false
		for _, w := range r.Windows {
			if item[2] == "weekly_scoped" && w.Title == item[1] || item[2] != "weekly_scoped" && strings.HasPrefix(w.ID, item[2]+"-") {
				duplicate = true
				break
			}
		}
		entry := object(root[item[0]])
		if percent, ok := number(entry["utilization"]); ok && !duplicate {
			r.Windows = append(r.Windows, window{ID: item[0], Title: item[1], Percent: percent, ResetsAt: parseDate(entry["resets_at"])})
		}
	}
	if len(r.Windows) == 0 {
		return nil, problem("invalidResponse")
	}
	spend, extra := object(root["spend"]), object(root["extra_usage"])
	enabled, ok := spend["enabled"].(bool)
	if !ok {
		enabled, _ = extra["is_enabled"].(bool)
	}
	r.ExtraUsageEnabled = enabled
	if v, ok := number(spend["percent"]); ok {
		r.ExtraUsagePercent = &v
	} else if v, ok := number(extra["utilization"]); ok {
		r.ExtraUsagePercent = &v
	}
	return r, nil
}

var fableName = regexp.MustCompile(`(?i)\bFable(?:[ \t]*[0-9]+(?:\.[0-9]+)*)?\b`)

func (r *reading) governing() []window {
	fable := -1
	var candidates []int
	for i, w := range r.Windows {
		if fableName.MatchString(w.Title) {
			candidates = append(candidates, i)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := r.Windows[candidates[i]], r.Windows[candidates[j]]
		if a.Percent != b.Percent {
			return a.Percent > b.Percent
		}
		if strings.ToLower(a.Title) != strings.ToLower(b.Title) {
			return strings.ToLower(a.Title) < strings.ToLower(b.Title)
		}
		if a.ResetsAt == nil && b.ResetsAt != nil {
			return false
		}
		if a.ResetsAt != nil && b.ResetsAt == nil {
			return true
		}
		if a.ResetsAt != nil && b.ResetsAt != nil && !a.ResetsAt.Equal(*b.ResetsAt) {
			return a.ResetsAt.Before(*b.ResetsAt)
		}
		return a.ID < b.ID
	})
	if len(candidates) > 0 {
		fable = candidates[0]
	}
	session, weekly := -1, -1
	for i, w := range r.Windows {
		if i != fable && (w.ID == "five_hour" || strings.HasPrefix(w.ID, "session-") || w.Title == "5-hour session") {
			session = i
			break
		}
	}
	for i, w := range r.Windows {
		if i != fable && i != session && (w.ID == "seven_day" || strings.HasPrefix(w.ID, "weekly_all-") || w.Title == "Weekly · all models") {
			weekly = i
			break
		}
	}
	var main []window
	for _, index := range []int{session, weekly, fable} {
		if index >= 0 {
			main = append(main, r.Windows[index])
		}
	}
	if len(main) == 0 {
		return r.Windows
	}
	return main
}

type headroom struct {
	left  float64
	limit window
	again *time.Time
}

func (r *reading) headroom(now time.Time) *headroom {
	var tightest *headroom
	peak := -1.0
	unknownReset, full := false, false
	var again *time.Time
	for _, w := range r.governing() {
		used := w.Percent
		if w.ResetsAt != nil && !w.ResetsAt.After(now) {
			used = 0
		}
		if used > peak {
			peak = used
			tightest = &headroom{left: math.Max(0, 100-used), limit: w}
		}
		if used >= 100 {
			full = true
			if w.ResetsAt == nil {
				unknownReset = true
			} else if again == nil || w.ResetsAt.After(*again) {
				again = w.ResetsAt
			}
		}
	}
	if tightest != nil && full && !unknownReset {
		tightest.again = again
	}
	return tightest
}

func planName(raw string) string {
	switch raw {
	case "pro":
		return "Pro"
	case "max5x":
		return "Max 5×"
	case "max20x":
		return "Max 20×"
	case "maxUnknown":
		return "Max (tier unknown)"
	case "teamPremium":
		return "Team Premium"
	case "teamUnknown":
		return "Team (tier unknown)"
	case "enterprise":
		return "Enterprise"
	default:
		return "Plan unknown"
	}
}
