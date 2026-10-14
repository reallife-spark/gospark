//go:build !cgo || !linux

// Package nvml is the NVML GPU-metrics backend for gputel. This build has cgo
// disabled (or is not Linux), so the driver binding is not compiled in and the
// backend registers nothing: gputel.Default falls back to the Null collector.
// See nvml_cgo.go for the real backend.
package nvml
