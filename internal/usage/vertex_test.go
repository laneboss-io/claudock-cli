package usage

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/laneboss-io/claudock-cli/internal/store"
)

func TestVertexBudgetReadAndAvailability(t *testing.T) {
	home := t.TempDir()
	dir := dataDirectory(home)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ledger := `{"version":1,"profiles":{"22222222-2222-4222-8222-222222222222":{"balanceUSD":100,"asOf":"2026-11-01T00:00:00.000Z","requests":[{"id":"one","sessionID":"session","at":"2026-11-02T00:00:00.000Z","costUSD":1.25}],"adjustments":[]}}}`
	if err := os.WriteFile(filepath.Join(dir, "api-credit.json"), []byte(ledger), 0600); err != nil {
		t.Fatal(err)
	}
	profile := store.Profile{Name: "cloud", Command: "claude-cloud", ConfigDirectory: "/tmp/cloud", Kind: store.Vertex, RegistryID: "22222222-2222-4222-8222-222222222222"}
	var out, stderr bytes.Buffer
	if status := showUsage(home, []store.Profile{profile}, nil, time.Minute, &out, &stderr); status != 0 {
		t.Fatalf("usage: %d %s", status, stderr.String())
	}
	if !strings.Contains(out.String(), "cloud\tVertex AI\tCredit · $98.75 of $100.00 left\t1.25\t-") {
		t.Fatalf("usage output %q", out.String())
	}
	if strings.Contains(stderr.String(), "do not update") {
		t.Fatal("usage still reports that launches do not update the budget")
	}
	out.Reset()
	stderr.Reset()
	if status := showAvailable(home, []store.Profile{profile}, nil, time.Minute, false, &out, &stderr); status != 0 {
		t.Fatalf("available: %d %s", status, stderr.String())
	}
	if !strings.Contains(out.String(), "cloud\tvertex\tVertex AI\t$98.75") {
		t.Fatalf("available output %q", out.String())
	}
	if strings.Contains(stderr.String(), "do not update") {
		t.Fatal("available still reports that launches do not update the budget")
	}
}

func TestVertexRegistryIDFallbackAlsoAppliesToConsole(t *testing.T) {
	p := store.Profile{Command: "claude-console", Kind: store.ConsoleLogin, RegistryID: "22222222-2222-4222-8222-222222222222"}
	if got := profileID(p, profileMetadata{}); got != p.RegistryID {
		t.Fatalf("ledger ID = %q", got)
	}
}
