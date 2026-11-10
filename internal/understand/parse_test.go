package understand

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestParseWritesThePythonGraph(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "app.py", "def greet():\n    return 'hi'\n")
	out := filepath.Join(t.TempDir(), "knowledge-graph.json")
	if err := Parse(context.Background(), "python3", sharedScript(t), root, out); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) == 0 {
		t.Fatal("parse wrote an empty graph")
	}
}
