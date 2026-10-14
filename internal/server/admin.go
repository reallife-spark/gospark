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

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/authz"
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
	return mux
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
