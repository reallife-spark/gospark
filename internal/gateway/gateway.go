// Package gateway is the lightweight Go API gateway in front of the compute
// plane (a local vLLM/serving engine). It proxies inference requests, streams
// Server-Sent Events without buffering, and exposes the router's escalation
// decision so a client can decide whether to escalate.
//
// It holds no model state: the heavy runtime is the Python/CUDA plane, which is
// the whole point of the split.
package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/router"
)

// Gateway proxies to Upstream.
type Gateway struct {
	Upstream  string
	Router    *router.Router
	Client    *http.Client
	LocalCaps []string
}

// New builds a gateway. An empty upstream disables proxying but keeps the
// health and route endpoints live.
func New(upstream string, r *router.Router) *Gateway {
	return &Gateway{
		Upstream: strings.TrimRight(upstream, "/"),
		Router:   r,
		Client: &http.Client{
			Timeout: 0, // inference streams may be long-lived
		},
	}
}

// Handler returns the gateway's routes.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", g.handleHealth)
	mux.HandleFunc("/v1/route", g.handleRoute)
	mux.HandleFunc("/v1/chat/completions", g.handleProxy)
	mux.HandleFunc("/", g.handleProxy)
	return mux
}

func (g *Gateway) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"upstream": g.Upstream,
	})
}

// handleRoute returns the router's decision for a prompt without running it.
func (g *Gateway) handleRoute(w http.ResponseWriter, r *http.Request) {
	prompt := r.URL.Query().Get("prompt")
	if prompt == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prompt query parameter required"})
		return
	}
	dec := g.Router.Decide(prompt, g.LocalCaps)
	writeJSON(w, http.StatusOK, dec)
}

func (g *Gateway) handleProxy(w http.ResponseWriter, r *http.Request) {
	if g.Upstream == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no upstream configured"})
		return
	}
	target, err := url.Parse(g.Upstream + r.URL.Path)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if r.URL.RawQuery != "" {
		target.RawQuery = r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(), r.Body)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	copyHeaders(req.Header, r.Header)
	req.Header.Set("X-Forwarded-For", r.RemoteAddr)

	resp, err := g.Client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	defer resp.Body.Close()
	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	// Stream with an explicit flush per write so SSE tokens leave immediately
	// instead of sitting in the response buffer until the handler returns.
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err == io.EOF {
			return
		}
		if err != nil {
			return
		}
	}
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// DecisionLine formats a decision as a compact log line.
func DecisionLine(d apiv1.EscalationDecision) string {
	return fmt.Sprintf("route=%s tokens=%d async=%t reason=%q", d.Target, d.EstimatedPromptTokens, d.AsyncRequired, d.Reason)
}
