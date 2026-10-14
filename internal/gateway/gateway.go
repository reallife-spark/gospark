// Package gateway is the lightweight Go API gateway in front of the compute
// plane (a local vLLM/serving engine). It proxies inference requests, streams
// Server-Sent Events without buffering, and exposes the router's escalation
// decision so a client can decide whether to escalate.
//
// It holds no model state: the heavy runtime is the Python/CUDA plane, which is
// the whole point of the split.
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/reallife-spark/goSpark/internal/apiv1"
	"github.com/reallife-spark/goSpark/internal/router"
)

// Gateway proxies to Upstream.
type Gateway struct {
	Upstream  string
	Router    *router.Router
	Client    *http.Client
	LocalCaps []string

	// Model is the serving model this gateway fronts. It is informational and
	// is the discovery fallback when no upstream is configured.
	Model string
	// ReadyPath is the upstream path probed by /readyz. Empty uses
	// DefaultReadyPath.
	ReadyPath string
	// ProbeTimeout bounds the /readyz upstream probe. Zero uses
	// DefaultProbeTimeout.
	ProbeTimeout time.Duration
}

const (
	// DefaultReadyPath is the readiness probe path (an OpenAI-compatible
	// models endpoint is the cheapest "is the engine serving?" check).
	DefaultReadyPath = "/v1/models"
	// DefaultProbeTimeout bounds the readiness probe so a hung upstream cannot
	// hang the probe.
	DefaultProbeTimeout = 2 * time.Second
)

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
	mux.HandleFunc("/readyz", g.handleReady)
	mux.HandleFunc("/v1/route", g.handleRoute)
	mux.HandleFunc("/v1/models", g.handleModels)
	mux.HandleFunc("/v1/chat/completions", g.handleProxy)
	mux.HandleFunc("/", g.handleProxy)
	return mux
}

func (g *Gateway) readyPath() string {
	if g.ReadyPath != "" {
		return g.ReadyPath
	}
	return DefaultReadyPath
}

func (g *Gateway) probeTimeout() time.Duration {
	if g.ProbeTimeout > 0 {
		return g.ProbeTimeout
	}
	return DefaultProbeTimeout
}

// handleHealth is liveness: the gateway process is up. It does not probe the
// upstream, so it stays fast and never blocks on a slow engine.
func (g *Gateway) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status":   "ok",
		"upstream": g.Upstream,
		"model":    g.Model,
	})
}

// handleReady is readiness: the gateway can serve, which for a proxying gateway
// means the compute-plane engine is reachable. It probes the upstream's models
// endpoint and reports 503 until the engine answers.
func (g *Gateway) handleReady(w http.ResponseWriter, r *http.Request) {
	if g.Upstream == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ready": false, "reason": "no upstream configured", "model": g.Model,
		})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), g.probeTimeout())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.Upstream+g.readyPath(), nil)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"ready": false, "reason": err.Error()})
		return
	}
	resp, err := g.Client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ready": false, "upstream": g.Upstream, "reason": err.Error(),
		})
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ready": false, "upstream": g.Upstream, "status": resp.StatusCode,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ready": true, "upstream": g.Upstream, "model": g.Model,
	})
}

// handleModels serves OpenAI-compatible model discovery. It proxies to a live
// engine so the answer is the truth; with no upstream it reports the declared
// model so a client can still discover what this node fronts.
func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	if g.Upstream != "" {
		g.proxy(w, r)
		return
	}
	if g.Model == "" {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no upstream configured"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data": []map[string]any{
			{"id": g.Model, "object": "model", "owned_by": "sparknet"},
		},
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
	g.proxy(w, r)
}

// proxy forwards the request to the upstream and streams the response back with
// an explicit flush per write, so SSE tokens leave immediately. It marks
// event-stream responses unbuffered so a reverse proxy in front (nginx and
// friends) does not hold the tokens back either.
func (g *Gateway) proxy(w http.ResponseWriter, r *http.Request) {
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
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Set("Cache-Control", "no-cache")
	}
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
