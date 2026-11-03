package memory

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeEmbedder struct{}

func (fakeEmbedder) Embed(_ context.Context, inputs []string) ([][]float32, error) {
	out := make([][]float32, len(inputs))
	for i := range inputs {
		out[i] = []float32{1, 0, 0}
	}
	return out, nil
}

func (fakeEmbedder) Health(context.Context) error { return nil }

func TestQdrantUpsertAndScopedSearch(t *testing.T) {
	var created bool
	var points map[string]any
	var search map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/collections/mem":
			if !created {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"status":{"error":"not found"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"result":{"config":{"params":{"vectors":{"size":3,"distance":"Cosine"}}}}}`))
		case r.Method == http.MethodPut && r.URL.Path == "/collections/mem":
			created = true
			_, _ = w.Write([]byte(`{"result":true}`))
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/points"):
			_ = json.NewDecoder(r.Body).Decode(&points)
			_, _ = w.Write([]byte(`{"result":{"status":"completed"}}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/points/search"):
			_ = json.NewDecoder(r.Body).Decode(&search)
			_, _ = w.Write([]byte(`{"result":[{"id":"x","score":0.91,"payload":{"memory_id":"rec-1","vault_id":"v1"}}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"status":{"error":"unknown"}}`))
		}
	}))
	defer server.Close()

	index := NewQdrantIndex(QdrantConfig{URL: server.URL, Collection: "mem"}, fakeEmbedder{})
	record := &Record{
		ID: "rec-1", VaultID: "v1", Type: Episodic, Text: "release plan", RetentionState: Active,
		Scope: Scope{UserID: "alice", ProjectID: "alpha"},
	}
	if err := index.Upsert(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if points == nil {
		t.Fatal("upsert did not reach qdrant")
	}
	if len(points["points"].([]any)) != 1 {
		t.Fatalf("points = %+v", points)
	}
	hits, err := index.Search(context.Background(), "v1", "release", []Type{Episodic}, Scope{UserID: "alice", ProjectID: "alpha"}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != "rec-1" || hits[0].Score != 0.91 {
		t.Fatalf("hits = %+v", hits)
	}
	filter := search["filter"].(map[string]any)
	must := filter["must"].([]any)
	if len(must) < 3 {
		t.Fatalf("filter did not narrow vault/type/user: %+v", must)
	}
	foundVault := false
	for _, item := range must {
		entry := item.(map[string]any)
		if entry["key"] == "vault_id" {
			foundVault = true
		}
	}
	if !foundVault {
		t.Fatalf("vault filter missing: %+v", must)
	}
}

func TestPointIDIsStableUUID(t *testing.T) {
	one, two := pointID("abc"), pointID("abc")
	if one != two || len(one) != 36 {
		t.Fatalf("point id = %q / %q", one, two)
	}
	if pointID("abc") == pointID("abd") {
		t.Fatal("different records share a point id")
	}
}
