package foundations

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type vector struct {
	Name           string          `json:"name"`
	Input          json.RawMessage `json:"input"`
	CanonicalInput json.RawMessage `json:"canonical_input"`
	Canonical      string          `json:"canonical"`
}

type vectorFile struct {
	SchemaVersion string   `json:"schema_version"`
	Vectors       []vector `json:"vectors"`
}

func loadVectors(t *testing.T) vectorFile {
	t.Helper()
	path := filepath.Join("..", "..", "spec", "sparknet", "foundations-vectors.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read vectors %s: %v", path, err)
	}
	var file vectorFile
	if err := json.Unmarshal(raw, &file); err != nil {
		t.Fatalf("parse vectors: %v", err)
	}
	if file.SchemaVersion != "sparknet-foundation-vectors/1" {
		t.Fatalf("unexpected vector schema %q", file.SchemaVersion)
	}
	if len(file.Vectors) == 0 {
		t.Fatal("vector file is empty")
	}
	return file
}

func TestCanonicalVectorsMatchPython(t *testing.T) {
	for _, item := range loadVectors(t).Vectors {
		var input any
		if err := json.Unmarshal(item.CanonicalInput, &input); err != nil {
			t.Fatalf("%s: canonical_input is not JSON: %v", item.Name, err)
		}
		got, err := CanonicalString(input)
		if err != nil {
			t.Fatalf("%s: %v", item.Name, err)
		}
		if got != item.Canonical {
			t.Errorf("%s: canonical bytes differ\n got: %s\nwant: %s", item.Name, got, item.Canonical)
		}
	}
}

func TestCanonicalJSONSortsKeysAndKeepsUTF8(t *testing.T) {
	got, err := CanonicalString(map[string]any{"b": 1, "a": "Über <ok> →"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":"Über <ok> →","b":1}`
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	if strings.Contains(got, `\u003c`) || strings.Contains(got, `\u003e`) || strings.Contains(got, `\u0026`) {
		t.Fatalf("canonical JSON HTML-escaped a value: %s", got)
	}
}

func TestCanonicalJSONRejectsNaN(t *testing.T) {
	if _, err := CanonicalJSON(map[string]any{"score": math.NaN()}); err == nil {
		t.Fatal("NaN must be refused")
	}
	if _, err := CanonicalJSON(map[string]any{"score": math.Inf(1)}); err == nil {
		t.Fatal("Infinity must be refused")
	}
}
