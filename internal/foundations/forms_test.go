package foundations

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

type foundationVector struct {
	Name           string          `json:"name"`
	CanonicalInput json.RawMessage `json:"canonical_input"`
	Canonical      string          `json:"canonical"`
}

func TestTypedFormsMatchCanonicalVectors(t *testing.T) {
	path := filepath.Join("..", "..", "spec", "sparknet", "foundations-vectors.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read vectors: %v", err)
	}
	var file struct {
		Vectors []foundationVector `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("decode vectors: %v", err)
	}
	targets := map[string]func() any{
		"event.message":   func() any { return &EventEnvelope{} },
		"graph.graph":     func() any { return &Graph{} },
		"hybrid.bundle":   func() any { return &HybridBundle{} },
		"exec.request":    func() any { return &ExecRequest{} },
		"collab.room":     func() any { return &CollabRoom{} },
		"collab.message":  func() any { return &CollabMessage{} },
		"package.package": func() any { return &PackageManifest{} },
		"package.index":   func() any { return &PackageIndex{} },
	}
	for _, vector := range file.Vectors {
		newTarget, ok := targets[vector.Name]
		if !ok {
			continue
		}
		t.Run(vector.Name, func(t *testing.T) {
			target := newTarget()
			if err := json.Unmarshal(vector.CanonicalInput, target); err != nil {
				t.Fatalf("decode canonical_input: %v", err)
			}
			got, err := CanonicalJSON(target)
			if err != nil {
				t.Fatalf("canonicalise: %v", err)
			}
			if string(got) != vector.Canonical {
				t.Fatalf("typed form changed the bytes\n got: %s\nwant: %s", got, vector.Canonical)
			}
		})
	}
}
