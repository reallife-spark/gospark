package understand

import (
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

func TestDiffReportsAddedAndRemoved(t *testing.T) {
	oldGraph := sampleGraph()
	newGraph := sampleGraph()
	newGraph.Nodes = append(newGraph.Nodes, Node{ID: "file:b.py", Type: "file", Name: "b.py", Summary: "new"})
	newGraph.Edges = append(newGraph.Edges, Edge{Source: "file:b.py", Target: "file:a.py", Type: "imports", Direction: "forward", Weight: 0.5})
	delta := Diff(oldGraph, newGraph)
	if len(delta.NodesAdded) != 1 || delta.NodesAdded[0] != "file:b.py" {
		t.Fatalf("nodes added = %v", delta.NodesAdded)
	}
	if len(delta.EdgesAdded) != 1 {
		t.Fatalf("edges added = %v", delta.EdgesAdded)
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
