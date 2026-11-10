package understand

import (
	"context"
	"testing"
)

func TestGoFragmentsUsesTheStandardLibraryParser(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "main.go", `package main

import "fmt"

type Server struct {
	Name string
}

func (s *Server) Handle() {
	fmt.Println(s.Name)
	helper()
}

func helper() {}
`)
	result, err := Scan(context.Background(), "python3", sharedScript(t), root, nil)
	if err != nil {
		t.Fatal(err)
	}
	fragments, err := GoFragments(root, result.Files)
	if err != nil {
		t.Fatal(err)
	}
	if len(fragments) != 1 {
		t.Fatalf("fragments = %d, want 1", len(fragments))
	}
	fragment := fragments[0]
	nodes := map[string]Node{}
	for _, node := range fragment.Nodes {
		nodes[node.ID] = node
	}
	if _, ok := nodes["file:main.go"]; !ok {
		t.Fatal("missing file node")
	}
	if _, ok := nodes["class:main.go:Server:5"]; !ok {
		t.Fatalf("missing Server class: %v", nodes)
	}
	var calls, serves bool
	for _, edge := range fragment.Edges {
		if edge.Type == "calls" {
			calls = true
		}
		if edge.Type == "serves" {
			serves = true
		}
	}
	if !calls {
		t.Fatal("expected a calls edge")
	}
	if !serves {
		t.Fatal("expected an endpoint serves edge")
	}
}
