package server

import (
	"net/http"
	"time"
)

// drainShutdownTimeout bounds how long a draining process waits for in-flight
// gateway requests to finish before exiting. A systemd unit's TimeoutStopSec
// must be at least this, plus a margin for the socket to close.
const drainShutdownTimeout = 5 * time.Second

// BeginDrain marks the process as draining: it stops accepting new work — new
// task dispatches are refused and the gateway reports not-ready and rejects new
// inference requests — while in-flight work is allowed to finish. It runs on
// ctx cancellation (graceful shutdown) and can be triggered early by an
// operator via POST /admin/drain. It is idempotent.
func (a *App) BeginDrain() {
	if a.draining.CompareAndSwap(false, true) {
		a.log.Info("draining: no new work will be accepted")
	}
}

// Draining reports whether the process is draining.
func (a *App) Draining() bool { return a.draining.Load() }

// handleAdminDrain serves POST /admin/drain: it flips the process into drain so
// a load balancer or systemd stop can quiesce it before the process exits.
func (a *App) handleAdminDrain(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	a.BeginDrain()
	writeAdminJSON(w, http.StatusOK, map[string]any{"draining": true})
}
