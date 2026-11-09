package understand

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPInitializeAndToolsList(t *testing.T) {
	graphPath := filepath.Join(t.TempDir(), "graph.json")
	if err := SaveGraph(graphPath, sampleGraph()); err != nil {
		t.Fatal(err)
	}
	input := strings.NewReader(
		"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n" +
			"{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\"}\n",
	)
	var output bytes.Buffer
	if err := ServeMCP(graphPath, input, &output); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("responses = %d: %s", len(lines), output.String())
	}
	var initialize map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &initialize); err != nil {
		t.Fatal(err)
	}
	if initialize["result"].(map[string]any)["serverInfo"].(map[string]any)["name"] != "sparknet-understand" {
		t.Fatalf("initialize = %v", initialize)
	}
	var tools map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &tools); err != nil {
		t.Fatal(err)
	}
	if len(tools["result"].(map[string]any)["tools"].([]any)) != 3 {
		t.Fatalf("tools = %v", tools)
	}
}
