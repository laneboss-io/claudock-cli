package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestVertexConfigurationValidation(t *testing.T) {
	for _, tc := range []struct {
		project, region string
		valid           bool
	}{
		{"example-project", "us-east5", true}, {"example-project", "global", true},
		{"bad", "global", false}, {"Bad Project", "global", false},
		{"example-project", "US EAST", false}, {"example-project", "", false},
	} {
		err := ValidateVertex(VertexConfiguration{Project: tc.project, Region: tc.region})
		if (err == nil) != tc.valid {
			t.Errorf("ValidateVertex(%q,%q) = %v", tc.project, tc.region, err)
		}
	}
}

func TestVertexRegistryCompatibility(t *testing.T) {
	original := []byte(`{"version":4,"profiles":[{"command":"claude","configDirectory":"/home/test/.claude","registryID":"11111111-1111-4111-8111-111111111111","managed":false,"isVertex":false},{"command":"claude-cloud","configDirectory":"/home/test/cloud","registryID":"22222222-2222-4222-8222-222222222222","managed":true,"isVertex":false,"authKind":"vertex","vertex":{"project":"example-project","region":"us-east5"}}],"suppressedCommands":[],"suppressedDirectories":[],"subscriptionOnlyMigration":1}`)
	registry, err := decodeRegistry(original, false)
	if err != nil {
		t.Fatal(err)
	}
	p := registry.Profiles[1]
	if p.Kind != Vertex || p.Vertex == nil || p.Vertex.Project != "example-project" || p.Vertex.Region != "us-east5" {
		t.Fatalf("bad Vertex profile: %#v", p)
	}
	encoded, err := encodeRegistry(registry, false)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(encoded, &object); err != nil {
		t.Fatal(err)
	}
	if object["version"] != float64(4) {
		t.Fatalf("registry version = %v", object["version"])
	}
	decoded, err := decodeRegistry(encoded, false)
	if err != nil || !decoded.Profiles[1].Equal(p) {
		t.Fatalf("round trip: %v %#v", err, decoded)
	}
	endpoint := Profile{Command: "claude-endpoint", ConfigDirectory: "/home/test/endpoint", RegistryID: "33333333-3333-4333-8333-333333333333", Managed: true, Kind: Endpoint, Endpoint: &EndpointConfiguration{BaseURL: "https://api.example.com", Model: "model"}}
	registry.Profiles = append([]Profile{registry.Profiles[0], endpoint}, registry.Profiles[1])
	encoded, err = encodeRegistry(registry, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(encoded, &object)
	if object["version"] != float64(4) {
		t.Fatalf("endpoint before Vertex incorrectly selected version %v", object["version"])
	}
	decoded.Profiles[1].Vertex = nil
	if _, err := encodeRegistry(decoded, false); err == nil {
		t.Fatal("accepted missing Vertex configuration")
	}
	registry.Profiles = registry.Profiles[:1]
	encoded, err = encodeRegistry(registry, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(encoded, &object)
	if object["version"] != float64(1) {
		t.Fatalf("registry version after removal = %v", object["version"])
	}
}

// A Swift-created registry must stay readable without a migration or rewrite.
func TestVertexSnapshotDoesNotRewriteCurrentRegistry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CLAUDOCK_REGISTRY_DIR", dir)
	t.Setenv("CLAUDOCK_REGISTRY", "")
	data := []byte(`{"version":4,"profiles":[{"command":"claude","configDirectory":"/home/test/.claude","registryID":"11111111-1111-4111-8111-111111111111","managed":false,"isVertex":false},{"command":"claude-cloud","configDirectory":"/home/test/cloud","registryID":"22222222-2222-4222-8222-222222222222","managed":true,"isVertex":false,"authKind":"vertex","vertex":{"project":"example-project","region":"global"}}],"suppressedCommands":[],"suppressedDirectories":[],"subscriptionOnlyMigration":1}`)
	path := filepath.Join(dir, "profiles.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := readSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.needsWrite {
		t.Fatal("current version 4 registry was marked for rewriting")
	}
	if len(snapshot.main.Profiles) != 2 || snapshot.main.Profiles[1].Kind != Vertex {
		t.Fatal("Vertex profile was lost")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(data) {
		t.Fatal("registry changed on read")
	}
}

func TestVertexProfileLifecycleWithSharedLedger(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CLAUDOCK_REGISTRY", "")
	t.Setenv("CLAUDOCK_REGISTRY_DIR", filepath.Join(home, "registry"))
	profile, err := Add(AddOptions{Name: "cloud", Kind: Vertex, Vertex: &VertexConfiguration{Project: "example-project", Region: "us-east5"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SetCredit(profile, "100.00"); err != nil {
		t.Fatal(err)
	}
	renamed, err := Rename(profile, "cloud2")
	if err != nil {
		t.Fatal(err)
	}
	if renamed.ID() != profile.ID() || renamed.Vertex == nil || *renamed.Vertex != *profile.Vertex {
		t.Fatal("rename lost Vertex identity or settings")
	}
	profiles, err := List()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, p := range profiles {
		if p.ID() == renamed.ID() {
			found = p.Equal(renamed)
		}
	}
	if !found {
		t.Fatal("renamed Vertex profile not listed")
	}
	if err := Remove(renamed); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "registry", "api-credit.json")); err != nil {
		t.Fatal("removal lost the shared credit ledger")
	}
}
