// Package health exposes liveness and readiness state suitable for
// Kubernetes-style probes (per §6.6 of the non-functional requirements).
//
// Services build their HTTP handlers using these probes to back the
// /healthz and /readyz endpoints. Liveness is implicit (the handler
// always returns 200 when invoked, signalling the process is up).
// Readiness is something the service explicitly toggles when it has
// finished its startup checks (DB migrated, config loaded, etc.).
package health

import "sync/atomic"

// Probes holds atomic readiness state. Construct with New and pass to
// httpserver.New (or wire your own handlers).
type Probes struct {
	ready atomic.Bool
}

// New returns a Probes in "not ready" state.
func New() *Probes { return &Probes{} }

// MarkReady transitions readiness to ready. Idempotent.
func (p *Probes) MarkReady() { p.ready.Store(true) }

// MarkNotReady transitions readiness to not ready (e.g. before
// graceful shutdown).
func (p *Probes) MarkNotReady() { p.ready.Store(false) }

// IsReady reports the current readiness state.
func (p *Probes) IsReady() bool { return p.ready.Load() }
