package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRecordCreditLedger(t *testing.T) {
	base := t.TempDir()
	t.Setenv("CLAUDOCK_REGISTRY", "")
	t.Setenv("CLAUDOCK_REGISTRY_DIR", base)
	p := Profile{Name: "cloud", Command: "claude-cloud", Kind: Vertex, RegistryID: "22222222-2222-4222-8222-222222222222"}
	model := "claude-test"
	asOf := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	requests := []CreditRequest{
		{ID: "new", SessionID: "session", At: asOf.Add(time.Hour + 1234567*time.Nanosecond), CostUSD: 0.173218, Model: &model},
		{ID: "old", SessionID: "session", At: asOf.Add(-time.Second), CostUSD: 5},
		{ID: "seen", SessionID: "session", At: asOf.Add(time.Hour), CostUSD: 7},
		{ID: "new", SessionID: "session", At: asOf.Add(time.Hour), CostUSD: 0.173218},
	}
	if added, err := RecordCredit(p, requests); err != nil || added != 0 {
		t.Fatalf("without a ledger: added %d, %v", added, err)
	}
	if _, err := os.Lstat(filepath.Join(base, "api-credit.json")); !os.IsNotExist(err) {
		t.Fatal("recording created a ledger")
	}
	ledger := `{"version":1,"profiles":{"other":{"balanceUSD":5,"asOf":"2026-11-01T00:00:00.000Z","requests":[],"adjustments":[]},"22222222-2222-4222-8222-222222222222":{"balanceUSD":100,"asOf":"2026-11-01T00:00:00.000Z","requests":[{"id":"seen","sessionID":"session","at":"2026-11-02T00:00:00.000Z","costUSD":1.25}],"adjustments":[]}}}`
	path := filepath.Join(base, "api-credit.json")
	if err := os.WriteFile(path, []byte(ledger), 0600); err != nil {
		t.Fatal(err)
	}
	if added, err := RecordCredit(Profile{Name: "none", Command: "claude-none", Kind: Vertex}, requests); err != nil || added != 0 {
		t.Fatalf("profile without an account: added %d, %v", added, err)
	}
	if added, err := RecordCredit(p, requests); err != nil || added != 1 {
		t.Fatalf("added %d, %v", added, err)
	}
	if added, err := RecordCredit(p, requests); err != nil || added != 0 {
		t.Fatalf("second record added %d, %v", added, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Version  int                        `json:"version"`
		Profiles map[string]json.RawMessage `json:"profiles"`
	}
	if err := json.Unmarshal(data, &root); err != nil || root.Version != 1 || !validCreditAccounts(root.Profiles) {
		t.Fatalf("ledger unreadable after record: %v %s", err, data)
	}
	var account struct {
		BalanceUSD float64          `json:"balanceUSD"`
		AsOf       string           `json:"asOf"`
		Requests   []map[string]any `json:"requests"`
		Other      []map[string]any `json:"adjustments"`
	}
	if err := json.Unmarshal(root.Profiles[p.ID()], &account); err != nil {
		t.Fatal(err)
	}
	if account.BalanceUSD != 100 || account.AsOf != "2026-11-01T00:00:00.000Z" || len(account.Requests) != 2 || account.Other == nil {
		t.Fatalf("account changed unexpectedly: %+v", account)
	}
	want := map[string]any{"id": "new", "sessionID": "session", "at": "2026-11-01T01:00:00.001Z", "costUSD": 0.173218, "model": "claude-test"}
	got := account.Requests[1]
	if len(got) != len(want) {
		t.Fatalf("request = %v", got)
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("request %s = %v, want %v", key, got[key], value)
		}
	}
	if string(root.Profiles["other"]) != `{"balanceUSD":5,"asOf":"2026-11-01T00:00:00.000Z","requests":[],"adjustments":[]}` {
		t.Fatalf("other account changed: %s", root.Profiles["other"])
	}
	if err := os.WriteFile(path, []byte(`{"version":2,"profiles":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RecordCredit(p, requests[:1]); err == nil {
		t.Fatal("recorded into an unreadable ledger")
	}
}
