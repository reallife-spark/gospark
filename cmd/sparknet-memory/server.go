package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/reallife-spark/goSpark/internal/delegates"
	"github.com/reallife-spark/goSpark/internal/memory"
)

type service struct {
	cfg      Config
	manager  *memory.Manager
	log      *slog.Logger
	semantic *memory.QdrantIndex

	brokerMu sync.Mutex
	brokers  map[string]*brokerEntry
}

type brokerEntry struct {
	store  *delegates.Store
	broker *delegates.Broker
}

func newService(cfg Config, manager *memory.Manager, logger *slog.Logger) *service {
	return &service{
		cfg:     cfg,
		manager: manager,
		log:     logger,
		brokers: map[string]*brokerEntry{},
	}
}

func (s *service) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.health)
	mux.HandleFunc("/readyz", s.ready)
	mux.Handle("/v1/", s.authorized(http.HandlerFunc(s.route)))
	return mux
}

func (s *service) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "service": "sparknet-memory", "schema": memory.Schema})
}

func (s *service) ready(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "vaults": len(s.manager.Vaults())})
}

func (s *service) authorized(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		provided := r.Header.Get("X-Sparknet-Memory-Token")
		if subtle.ConstantTimeCompare([]byte(provided), []byte(s.cfg.GatewayToken)) != 1 {
			writeError(w, http.StatusUnauthorized, "unauthorized", "the local gateway token is required")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, s.cfg.MaxRequestBytes)
		next.ServeHTTP(w, r)
	})
}

func (s *service) route(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case r.Method == http.MethodPost && path == "/v1/memory/remember":
		s.remember(w, r)
	case r.Method == http.MethodPost && path == "/v1/memory/query":
		s.query(w, r, true)
	case r.Method == http.MethodPost && path == "/v1/memory/inspect":
		s.query(w, r, false)
	case r.Method == http.MethodPost && path == "/v1/memory/forget":
		s.forget(w, r)
	case r.Method == http.MethodPost && path == "/v1/memory/prune":
		s.prune(w, r)
	case r.Method == http.MethodPost && path == "/v1/memory/index":
		s.indexVault(w, r)
	case r.Method == http.MethodGet && path == "/v1/memory/policy":
		s.getPolicy(w, r)
	case r.Method == http.MethodPost && path == "/v1/memory/policy":
		s.setPolicy(w, r)
	case r.Method == http.MethodGet && path == "/v1/memory/status":
		s.status(w, r)
	case r.Method == http.MethodGet && path == "/v1/memory/list":
		s.list(w, r)
	case r.Method == http.MethodPost && path == "/v1/memory/export":
		s.export(w, r)
	case r.Method == http.MethodPost && path == "/v1/memory/import":
		s.importBundle(w, r)
	case r.Method == http.MethodPost && path == "/v1/delegates/plan":
		s.startPlan(w, r)
	case r.Method == http.MethodGet && path == "/v1/delegates/runs":
		s.listRuns(w, r)
	case strings.HasPrefix(path, "/v1/delegates/runs/"):
		s.runRoute(w, r, strings.TrimPrefix(path, "/v1/delegates/runs/"))
	case strings.HasPrefix(path, "/v1/delegates/plans/") && strings.HasSuffix(path, "/cancel"):
		s.cancelPlan(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/v1/delegates/plans/"), "/cancel"))
	default:
		writeError(w, http.StatusNotFound, "not-found", "unknown memory route")
	}
}

func (s *service) vault(w http.ResponseWriter, r *http.Request, vaultID, vaultPath string) (*memory.Vault, bool) {
	var (
		v   *memory.Vault
		err error
	)
	switch {
	case strings.TrimSpace(vaultPath) != "":
		v, err = s.manager.VaultForPath(vaultPath)
	case strings.TrimSpace(vaultID) != "":
		v, err = s.manager.VaultByID(vaultID)
	default:
		writeError(w, http.StatusBadRequest, "vault-required", "vault_id or vault_path is required")
		return nil, false
	}
	if err != nil {
		if errors.Is(err, memory.ErrVaultNotFound) {
			writeError(w, http.StatusNotFound, "vault-not-found", "vault is not registered")
			return nil, false
		}
		if errors.Is(err, memory.ErrVaultForbidden) {
			writeError(w, http.StatusForbidden, "vault-forbidden", "vault is outside the allowed roots")
			return nil, false
		}
		writeError(w, http.StatusInternalServerError, "vault-error", err.Error())
		return nil, false
	}
	return v, true
}

type rememberRequest struct {
	VaultID   string        `json:"vault_id"`
	VaultPath string        `json:"vault_path"`
	Record    memory.Record `json:"record"`
}

func (s *service) remember(w http.ResponseWriter, r *http.Request) {
	var body rememberRequest
	if !decode(w, r, &body) {
		return
	}
	v, ok := s.vault(w, r, body.VaultID, body.VaultPath)
	if !ok {
		return
	}
	record, created, err := v.Remember(body.Record)
	if err != nil {
		writeError(w, http.StatusBadRequest, "record-invalid", err.Error())
		return
	}
	if s.semantic != nil && record.Type != memory.ShortTerm {
		if err := s.semantic.Upsert(r.Context(), &record); err != nil {
			s.warn("semantic upsert skipped", "memory_id", record.ID, "error", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "vault_id": v.ID, "created": created, "record": record})
}

type queryRequest struct {
	VaultID     string        `json:"vault_id"`
	VaultPath   string        `json:"vault_path"`
	Query       string        `json:"query"`
	MemoryTypes []memory.Type `json:"memory_types"`
	Scope       memory.Scope  `json:"scope"`
	TopK        int           `json:"top_k"`
}

func (s *service) query(w http.ResponseWriter, r *http.Request, markUse bool) {
	var body queryRequest
	if !decode(w, r, &body) {
		return
	}
	v, ok := s.vault(w, r, body.VaultID, body.VaultPath)
	if !ok {
		return
	}
	q := memory.Query{Text: body.Query, Types: body.MemoryTypes, Scope: body.Scope, TopK: body.TopK}
	var (
		hits []memory.Hit
		err  error
	)
	if markUse {
		hits, err = v.Query(q)
	} else {
		hits, err = v.Inspect(q)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, "query-invalid", err.Error())
		return
	}
	if s.semantic != nil && strings.TrimSpace(body.Query) != "" {
		semanticHits, semanticErr := s.semantic.Search(r.Context(), v.ID, body.Query, body.MemoryTypes, body.Scope, body.TopK)
		if semanticErr != nil {
			s.warn("semantic search skipped", "vault_id", v.ID, "error", semanticErr)
		} else {
			filtered := semanticHits[:0]
			for _, hit := range semanticHits {
				record, ok := v.Get(hit.ID)
				if !ok || !memory.ScopeMatches(record.Scope, body.Scope) {
					continue
				}
				if len(body.MemoryTypes) > 0 {
					matched := false
					for _, t := range body.MemoryTypes {
						if record.Type == t {
							matched = true
							break
						}
					}
					if !matched {
						continue
					}
				}
				filtered = append(filtered, hit)
			}
			hits = mergeSemantic(v, hits, filtered, body.TopK, markUse)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "vault_id": v.ID, "hits": hits, "count": len(hits)})
}

type forgetRequest struct {
	VaultID   string   `json:"vault_id"`
	VaultPath string   `json:"vault_path"`
	IDs       []string `json:"ids"`
	Mode      string   `json:"mode"`
	Reason    string   `json:"reason"`
}

func (s *service) forget(w http.ResponseWriter, r *http.Request) {
	var body forgetRequest
	if !decode(w, r, &body) {
		return
	}
	v, ok := s.vault(w, r, body.VaultID, body.VaultPath)
	if !ok {
		return
	}
	changed, err := v.Forget(body.IDs, body.Mode, body.Reason)
	if err != nil {
		writeError(w, http.StatusBadRequest, "forget-invalid", err.Error())
		return
	}
	if s.semantic != nil && changed > 0 {
		if err := s.semantic.Delete(r.Context(), body.IDs); err != nil {
			s.warn("semantic delete skipped", "vault_id", v.ID, "error", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "vault_id": v.ID, "changed": changed})
}

type indexRequest struct {
	VaultID   string `json:"vault_id"`
	VaultPath string `json:"vault_path"`
	UserID    string `json:"user_id"`
	ProjectID string `json:"project_id"`
	MaxBytes  int64  `json:"max_bytes"`
}

func (s *service) indexVault(w http.ResponseWriter, r *http.Request) {
	var body indexRequest
	if !decode(w, r, &body) {
		return
	}
	v, ok := s.vault(w, r, body.VaultID, body.VaultPath)
	if !ok {
		return
	}
	documents, err := memory.ScanVault(v.Path, body.MaxBytes)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "index-failed", err.Error())
		return
	}
	bundle := memory.Bundle{Schema: "sparknet-memory-bundle/1", VaultID: v.ID}
	for _, document := range documents {
		bundle.Records = append(bundle.Records, memory.Record{
			Type:       memory.Semantic,
			Subtype:    "vault_page",
			SourceKind: memory.SourceVault,
			SourceRef:  document.SourceRef,
			Text:       document.Text,
			Summary:    document.Summary,
			Importance: 0.5,
			Scope:      memory.Scope{UserID: body.UserID, ProjectID: body.ProjectID},
			Consent:    memory.Consent{Capture: true},
			Provenance: memory.Provenance{Agent: "memory-index"},
		})
	}
	result, err := v.Import(bundle, false)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "index-failed", err.Error())
		return
	}
	if s.semantic != nil {
		for _, record := range v.List(5000) {
			if record.Type != memory.Semantic || record.Subtype != "vault_page" {
				continue
			}
			if err := s.semantic.Upsert(r.Context(), &record); err != nil {
				s.warn("semantic vault index skipped", "memory_id", record.ID, "error", err)
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"vault_id":  v.ID,
		"documents": len(documents),
		"result":    result,
	})
}

type vaultRequest struct {
	VaultID   string `json:"vault_id"`
	VaultPath string `json:"vault_path"`
}

func (s *service) vaultFromBody(w http.ResponseWriter, r *http.Request) (*memory.Vault, bool) {
	var body vaultRequest
	if !decode(w, r, &body) {
		return nil, false
	}
	return s.vault(w, r, body.VaultID, body.VaultPath)
}

func (s *service) prune(w http.ResponseWriter, r *http.Request) {
	v, ok := s.vaultFromBody(w, r)
	if !ok {
		return
	}
	changed, err := v.Prune(time.Now().UTC())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "prune-failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "vault_id": v.ID, "suppressed": changed})
}

type policyRequest struct {
	VaultID   string         `json:"vault_id"`
	VaultPath string         `json:"vault_path"`
	Policy    map[string]any `json:"policy"`
}

func (s *service) getPolicy(w http.ResponseWriter, r *http.Request) {
	v, ok := s.vaultFromQuery(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "vault_id": v.ID, "policy": v.Policy()})
}

func (s *service) setPolicy(w http.ResponseWriter, r *http.Request) {
	var body policyRequest
	if !decode(w, r, &body) {
		return
	}
	v, ok := s.vault(w, r, body.VaultID, body.VaultPath)
	if !ok {
		return
	}
	if err := v.SetPolicy(body.Policy); err != nil {
		writeError(w, http.StatusInternalServerError, "policy-failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "vault_id": v.ID, "policy": v.Policy()})
}

func (s *service) status(w http.ResponseWriter, r *http.Request) {
	v, ok := s.vaultFromQuery(w, r)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, v.Status())
}

func (s *service) list(w http.ResponseWriter, r *http.Request) {
	v, ok := s.vaultFromQuery(w, r)
	if !ok {
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "vault_id": v.ID, "records": v.List(limit)})
}

func (s *service) export(w http.ResponseWriter, r *http.Request) {
	v, ok := s.vaultFromBody(w, r)
	if !ok {
		return
	}
	bundle, err := v.Export()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "export-failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, bundle)
}

type importRequest struct {
	VaultID   string        `json:"vault_id"`
	VaultPath string        `json:"vault_path"`
	Bundle    memory.Bundle `json:"bundle"`
	Replace   bool          `json:"replace"`
}

func (s *service) importBundle(w http.ResponseWriter, r *http.Request) {
	var body importRequest
	if !decode(w, r, &body) {
		return
	}
	v, ok := s.vault(w, r, body.VaultID, body.VaultPath)
	if !ok {
		return
	}
	result, err := v.Import(body.Bundle, body.Replace)
	if err != nil {
		writeError(w, http.StatusBadRequest, "import-failed", err.Error())
		return
	}
	if s.semantic != nil {
		for _, record := range v.List(5000) {
			if record.Type == memory.ShortTerm || record.RetentionState != memory.Active {
				continue
			}
			if err := s.semantic.Upsert(r.Context(), &record); err != nil {
				s.warn("semantic import index skipped", "memory_id", record.ID, "error", err)
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "vault_id": v.ID, "result": result})
}

type planRequest struct {
	VaultID   string         `json:"vault_id"`
	VaultPath string         `json:"vault_path"`
	Plan      delegates.Plan `json:"plan"`
}

func (s *service) startPlan(w http.ResponseWriter, r *http.Request) {
	var body planRequest
	if !decode(w, r, &body) {
		return
	}
	v, ok := s.vault(w, r, body.VaultID, body.VaultPath)
	if !ok {
		return
	}
	entry, err := s.brokerFor(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "broker-failed", err.Error())
		return
	}
	runs, err := entry.broker.Start(r.Context(), body.Plan)
	if err != nil {
		writeError(w, http.StatusBadRequest, "plan-invalid", err.Error())
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "vault_id": v.ID, "plan_id": body.Plan.PlanID, "runs": runs})
}

func (s *service) listRuns(w http.ResponseWriter, r *http.Request) {
	v, ok := s.vaultFromQuery(w, r)
	if !ok {
		return
	}
	entry, err := s.brokerFor(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "broker-failed", err.Error())
		return
	}
	limit := 100
	if raw := r.URL.Query().Get("limit"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = parsed
		}
	}
	runs, err := entry.store.List(limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "store-failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "vault_id": v.ID, "runs": runs, "count": len(runs)})
}

func (s *service) runRoute(w http.ResponseWriter, r *http.Request, tail string) {
	parts := strings.Split(strings.Trim(tail, "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeError(w, http.StatusBadRequest, "run-required", "run id is required")
		return
	}
	v, ok := s.vaultFromQuery(w, r)
	if !ok {
		return
	}
	entry, err := s.brokerFor(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "broker-failed", err.Error())
		return
	}
	run, err := entry.store.Get(parts[0])
	if err != nil {
		writeError(w, http.StatusNotFound, "run-not-found", err.Error())
		return
	}
	if len(parts) == 1 {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "vault_id": v.ID, "run": run})
		return
	}
	if len(parts) == 2 && parts[1] == "events" {
		if r.URL.Query().Get("stream") == "1" {
			s.streamRun(w, r, entry, run.RunID)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "vault_id": v.ID, "run_id": run.RunID, "status": run.Status, "events": run.Events})
		return
	}
	writeError(w, http.StatusNotFound, "not-found", "unknown run route")
}

func (s *service) streamRun(w http.ResponseWriter, r *http.Request, entry *brokerEntry, runID string) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "stream-unsupported", "the server cannot flush")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	last := 0
	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		run, err := entry.store.Get(runID)
		if err != nil {
			fmt.Fprintf(w, "event: error\ndata: %q\n\n", err.Error())
			flusher.Flush()
			return
		}
		for _, event := range run.Events {
			if event.Sequence <= last {
				continue
			}
			payload, _ := json.Marshal(event)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, payload)
			last = event.Sequence
		}
		flusher.Flush()
		if run.Status.Terminal() {
			fmt.Fprintf(w, "event: end\ndata: {\"status\":%q}\n\n", run.Status)
			flusher.Flush()
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *service) cancelPlan(w http.ResponseWriter, r *http.Request, planID string) {
	if strings.TrimSpace(planID) == "" {
		writeError(w, http.StatusBadRequest, "plan-required", "plan id is required")
		return
	}
	v, ok := s.vaultFromQuery(w, r)
	if !ok {
		return
	}
	entry, err := s.brokerFor(v)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "broker-failed", err.Error())
		return
	}
	if !entry.broker.Cancel(planID) {
		writeError(w, http.StatusNotFound, "plan-not-found", "plan is not in flight")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "plan_id": planID})
}

func (s *service) warn(message string, args ...any) {
	if s.log != nil {
		s.log.Warn(message, args...)
	}
}

func mergeSemantic(v *memory.Vault, lexical []memory.Hit, semantic []memory.SemanticHit, topK int, markUse bool) []memory.Hit {
	if topK <= 0 {
		topK = 8
	}
	if topK > 50 {
		topK = 50
	}
	byID := make(map[string]*memory.Hit, len(lexical)+len(semantic))
	for _, hit := range lexical {
		copy := hit
		byID[hit.Record.ID] = &copy
	}
	var semanticOnly []string
	for _, hit := range semantic {
		if existing := byID[hit.ID]; existing != nil {
			existing.Score = math.Round((0.6*existing.Score+0.4*hit.Score)*10000) / 10000
			if existing.Reasons == nil {
				existing.Reasons = map[string]float64{}
			}
			existing.Reasons["semantic"] = math.Round(hit.Score*10000) / 10000
			continue
		}
		record, ok := v.Get(hit.ID)
		if !ok || record.RetentionState != memory.Active || record.Type == memory.ShortTerm {
			continue
		}
		importance := record.Importance
		if importance == 0 {
			importance = 0.5
		}
		byID[record.ID] = &memory.Hit{
			Record: record,
			Score:  math.Round((0.7*hit.Score+0.3*importance)*10000) / 10000,
			Reasons: map[string]float64{
				"semantic":   math.Round(hit.Score*10000) / 10000,
				"importance": math.Round(importance*10000) / 10000,
			},
		}
		semanticOnly = append(semanticOnly, record.ID)
	}
	hits := make([]memory.Hit, 0, len(byID))
	for _, hit := range byID {
		hits = append(hits, *hit)
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].Score > hits[j].Score })
	if len(hits) > topK {
		hits = hits[:topK]
	}
	if markUse && len(semanticOnly) > 0 {
		v.MarkUsed(semanticOnly)
	}
	return hits
}

func (s *service) brokerFor(v *memory.Vault) (*brokerEntry, error) {
	s.brokerMu.Lock()
	defer s.brokerMu.Unlock()
	if entry := s.brokers[v.ID]; entry != nil {
		return entry, nil
	}
	store, err := delegates.NewStore(filepath.Join(s.manager.Root(), "vaults", v.ID, "runs"))
	if err != nil {
		return nil, err
	}
	executors := []delegates.Executor{
		&delegates.CommandExecutor{HarnessName: delegates.HarnessCommand, Command: s.cfg.OpenCodeCommand},
	}
	if len(s.cfg.OpenCodeCommand) > 0 {
		executors = append(executors, &delegates.CommandExecutor{HarnessName: delegates.HarnessOpenCode, Command: s.cfg.OpenCodeCommand})
	}
	entry := &brokerEntry{store: store, broker: delegates.NewBroker(store, executors)}
	s.brokers[v.ID] = entry
	return entry, nil
}

func (s *service) vaultFromQuery(w http.ResponseWriter, r *http.Request) (*memory.Vault, bool) {
	return s.vault(w, r, r.URL.Query().Get("vault_id"), r.URL.Query().Get("vault_path"))
}

func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "malformed", err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"ok": false, "code": code, "error": message})
}

func stateDirFromEnv() string {
	if value := os.Getenv("SPARKNET_STATE_DIR"); value != "" {
		return filepath.Join(value, "memory")
	}
	return ""
}

// Close stops every delegate broker owned by this service.
func (s *service) Close() {
	for _, entry := range s.brokers {
		entry.broker.Close()
	}
}
