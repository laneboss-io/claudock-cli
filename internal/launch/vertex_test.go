package launch

import (
	"github.com/laneboss-io/claudock-cli/internal/store"
	"os"
	"path/filepath"
	"testing"
)

func TestVertexLaunchEnvironment(t *testing.T) {
	p := store.Profile{Name: "cloud", Managed: true, Kind: store.Vertex, Vertex: &store.VertexConfiguration{Project: "example-project", Region: "us-east5"}}
	env := isolatedEnvironment([]string{"ANTHROPIC_API_KEY=wrong", "CLAUDE_CODE_USE_BEDROCK=1", "GOOGLE_APPLICATION_CREDENTIALS=/tmp/adc.json", "ANTHROPIC_VERTEX_PROJECT_ID=old", "ANTHROPIC_VERTEX_BASE_URL=https://wrong.invalid", "CLAUDE_CODE_SKIP_VERTEX_AUTH=1", "VERTEX_REGION_CLAUDE_SONNET=wrong", "GOOGLE_CLOUD_PROJECT=other-project"})
	if err := credentialEnvironment(p, nil, env); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"CLAUDE_CODE_USE_VERTEX": "1", "ANTHROPIC_VERTEX_PROJECT_ID": "example-project", "CLOUD_ML_REGION": "us-east5", "GOOGLE_APPLICATION_CREDENTIALS": "/tmp/adc.json"} {
		if env[k] != want {
			t.Errorf("%s = %q, want %q", k, env[k], want)
		}
	}
	if env["ANTHROPIC_API_KEY"] != "" || env["CLAUDE_CODE_USE_BEDROCK"] != "" {
		t.Fatal("inherited provider credentials were not cleared")
	}
	if env["ANTHROPIC_VERTEX_BASE_URL"] != "" || env["CLAUDE_CODE_SKIP_VERTEX_AUTH"] != "" || env["VERTEX_REGION_CLAUDE_SONNET"] != "" || env["GOOGLE_CLOUD_PROJECT"] != "example-project" {
		t.Fatal("inherited Vertex overrides were not isolated")
	}
	p.Vertex = nil
	if err := credentialEnvironment(p, nil, env); err == nil {
		t.Fatal("missing configuration accepted")
	}
}

func TestVertexConfigRejectsProviderOverrides(t *testing.T) {
	dir := t.TempDir()
	p := store.Profile{Name: "cloud", ConfigDirectory: dir, Managed: true, Kind: store.Vertex, Vertex: &store.VertexConfiguration{Project: "example-project", Region: "us-east5"}}
	for _, tc := range []struct {
		content string
		valid   bool
	}{
		{`{"env":{"ANTHROPIC_VERTEX_PROJECT_ID":"example-project","CLOUD_ML_REGION":"us-east5"}}`, true},
		{`{"env":{"ANTHROPIC_VERTEX_PROJECT_ID":"other-project"}}`, false},
		{`{"env":{"CLOUD_ML_REGION":"global"}}`, false},
		{`{"env":{"ANTHROPIC_VERTEX_BASE_URL":"https://other.invalid"}}`, false},
		{`{"env":{"CLAUDE_CODE_SKIP_VERTEX_AUTH":"1"}}`, false},
		{`{"env":{"VERTEX_REGION_CLAUDE_SONNET":"global"}}`, false},
		{`{"env":{"GOOGLE_CLOUD_PROJECT":"other-project"}}`, false},
	} {
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(tc.content), 0600); err != nil {
			t.Fatal(err)
		}
		err := validateConfig(p)
		if (err == nil) != tc.valid {
			t.Fatalf("settings %s: got %v", tc.content, err)
		}
	}
}

func TestVertexLaunchUsesProfileADC(t *testing.T) {
	adc := filepath.Join(t.TempDir(), "adc.json")
	if err := os.WriteFile(adc, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	p := store.Profile{Name: "cloud", Managed: true, Kind: store.Vertex, Vertex: &store.VertexConfiguration{Project: "example-project", Region: "us-east5", ADC: adc}}
	env := isolatedEnvironment([]string{"GOOGLE_APPLICATION_CREDENTIALS=/tmp/other.json"})
	if err := credentialEnvironment(p, nil, env); err != nil {
		t.Fatal(err)
	}
	if env["GOOGLE_APPLICATION_CREDENTIALS"] != adc {
		t.Fatalf("ADC = %q", env["GOOGLE_APPLICATION_CREDENTIALS"])
	}
	p.Vertex.ADC = filepath.Join(filepath.Dir(adc), "missing.json")
	if err := credentialEnvironment(p, nil, env); err == nil {
		t.Fatal("missing ADC file accepted")
	}
}
