package profilecmd

import (
	"github.com/laneboss-io/claudock-cli/internal/store"
	"testing"
)

func TestVertexProfileArguments(t *testing.T) {
	got, err := parse([]string{"add", "cloud", "--region", "global", "--vertex", "--project", "example-project"})
	if err != nil || got.kind != store.Vertex || got.project != "example-project" || got.region != "global" {
		t.Fatalf("parse = %#v, %v", got, err)
	}
	for _, args := range [][]string{
		{"add", "cloud", "--vertex", "--project", "example-project"},
		{"add", "cloud", "--project", "example-project", "--region", "global"},
		{"add", "cloud", "--vertex", "--console", "--project", "example-project", "--region", "global"},
		{"add", "cloud", "--vertex", "--model", "foo", "--project", "example-project", "--region", "global"},
	} {
		if _, err := parse(args); err == nil {
			t.Errorf("accepted invalid arguments %#v", args)
		}
	}
}
