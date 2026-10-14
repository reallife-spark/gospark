//go:build !cgo || !linux

// Package dcgm is the DCGM health backend for gputel. This build has cgo
// disabled (or is not Linux), so the DCGM binding is not compiled in and the
// backend registers nothing: gputel.Default falls back to the Null collector.
// See dcgm_cgo.go for the real backend.
package dcgm
