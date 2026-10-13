package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/laneboss-io/claudock-cli/internal/store"
)

type swapOptions struct {
	from      string
	session   string
	cwd       string
	prompt    string
	threshold float64
	continued bool
	dryRun    bool
	exclude   map[string]bool
	prefer    map[string]int
	vendor    []string
}

func parseSwap(args []string) (swapOptions, error) {
	result := swapOptions{
		prompt: "Continue the same session from where it stopped.", threshold: 85,
		exclude: make(map[string]bool), prefer: make(map[string]int),
	}
	seen := make(map[string]bool)
	for i := 0; i < len(args); i++ {
		name := args[i]
		if name == "--" {
			result.vendor = args[i+1:]
			break
		}
		if name == "--continue" || name == "--dry-run" {
			if seen[name] {
				return result, invalid("A profile swap option was repeated.")
			}
			seen[name] = true
			if name == "--continue" {
				result.continued = true
			} else {
				result.dryRun = true
			}
			continue
		}
		if name != "--from" && name != "--session" && name != "--cwd" && name != "--max-used" &&
			name != "--exclude" && name != "--prefer" && name != "--prompt" {
			return result, invalid("Use: claudock profile swap --from NAME (--session ID | --continue) [--cwd DIR] [--max-used PCT] [--exclude NAME] [--prefer NAME] [--prompt TEXT] [--dry-run] [-- CLAUDE_ARGS...].")
		}
		if i+1 >= len(args) || (seen[name] && name != "--exclude" && name != "--prefer") {
			return result, invalid("A profile swap option is repeated or missing its value.")
		}
		seen[name] = true
		i++
		value := args[i]
		switch name {
		case "--from", "--exclude", "--prefer":
			if !validName(value) {
				return result, invalid("Profile swap requires valid profile names.")
			}
			switch name {
			case "--from":
				result.from = value
			case "--exclude":
				result.exclude[value] = true
			case "--prefer":
				if _, exists := result.prefer[value]; exists {
					return result, invalid("A preferred profile was repeated.")
				}
				result.prefer[value] = len(result.prefer)
			}
		case "--session":
			if !sessionIDShape.MatchString(value) {
				return result, invalid("--session requires an existing session UUID.")
			}
			result.session = value
		case "--cwd":
			result.cwd = value
		case "--prompt":
			result.prompt = value
		case "--max-used":
			number, err := strconv.ParseFloat(value, 64)
			if err != nil || math.IsInf(number, 0) || math.IsNaN(number) || number < 0 || number > 100 {
				return result, invalid("--max-used requires a finite percentage from 0 to 100.")
			}
			result.threshold = number
		}
	}
	if result.from == "" || result.continued == (result.session != "") {
		return result, invalid("Profile swap requires --from and exactly one of --session or --continue.")
	}
	for _, opt := range claudeOptions(result.vendor) {
		switch opt.name {
		case "-c", "--continue", "-r", "--resume", "--session-id", "--fork-session", "--teleport", "--cloud", "--remote", "--from-pr":
			return result, invalid("Vendor arguments cannot change the session selected for profile swap.")
		}
	}
	return result, nil
}

type quotaWindow struct {
	used  float64
	reset *time.Time
}

type swapCandidate struct {
	profile store.Profile
	peak    float64
	windows []quotaWindow
}

func swap(args []string, out, errOut io.Writer) error {
	options, err := parseSwap(args)
	if err != nil {
		return err
	}
	cwd := options.cwd
	if cwd == "" {
		cwd, err = os.Getwd()
	}
	if err == nil {
		cwd, err = filepath.Abs(cwd)
	}
	if err == nil {
		cwd, err = filepath.EvalSymlinks(cwd)
	}
	if err != nil {
		return invalid("The profile swap working directory cannot be resolved.")
	}
	info, err := os.Stat(cwd)
	if err != nil || !info.IsDir() {
		return invalid("The profile swap working directory must be a directory.")
	}
	from, err := store.Get(options.from)
	if err != nil || from.Kind == store.Codex || !validName(from.Name) || !validPath(from.ConfigDirectory) {
		return fmt.Errorf("cannot select the previous Claude profile")
	}
	env := isolatedEnvironment(os.Environ())
	source := newSessionContext(from.ConfigDirectory, cwd, env)
	id := options.session
	if options.continued {
		path := source.latest(false, false)
		if path == "" {
			return fmt.Errorf("%s: no session to continue in this directory", from.Name)
		}
		id = strings.TrimSuffix(filepath.Base(path), ".jsonl")
	}
	sources := source.resumed(id)
	if len(sources) == 0 {
		return fmt.Errorf("%s: the requested session is not readable", from.Name)
	}
	sourceBytes, err := readRegular(sources[0], 64<<20, false)
	if err != nil {
		return fmt.Errorf("%s: the requested session is not readable", from.Name)
	}
	profiles, err := store.List()
	if err != nil {
		return fmt.Errorf("cannot read profiles for swap")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var candidates []swapCandidate
	for _, p := range profiles {
		if p.Kind != store.Subscription || p.IsVertex || p.DiscoveryNote != "" {
			continue
		}
		if !validName(p.Name) || !validPath(p.ConfigDirectory) {
			return fmt.Errorf("the profile swap registry contains an unsafe profile")
		}
		if p.Command == from.Command || p.ConfigDirectory == from.ConfigDirectory ||
			options.exclude[p.Name] || options.exclude[p.Command] {
			continue
		}
		windows, err := readSwapUsage(ctx, p)
		if err != nil {
			return err
		}
		peak := 0.0
		for _, window := range windows {
			peak = math.Max(peak, window.used)
		}
		candidates = append(candidates, swapCandidate{p, peak, windows})
	}
	preference := func(p store.Profile) int {
		if rank, ok := options.prefer[p.Name]; ok {
			return rank
		}
		if rank, ok := options.prefer[p.Command]; ok {
			return rank
		}
		return len(options.prefer)
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.peak != b.peak {
			return a.peak < b.peak
		}
		if preference(a.profile) != preference(b.profile) {
			return preference(a.profile) < preference(b.profile)
		}
		return a.profile.Name < b.profile.Name
	})
	var chosen *swapCandidate
	roomWithoutHistory := false
	for i := range candidates {
		candidate := &candidates[i]
		if candidate.peak >= options.threshold {
			continue
		}
		// Histories must already be shared. Never copy a transcript, credential,
		// or account folder as a side effect of selecting quota.
		target := newSessionContext(candidate.profile.ConfigDirectory, cwd, env)
		for _, path := range target.resumed(id) {
			data, err := readRegular(path, 64<<20, false)
			if err == nil && bytes.Equal(data, sourceBytes) {
				chosen = candidate
				break
			}
		}
		if chosen != nil {
			break
		}
		roomWithoutHistory = true
	}
	if chosen == nil {
		message := fmt.Sprintf("no profile under %g%% with the same session history", options.threshold)
		if roomWithoutHistory {
			message += "; profiles with room do not share this session's current transcript"
		} else {
			var next *time.Time
			for _, candidate := range candidates {
				var available *time.Time
				known := options.threshold > 0
				for _, window := range candidate.windows {
					if window.used >= options.threshold {
						if window.reset == nil {
							known = false
						} else if available == nil || window.reset.After(*available) {
							available = window.reset
						}
					}
				}
				if known && available != nil && (next == nil || available.Before(*next)) {
					next = available
				}
			}
			if next != nil {
				message += "; next reset " + next.UTC().Format(time.RFC3339)
			} else {
				message += "; next reset unknown"
			}
		}
		return &commandError{message, 3}
	}
	vendor := []string{"-p", "--resume", id, options.prompt}
	vendor = append(vendor, options.vendor...)
	fmt.Fprintf(errOut, "claudock: resuming from %s on %s: tightest window %g%% used (< %g%%)\n",
		from.Name, chosen.profile.Name, chosen.peak, options.threshold)
	if options.dryRun {
		self, err := os.Executable()
		if err != nil {
			return fmt.Errorf("cannot locate Claudock for the dry run")
		}
		argv := []string{self, "run", chosen.profile.Name, "--"}
		argv = append(argv, vendor...)
		return json.NewEncoder(out).Encode(struct {
			Argv []string `json:"argv"`
			CWD  string   `json:"cwd"`
		}{argv, cwd})
	}
	if err := os.Chdir(cwd); err != nil {
		return fmt.Errorf("cannot enter the profile swap working directory")
	}
	return launchProfile(chosen.profile, vendor, false, errOut)
}

func readSwapUsage(ctx context.Context, p store.Profile) ([]quotaWindow, error) {
	token, err := secret(p, "oauth")
	if err != nil {
		return nil, err
	}
	// Accept either a backend's extracted access token or Claude's stored item.
	if strings.HasPrefix(strings.TrimSpace(token), "{") {
		var saved struct {
			OAuth struct {
				Access string   `json:"accessToken"`
				Expiry *float64 `json:"expiresAt"`
			} `json:"claudeAiOauth"`
		}
		if json.Unmarshal([]byte(token), &saved) != nil ||
			(saved.OAuth.Expiry != nil && *saved.OAuth.Expiry <= float64(time.Now().UnixMilli())) {
			return nil, fmt.Errorf("%s: credential item oauth is invalid or expired", p.Name)
		}
		token = saved.OAuth.Access
	}
	if token == "" || len(token) > 16384 || strings.ContainsAny(token, "\r\n\x00") {
		return nil, fmt.Errorf("%s: credential item oauth is missing or invalid", p.Name)
	}
	endpoint, err := swapUsageEndpoint()
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("%s: cannot prepare usage request", p.Name)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("anthropic-beta", "oauth-2025-04-20")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "Claudock-Go")
	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 15 * time.Second,
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Timeout: 20 * time.Second, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("%s: usage request failed", p.Name)
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return nil, fmt.Errorf("%s: usage request failed (HTTP %d)", p.Name, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, fmt.Errorf("%s: usage response is unreadable", p.Name)
	}
	root, err := jsonObject(data)
	if err != nil {
		return nil, fmt.Errorf("%s: usage response is invalid", p.Name)
	}
	var windows []quotaWindow
	add := func(value any, percentage string) error {
		entry, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: usage window is invalid", p.Name)
		}
		n, ok := entry[percentage].(json.Number)
		number, err := n.Float64()
		if !ok || err != nil || math.IsInf(number, 0) || math.IsNaN(number) || number < 0 {
			return fmt.Errorf("%s: usage percentage is invalid", p.Name)
		}
		var reset *time.Time
		if value, exists := entry["resets_at"]; exists && value != nil && value != "" {
			text, ok := value.(string)
			stamp, err := time.Parse(time.RFC3339Nano, text)
			if !ok || err != nil {
				return fmt.Errorf("%s: usage reset time is invalid", p.Name)
			}
			reset = &stamp
			if !stamp.After(time.Now()) {
				number = 0
			}
		}
		windows = append(windows, quotaWindow{number, reset})
		return nil
	}
	if value, exists := root["limits"]; exists {
		limits, ok := value.([]any)
		if !ok {
			return nil, fmt.Errorf("%s: usage limits are invalid", p.Name)
		}
		for _, limit := range limits {
			if err := add(limit, "percent"); err != nil {
				return nil, err
			}
		}
	}
	for _, key := range []string{"five_hour", "seven_day", "seven_day_opus", "seven_day_sonnet", "seven_day_oauth_apps", "seven_day_cowork"} {
		if value, exists := root[key]; exists && value != nil {
			if err := add(value, "utilization"); err != nil {
				return nil, err
			}
		}
	}
	if len(windows) == 0 {
		return nil, fmt.Errorf("%s: usage response has no windows", p.Name)
	}
	return windows, nil
}
