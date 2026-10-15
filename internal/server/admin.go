package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"time"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/authz"
	"github.com/reallife-spark/goSpark/internal/placement"
)

// requireLoopback rejects any admin listen address that is not on the loopback
// interface. The admin surface carries full authority over the signed registry
// (the process holds the Master key), so exposing it would let anyone who can
// reach the port revoke or distribute nodes without the key. Remote
// administration would need a signed-request path, which is out of scope.
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("admin: invalid listen address %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("admin: listen %q is not loopback; the admin surface grants full registry "+
			"control and must not be exposed", addr)
	}
	return nil
}

// adminHandler builds the loopback admin mux. It is the operator's live handle
// on the registry a running control plane is serving.
func (a *App) adminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/admin/registry", a.handleAdminRegistry)
	mux.HandleFunc("/admin/registry/revoke", a.handleAdminRevoke)
	mux.HandleFunc("/admin/registry/authorize", a.handleAdminAuthorize)
	mux.HandleFunc("/admin/gpu", a.handleAdminGPU)
	mux.HandleFunc("/admin/health", a.handleAdminHealth)
	mux.HandleFunc("/admin/metrics", a.handleAdminMetrics)
	mux.HandleFunc("/admin/placement", a.handleAdminPlacement)
	mux.HandleFunc("/admin/kvcache", a.handleAdminKVCache)
	return mux
}

// handleAdminPlacement serves GET /admin/placement: it ranks the nodes the
// control plane may dispatch to and reports the chosen one, without dispatching.
// It is read-only and makes the dispatch policy inspectable. Query parameters:
//
//	caps=cluster-serve       capability to require (default cluster-serve)
//	min_free_mem=<bytes>     require at least this much free accelerator memory
//	max_temp=<C>             require every reported GPU at or below this ceiling
//	require_healthy=1        require a Healthy verdict on every reported GPU
//	max_age=<Go duration>    treat telemetry older than this as stale
func (a *App) handleAdminPlacement(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()
	capability := authz.Capability(q.Get("caps"))
	if capability == "" {
		capability = authz.CapClusterServe
	}
	if !authz.IsKnown(capability) {
		writeAdminError(w, http.StatusBadRequest, fmt.Errorf("unknown capability %q", capability))
		return
	}
	req := placement.Request{
		Requirement: placement.Requirement{RequireHealthy: truthy(q.Get("require_healthy"))},
	}
	if v := q.Get("min_free_mem"); v != "" {
		n, err := strconv.ParseUint(v, 10, 64)
		if err != nil {
			writeAdminError(w, http.StatusBadRequest, fmt.Errorf("min_free_mem: %w", err))
			return
		}
		req.MinFreeMemBytes = n
	}
	if v := q.Get("max_temp"); v != "" {
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			writeAdminError(w, http.StatusBadRequest, fmt.Errorf("max_temp: %w", err))
			return
		}
		req.MaxTempC = uint32(n)
	}
	if v := q.Get("max_age"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			writeAdminError(w, http.StatusBadRequest, fmt.Errorf("max_age: %w", err))
			return
		}
		req.MaxTelemetryAge = d
	}
	writeAdminJSON(w, http.StatusOK, a.Place(req, capability))
}

// handleAdminKVCache serves GET /admin/kvcache: the status of the
// compute-plane RadixAttention seam, and — with ?prompt= — the node it reports
// as holding that prompt's cached prefix. It is read-only; the prefix tree
// itself stays in the compute plane.
func (a *App) handleAdminKVCache(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	type view struct {
		Configured bool   `json:"configured"`
		Endpoint   string `json:"endpoint,omitempty"`
		Healthy    bool   `json:"healthy"`
		Error      string `json:"error,omitempty"`
		Prompt     string `json:"prompt,omitempty"`
		Node       string `json:"node,omitempty"`
		Matched    int    `json:"matched_tokens,omitempty"`
		PrefixHash string `json:"prefix_hash,omitempty"`
	}
	var out view
	if a.kv == nil {
		writeAdminJSON(w, http.StatusOK, out)
		return
	}
	out.Configured = true
	out.Endpoint = a.kv.Name()
	if err := a.kv.Health(r.Context()); err != nil {
		out.Error = err.Error()
	} else {
		out.Healthy = true
	}
	if prompt := r.URL.Query().Get("prompt"); prompt != "" {
		out.Prompt = prompt
		res, ok, err := a.KVLookup(r.Context(), prompt)
		if err != nil {
			out.Error = err.Error()
		} else if ok {
			out.Node, out.Matched, out.PrefixHash = res.Node, res.MatchedTokens, res.PrefixHash
		}
	}
	writeAdminJSON(w, http.StatusOK, out)
}

// truthy accepts the usual on-flags for a query bool.
func truthy(v string) bool {
	switch v {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// handleAdminGPU serves GET /admin/gpu: the last GPU telemetry per node, as
// reported by the sparknet-gpu sidecar. It is read-only; telemetry is never
// authority, so there is no write counterpart.
func (a *App) handleAdminGPU(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeAdminJSON(w, http.StatusOK, a.GPUSnapshot())
}

// AdminAuthorize grants or updates one node's capabilities on the live
// registry, then re-signs and re-publishes — the sign-authorization right. It
// is the running-process counterpart of `sparknetctl registry sign`. The entry
// is stamped with a fresh issue time, so a grant also advances the rollback
// guard.
func (a *App) AdminAuthorize(ctx context.Context, proj apiv1.NodeIdentity, capabilities []string, ttl time.Duration) (string, error) {
	if a.role != RoleControl {
		return "", errors.New("server: admin surface is control-plane only")
	}
	if a.registryDoc == nil {
		return "", errors.New("server: no registry configured")
	}
	if proj.NodeID == "" {
		return "", errors.New("authz: node_id is required")
	}
	if len(capabilities) == 0 {
		return "", errors.New("authz: at least one capability is required")
	}
	if ttl <= 0 {
		ttl = authz.DefaultAuthorizationTTL
	}
	now := time.Now()
	entry, err := authz.NewAuthorization(a.id, proj, capabilities, now, now.Add(ttl))
	if err != nil {
		return "", err
	}
	digest, err := a.mutateRegistry(ctx, func(doc *apiv1.NodeRegistry) error {
		for i := range doc.Authorizations {
			if doc.Authorizations[i].Identity.NodeID == entry.Identity.NodeID {
				doc.Authorizations[i] = entry
				return nil
			}
		}
		doc.Authorizations = append(doc.Authorizations, entry)
		return nil
	})
	if err != nil {
		return "", err
	}
	a.log.Info("authorization granted", "node", proj.NodeID, "caps", capabilities, "digest", short(digest))
	return digest, nil
}

// AdminRevoke flips one node's revocation flag on the live registry, re-signs
// every entry with a fresh stamp, refreshes the executor keys and re-publishes.
// It is the running-process counterpart of `sparknetctl registry revoke`.
func (a *App) AdminRevoke(ctx context.Context, nodeID string, revoked bool) (string, error) {
	if a.role != RoleControl {
		return "", errors.New("server: admin surface is control-plane only")
	}
	if a.registryDoc == nil {
		return "", errors.New("server: no registry configured")
	}
	digest, err := a.mutateRegistry(ctx, func(doc *apiv1.NodeRegistry) error {
		return authz.SetRevocation(doc, nodeID, revoked)
	})
	if err != nil {
		return "", err
	}
	a.log.Info("registry revocation changed", "node", nodeID, "revoked", revoked, "digest", short(digest))
	return digest, nil
}

// adminNodeView is the per-node shape the admin surface reports.
type adminNodeView struct {
	NodeID       string   `json:"node_id"`
	Revoked      bool     `json:"revoked"`
	Capabilities []string `json:"capabilities,omitempty"`
	OverlayIPv4  string   `json:"overlay_ipv4,omitempty"`
	IssuedAt     string   `json:"issued_at,omitempty"`
	ExpiresAt    string   `json:"expires_at,omitempty"`
}

// adminRegistryView is the GET body: the digest a peer would report plus every
// authorization currently served.
type adminRegistryView struct {
	Digest             string          `json:"digest"`
	SchemaVersion      string          `json:"schema_version"`
	MasterPublicKeyB64 string          `json:"master_public_key_b64"`
	Nodes              []adminNodeView `json:"nodes"`
}

// handleAdminRegistry serves GET (snapshot) and PUT (replace authorizations).
func (a *App) handleAdminRegistry(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		a.adminShow(w)
	case http.MethodPut:
		a.adminReplace(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (a *App) adminShow(w http.ResponseWriter) {
	a.regMu.RLock()
	doc := a.registryDoc
	digest, _ := authz.RegistryDigest(doc)
	var view adminRegistryView
	if doc != nil {
		view = adminRegistryView{
			SchemaVersion:      doc.SchemaVersion,
			MasterPublicKeyB64: doc.MasterPublicKeyB64,
		}
		for i := range doc.Authorizations {
			au := doc.Authorizations[i]
			view.Nodes = append(view.Nodes, adminNodeView{
				NodeID:       au.Identity.NodeID,
				Revoked:      au.Revoked,
				Capabilities: au.Capabilities,
				OverlayIPv4:  au.OverlayIPv4,
				IssuedAt:     au.IssuedAt,
				ExpiresAt:    au.ExpiresAt,
			})
		}
	}
	a.regMu.RUnlock()
	view.Digest = digest
	sort.Slice(view.Nodes, func(i, j int) bool { return view.Nodes[i].NodeID < view.Nodes[j].NodeID })
	writeAdminJSON(w, http.StatusOK, view)
}

func (a *App) adminReplace(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Authorizations []apiv1.NodeAuthorization `json:"authorizations"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil {
		writeAdminError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if a.registryDoc == nil {
		writeAdminError(w, http.StatusConflict, errors.New("no registry configured"))
		return
	}
	digest, err := a.ReSignRegistry(r.Context(), req.Authorizations)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, err)
		return
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{"digest": digest, "nodes": len(req.Authorizations)})
}

// handleAdminAuthorize accepts {"identity": {...}, "capabilities": [...],
// "ttl_seconds": N}. It upserts one node's authorization, the sign-authorization
// right.
func (a *App) handleAdminAuthorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Identity     apiv1.NodeIdentity `json:"identity"`
		Capabilities []string           `json:"capabilities"`
		TTLSeconds   int64              `json:"ttl_seconds"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeAdminError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	ttl := authz.DefaultAuthorizationTTL
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}
	digest, err := a.AdminAuthorize(r.Context(), req.Identity, req.Capabilities, ttl)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, err)
		return
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{
		"digest":       digest,
		"node_id":      req.Identity.NodeID,
		"capabilities": req.Capabilities,
	})
}

// handleAdminRevoke accepts {"node_id": "...", "revoked": bool}. revoked
// defaults to true, so {"node_id":"x"} revokes and {"node_id":"x","revoked":
// false} reinstates.
func (a *App) handleAdminRevoke(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		NodeID  string `json:"node_id"`
		Revoked *bool  `json:"revoked"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeAdminError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if req.NodeID == "" {
		writeAdminError(w, http.StatusBadRequest, errors.New("node_id is required"))
		return
	}
	revoked := true
	if req.Revoked != nil {
		revoked = *req.Revoked
	}
	digest, err := a.AdminRevoke(r.Context(), req.NodeID, revoked)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, authz.ErrRegistryUnknownNode) {
			status = http.StatusNotFound
		}
		writeAdminError(w, status, err)
		return
	}
	writeAdminJSON(w, http.StatusOK, map[string]any{
		"digest":  digest,
		"node_id": req.NodeID,
		"revoked": revoked,
	})
}

func writeAdminJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeAdminError(w http.ResponseWriter, status int, err error) {
	writeAdminJSON(w, status, map[string]string{"error": err.Error()})
}
