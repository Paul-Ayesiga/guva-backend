// Package health provides liveness and readiness state, suitable for
// Kubernetes probes per §6.6 of the non-functional requirements.
package health

import "sync/atomic"

type Probes struct {
	ready atomic.Bool
}

func New() *Probes { return &Probes{} }

func (p *Probes) MarkReady()   { p.ready.Store(true) }
func (p *Probes) MarkNotReady() { p.ready.Store(false) }

func (p *Probes) IsReady() bool { return p.ready.Load() }
