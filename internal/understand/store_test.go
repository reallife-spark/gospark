package understand

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sampleGraph() Graph {
	return Graph{
		Version: "1.0",
		Kind:    "codebase",
		Nodes: []Node{
			{ID: "file:a.py", Type: "file", Name: "a.py", Summary: "api module"},
			{ID: "function:a.py:handler:3", Type: "function", Name: "handler", Summary: "handle a request"},
			{ID: "module:http", Type: "module", Name: "http", Summary: "http library"},
		},
		Edges: []Edge{
			{Source: "file:a.py", Target: "function:a.py:handler:3", Type: "contains", Direction: "forward", Weight: 0.5},
			{Source: "function:a.py:handler:3", Target: "module:http", Type: "imports", Direction: "forward", Weight: 0.5},
		},
	}
}

func TestQueryRanksNameMatches(t *testing.T) {
	results := Query(sampleGraph(), "handler", 10)
	if len(results) == 0 || results[0].ID != "function:a.py:handler:3" {
		t.Fatalf("unexpected results: %v", results)
	}
}

func TestNeighborsAndShortestPath(t *testing.T) {
	graph := sampleGraph()
	if got := Neighbors(graph, "function:a.py:handler:3", "both"); len(got) != 2 {
		t.Fatalf("neighbors = %v", got)
	}
	path := ShortestPath(graph, "file:a.py", "module:http")
	if strings.Join(path, ">") != "file:a.py>function:a.py:handler:3>module:http" {
		t.Fatalf("path = %v", path)
	}
}

func TestLoadAndSavePreserveRawGraphFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "graph.json")
	raw := `{
  "version": "1.0",
  "kind": "codebase",
  "project": {"name": "fixture"},
  "nodes": [
    {
      "id": "file:a.py",
      "type": "file",
      "name": "a.py",
      "summary": "",
      "tags": [],
      "complexity": "moderate",
      "futureField": {"kept": true}
    }
  ],
  "edges": [],
  "layers": [],
  "tour": [
    {
      "order": 1,
      "title": "a.py",
      "description": "",
      "nodeIds": ["file:a.py"],
      "languageLesson": "Keep this optional field."
    }
  ]
}`
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	graph, err := LoadGraph(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(graph.Tour) != 1 || graph.Tour[0].LanguageLesson != "Keep this optional field." {
		t.Fatalf("languageLesson was not loaded: %#v", graph.Tour)
	}
	encoded, err := json.Marshal(graph)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "futureField") {
		t.Fatal("unknown node field was dropped")
	}
	if !strings.Contains(string(encoded), "languageLesson") {
		t.Fatal("languageLesson was dropped")
	}
	second := filepath.Join(t.TempDir(), "saved.json")
	if err := SaveGraph(second, graph); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "futureField") || !strings.Contains(string(saved), "languageLesson") {
		t.Fatalf("saved graph lost optional fields: %s", saved)
	}
}

func TestExportsAreText(t *testing.T) {
	graph := sampleGraph()
	if !strings.Contains(ExportDOT(graph), `"function:a.py:handler:3"`) {
		t.Fatal("DOT export missing node")
	}
	if !strings.Contains(ExportMermaid(graph), "-->") {
		t.Fatal("Mermaid export missing edge")
	}
}
