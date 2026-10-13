// Package usage implements the usage, available, require-token, and version commands.
package usage

import (
	"fmt"
	"io"
	"math"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/laneboss-io/claudock-cli/internal/policy"
	"github.com/laneboss-io/claudock-cli/internal/store"
)

const Version = "1.7.0"

// Run validates the whole command before any registry or credential read.
// Unhandled commands return false so the other command lanes can dispatch them.
func Run(arguments []string, stdout, stderr io.Writer) (handled bool, code int) {
	if len(arguments) == 0 {
		return false, 0
	}
	command := arguments[0]
	switch command {
	case "version", "--version":
		if len(arguments) != 1 {
			diagnostic(stderr, "Use 'claudock version'. Run 'claudock help' for usage.")
			return true, 2
		}
		fmt.Fprintln(stdout, "Claudock "+Version)
		return true, 0
	case "require-token":
		if len(arguments) != 2 || arguments[1] != "on" && arguments[1] != "off" && arguments[1] != "status" {
			diagnostic(stderr, "Use 'claudock require-token on|off|status'. Run 'claudock help' for usage.")
			return true, 2
		}
		if arguments[1] == "status" {
			required, err := policy.Required()
			if err != nil {
				diagnostic(stderr, err.Error())
				return true, 1
			}
			if required {
				fmt.Fprintln(stdout, "on")
			} else {
				fmt.Fprintln(stdout, "off")
			}
			return true, 0
		}
		required := arguments[1] == "on"
		if err := policy.SetRequired(required); err != nil {
			diagnostic(stderr, err.Error())
			return true, 1
		}
		if required {
			fmt.Fprintln(stdout, "Claudock now requires an inference token to launch subscription profiles; it no longer falls back to their normal login.")
		} else {
			fmt.Fprintln(stdout, "Claudock no longer requires an inference token; subscription profiles without one launch with their normal login.")
		}
		return true, 0
	case "usage", "available":
		maxAge, namesOnly, message := usageOptions(arguments[1:], command)
		if message != "" {
			diagnostic(stderr, message+" Run 'claudock help' for usage.")
			return true, 2
		}
		home, err := os.UserHomeDir()
		if err != nil {
			diagnostic(stderr, "Could not locate the home directory.")
			return true, 1
		}
		profiles, err := store.List()
		if err != nil {
			diagnostic(stderr, "Could not read the profile registry.")
			return true, 1
		}
		entries, err := metadata(home)
		if err != nil {
			diagnostic(stderr, err.Error())
			return true, 1
		}
		if command == "usage" {
			return true, showUsage(home, profiles, entries, maxAge, stdout, stderr)
		}
		return true, showAvailable(home, profiles, entries, maxAge, namesOnly, stdout, stderr)
	default:
		return false, 0
	}
}

func usageOptions(options []string, command string) (time.Duration, bool, string) {
	options = append([]string(nil), options...)
	names := false
	if command == "available" && len(options) > 0 {
		if options[0] == "--names" {
			names = true
			options = options[1:]
		} else if options[len(options)-1] == "--names" {
			names = true
			options = options[:len(options)-1]
		}
	}
	if len(options) == 0 {
		return 180 * time.Second, names, ""
	}
	if len(options) == 1 && options[0] == "--fresh" {
		return 0, names, ""
	}
	if len(options) == 2 && options[0] == "--max-age" {
		valid := options[1] != ""
		for _, c := range options[1] {
			valid = valid && c >= '0' && c <= '9'
		}
		seconds, err := strconv.ParseUint(options[1], 10, 32)
		if !valid || err != nil || seconds > 86_400 {
			return 0, names, "--max-age takes a whole number of seconds from 0 to 86400."
		}
		return time.Duration(seconds) * time.Second, names, ""
	}
	if command == "available" {
		command += " [--names]"
	}
	return 0, names, "Use 'claudock " + command + "', 'claudock " + command + " --max-age SECONDS', or 'claudock " + command + " --fresh'."
}

func field(value string) string {
	return strings.Map(func(c rune) rune {
		if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) {
			return ' '
		}
		return c
	}, value)
}

func diagnostic(out io.Writer, message string) {
	fmt.Fprintln(out, "claudock: "+field(message))
}

func row(out io.Writer, fields ...string) {
	for i := range fields {
		fields[i] = field(fields[i])
	}
	fmt.Fprintln(out, strings.Join(fields, "\t"))
}

func resetText(at *time.Time) string {
	if at == nil {
		return "unknown"
	}
	return at.UTC().Format(time.RFC3339)
}

func staleReason(reason *monitorError) string {
	if reason.kind == "rateLimited" && reason.retryAt != nil {
		return "rate limited until " + reason.retryAt.Local().Format("15:04")
	}
	if reason.kind == "usageBusy" {
		return "another Claudock process is reading usage"
	}
	return strings.TrimSuffix(reason.Error(), ".")
}

func age(at time.Time) string {
	minutes := max(1, int(math.Round(math.Max(0, time.Since(at).Minutes()))))
	return strconv.Itoa(minutes) + " min ago"
}

func endpointNote(profile store.Profile) string {
	host := "provider"
	if endpoint, err := url.Parse(profile.EndpointURL); err == nil && endpoint.Hostname() != "" {
		host = endpoint.Hostname()
	}
	return "third-party endpoint (" + host + ", " + profile.EndpointModel + "), billed per token by the provider; no quota to read."
}

func supported(profile store.Profile, meta profileMetadata) bool {
	return profile.ConfigDirectory != "" && !meta.IsVertex && meta.DiscoveryNote == nil && profile.Kind != store.Codex
}

func showUsage(home string, profiles []store.Profile, entries map[string]profileMetadata, maxAge time.Duration, stdout, stderr io.Writer) int {
	fetch := newFetcher(home)
	defer fetch.finish()
	failed := false
	row(stdout, "PROFILE", "PLAN", "WINDOW", "USED_PERCENT", "RESETS_UTC")
	for _, profile := range profiles {
		meta := metadataFor(profile, entries)
		if !supported(profile, meta) {
			diagnostic(stderr, profile.Name+": skipped; this profile does not support subscription usage.")
			continue
		}
		switch profile.Kind {
		case store.Endpoint:
			diagnostic(stderr, profile.Name+": "+endpointNote(profile))
			continue
		case store.APIKey, store.ConsoleLogin:
			credit, err := readCredit(home, profileID(profile, meta))
			if err != nil {
				failed = true
				diagnostic(stderr, profile.Name+": "+err.Error())
			} else if credit != nil {
				row(stdout, profile.Name, "Console API", credit.usageWindow(), credit.usedPercent(), "-")
			} else {
				billing := "Console API key profiles are billed per token and have"
				if profile.Kind == store.ConsoleLogin {
					billing = "Console login profiles are billed per token and have"
				}
				diagnostic(stderr, profile.Name+": skipped; "+billing+" no subscription limits. Set its balance with: claudock profile set-credit "+profile.Name+" AMOUNT")
			}
			continue
		case store.Subscription, "":
		default:
			diagnostic(stderr, profile.Name+": skipped; this profile does not support subscription usage.")
			continue
		}
		result := fetch.reading(profile, maxAge)
		if result.reading == nil {
			failed = true
			diagnostic(stderr, profile.Name+": "+result.reason.Error())
			continue
		}
		if result.reason != nil {
			diagnostic(stderr, profile.Name+": "+staleReason(result.reason)+"; showing reading from "+age(result.reading.FetchedAt)+".")
		}
		for _, limit := range result.reading.Windows {
			row(stdout, profile.Name, planName(result.reading.Plan), limit.Title, fmt.Sprintf("%.2f", limit.Percent), resetText(limit.ResetsAt))
		}
	}
	if failed {
		return 1
	}
	return 0
}

type availableRow struct {
	profile store.Profile
	kind    string
	plan    string
	left    string
	limit   string
	resets  string
	rank    float64
}

func showAvailable(home string, profiles []store.Profile, entries map[string]profileMetadata, maxAge time.Duration, namesOnly bool, stdout, stderr io.Writer) int {
	fetch := newFetcher(home)
	defer fetch.finish()
	now := time.Now()
	var rows []availableRow
	var nextFree *time.Time
	var nextName string
	for _, profile := range profiles {
		meta := metadataFor(profile, entries)
		if !supported(profile, meta) {
			continue
		}
		switch profile.Kind {
		case store.Endpoint:
			diagnostic(stderr, profile.Name+": not listed; "+endpointNote(profile))
			continue
		case store.APIKey, store.ConsoleLogin:
			credit, err := readCredit(home, profileID(profile, meta))
			if err != nil {
				diagnostic(stderr, profile.Name+": "+err.Error())
				continue
			}
			if credit == nil {
				diagnostic(stderr, profile.Name+": not listed; set its Console credit to track it: claudock profile set-credit "+profile.Name+" AMOUNT")
				continue
			}
			if credit.left.Sign() <= 0 {
				continue
			}
			kind := "api-key"
			if profile.Kind == store.ConsoleLogin {
				kind = "console-login"
			}
			rows = append(rows, availableRow{profile: profile, kind: kind, plan: "Console API", left: credit.leftText(), limit: "Credit", resets: "-", rank: credit.rank()})
			continue
		case store.Subscription, "":
		default:
			continue
		}
		result := fetch.reading(profile, maxAge)
		if result.reading == nil {
			diagnostic(stderr, profile.Name+": not listed; "+result.reason.Error())
			continue
		}
		if result.reason != nil {
			diagnostic(stderr, profile.Name+": "+staleReason(result.reason)+"; using reading from "+age(result.reading.FetchedAt)+".")
		}
		if needsSignIn(result.reason) {
			continue
		}
		room := result.reading.headroom(now)
		if room == nil {
			continue
		}
		if room.left <= 0 {
			if room.again != nil && (nextFree == nil || room.again.Before(*nextFree)) {
				nextFree, nextName = room.again, profile.Name
			}
			continue
		}
		rows = append(rows, availableRow{profile: profile, kind: "subscription", plan: planName(result.reading.Plan), left: fmt.Sprintf("%.2f%%", room.left), limit: room.limit.Title, resets: resetText(room.limit.ResetsAt), rank: room.left})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].rank == rows[j].rank {
			return rows[i].profile.Name < rows[j].profile.Name
		}
		return rows[i].rank > rows[j].rank
	})
	if !namesOnly {
		row(stdout, "PROFILE", "KIND", "PLAN", "LEFT", "TIGHTEST_LIMIT", "RESETS_UTC")
	}
	for _, entry := range rows {
		if namesOnly {
			row(stdout, entry.profile.Name)
		} else {
			row(stdout, entry.profile.Name, entry.kind, entry.plan, entry.left, entry.limit, entry.resets)
		}
	}
	if len(rows) == 0 {
		message := "No profile has usage left."
		if nextFree != nil {
			message += " " + nextName + " is next free, at " + resetText(nextFree) + "."
		}
		diagnostic(stderr, message)
		return 1
	}
	return 0
}
