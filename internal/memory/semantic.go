package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Embedder is the seam to the compute plane's embedding model. The memory
// package does not link a model; an OpenAI-compatible endpoint is enough.
type Embedder interface {
	Embed(ctx context.Context, inputs []string) ([][]float32, error)
	Health(ctx context.Context) error
}

// QdrantConfig configures the optional semantic index.
type QdrantConfig struct {
	URL        string
	APIKey     string
	Collection string
	Timeout    time.Duration
}

// QdrantIndex is a small Qdrant REST client. It is optional: when the endpoint
// is unreachable the service keeps using the lexical store and says so.
type QdrantIndex struct {
	base       string
	apiKey     string
	collection string
	embedder   Embedder
	client     *http.Client
}

// SemanticHit is one vector result.
type SemanticHit struct {
	ID      string         `json:"id"`
	Score   float64        `json:"score"`
	Payload map[string]any `json:"payload,omitempty"`
}

// NewQdrantIndex builds the client. A missing URL returns nil, not an error:
// lexical retrieval is the documented fallback.
func NewQdrantIndex(cfg QdrantConfig, embedder Embedder) *QdrantIndex {
	base := strings.TrimRight(strings.TrimSpace(cfg.URL), "/")
	if base == "" || embedder == nil {
		return nil
	}
	collection := strings.TrimSpace(cfg.Collection)
	if collection == "" {
		collection = "sparknet-memory"
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return &QdrantIndex{
		base:       base,
		apiKey:     cfg.APIKey,
		collection: collection,
		embedder:   embedder,
		client:     &http.Client{Timeout: timeout},
	}
}

// Collection returns the configured collection name.
func (q *QdrantIndex) Collection() string {
	if q == nil {
		return ""
	}
	return q.collection
}

// Health probes Qdrant and the embedding endpoint.
func (q *QdrantIndex) Health(ctx context.Context) error {
	if q == nil {
		return nil
	}
	if err := q.embedder.Health(ctx); err != nil {
		return fmt.Errorf("semantic: embedding endpoint: %w", err)
	}
	status, _, err := q.request(ctx, http.MethodGet, "", nil)
	if err != nil {
		return err
	}
	if status != http.StatusOK && status != http.StatusNotFound {
		return fmt.Errorf("semantic: qdrant status %d", status)
	}
	return nil
}

// Upsert embeds the record text and stores one point. A failure is returned to
// the caller so it can keep the lexical write and log the degraded state.
func (q *QdrantIndex) Upsert(ctx context.Context, record *Record) error {
	if q == nil || record == nil || record.Text == "" {
		return nil
	}
	vectors, err := q.embedder.Embed(ctx, []string{record.Text + " " + record.Summary})
	if err != nil {
		return err
	}
	if len(vectors) != 1 || len(vectors[0]) == 0 {
		return errors.New("semantic: embedder returned no vector")
	}
	dim := len(vectors[0])
	if err := q.ensure(ctx, dim); err != nil {
		return err
	}
	payload := map[string]any{
		"memory_id":       record.ID,
		"vault_id":        record.VaultID,
		"memory_type":     string(record.Type),
		"subtype":         record.Subtype,
		"source_kind":     string(record.SourceKind),
		"source_ref":      record.SourceRef,
		"user_id":         record.Scope.UserID,
		"project_id":      record.Scope.ProjectID,
		"thread_id":       record.Scope.ThreadID,
		"workflow_id":     record.Scope.WorkflowID,
		"agent_id":        record.Scope.AgentID,
		"retention_state": string(record.RetentionState),
		"content_hash":    record.ContentHash,
	}
	body := map[string]any{
		"points": []map[string]any{{
			"id":      pointID(record.ID),
			"vector":  vectors[0],
			"payload": payload,
		}},
	}
	status, raw, err := q.request(ctx, http.MethodPut, "/points?wait=true", body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("semantic: upsert status %d: %s", status, truncate(string(raw), 300))
	}
	return nil
}

// Search returns vector hits scoped to one vault. The filter is applied by
// Qdrant before top-k, not after it.
func (q *QdrantIndex) Search(
	ctx context.Context,
	vaultID, query string,
	types []Type,
	scope Scope,
	topK int,
) ([]SemanticHit, error) {
	if q == nil || strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if topK <= 0 {
		topK = 8
	}
	if topK > 50 {
		topK = 50
	}
	vectors, err := q.embedder.Embed(ctx, []string{query})
	if err != nil {
		return nil, err
	}
	if len(vectors) != 1 || len(vectors[0]) == 0 {
		return nil, errors.New("semantic: embedder returned no query vector")
	}
	if err := q.ensure(ctx, len(vectors[0])); err != nil {
		return nil, err
	}
	must := []map[string]any{
		{"key": "vault_id", "match": map[string]any{"value": vaultID}},
		{"key": "retention_state", "match": map[string]any{"value": string(Active)}},
	}
	if len(types) > 0 {
		values := make([]string, 0, len(types))
		for _, t := range types {
			values = append(values, string(t))
		}
		must = append(must, map[string]any{"key": "memory_type", "match": map[string]any{"any": values}})
	}
	for key, value := range map[string]string{
		"user_id": scope.UserID, "project_id": scope.ProjectID,
		"thread_id": scope.ThreadID, "workflow_id": scope.WorkflowID,
		"agent_id": scope.AgentID,
	} {
		if value != "" {
			must = append(must, map[string]any{"key": key, "match": map[string]any{"value": value}})
		}
	}
	body := map[string]any{
		"vector":       vectors[0],
		"limit":        topK,
		"with_payload": true,
		"filter":       map[string]any{"must": must},
	}
	status, raw, err := q.request(ctx, http.MethodPost, "/points/search", body)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("semantic: search status %d: %s", status, truncate(string(raw), 300))
	}
	var parsed struct {
		Result []struct {
			ID      any            `json:"id"`
			Score   float64        `json:"score"`
			Payload map[string]any `json:"payload"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, err
	}
	hits := make([]SemanticHit, 0, len(parsed.Result))
	for _, item := range parsed.Result {
		payload := item.Payload
		memoryID, _ := payload["memory_id"].(string)
		if memoryID == "" {
			continue
		}
		hits = append(hits, SemanticHit{ID: memoryID, Score: item.Score, Payload: payload})
	}
	return hits, nil
}

// Delete removes points for record ids. It is best-effort: the lexical store is
// already authoritative and the caller logs a degraded index.
func (q *QdrantIndex) Delete(ctx context.Context, ids []string) error {
	if q == nil || len(ids) == 0 {
		return nil
	}
	points := make([]string, 0, len(ids))
	for _, id := range ids {
		points = append(points, pointID(id))
	}
	body := map[string]any{"points": points}
	status, raw, err := q.request(ctx, http.MethodPost, "/points/delete?wait=true", body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("semantic: delete status %d: %s", status, truncate(string(raw), 300))
	}
	return nil
}

func (q *QdrantIndex) ensure(ctx context.Context, dim int) error {
	status, raw, err := q.request(ctx, http.MethodGet, "", nil)
	if err != nil {
		return err
	}
	if status == http.StatusOK {
		// The collection already exists. Its dimension is the operator's
		// contract; a mismatch is refused by Qdrant rather than hidden.
		return nil
	}
	if status != http.StatusNotFound {
		return fmt.Errorf("semantic: collection probe status %d: %s", status, truncate(string(raw), 300))
	}
	body := map[string]any{
		"vectors": map[string]any{"size": dim, "distance": "Cosine"},
	}
	status, raw, err = q.request(ctx, http.MethodPut, "", body)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("semantic: create status %d: %s", status, truncate(string(raw), 300))
	}
	return nil
}

func (q *QdrantIndex) request(ctx context.Context, method, suffix string, body any) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		reader = bytes.NewReader(raw)
	}
	url := q.base + "/collections/" + q.collection + suffix
	request, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return 0, nil, err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if q.apiKey != "" {
		request.Header.Set("api-key", q.apiKey)
	}
	response, err := q.client.Do(request)
	if err != nil {
		return 0, nil, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil {
		return response.StatusCode, nil, err
	}
	return response.StatusCode, raw, nil
}

func pointID(id string) string {
	sum := sha256.Sum256([]byte(id))
	hexed := hex.EncodeToString(sum[:16])
	return hexed[0:8] + "-" + hexed[8:12] + "-" + hexed[12:16] + "-" + hexed[16:20] + "-" + hexed[20:32]
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
