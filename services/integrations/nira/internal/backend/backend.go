// Package backend defines the integration-service's pluggable
// "where does the data come from" interface. Two implementations
// ship today:
//
//   - simulator: in-memory canned records, dev-only.
//   - upstream:  production HTTP client against the real NIRA API,
//     with mTLS, exponential-backoff retries, and a circuit breaker.
//
// The selection is at startup via NIRA_BACKEND; verification (and
// any other caller) never knows which is active.
package backend

import (
	"context"
	"errors"

	"github.com/guva-ug/guva-backend/services/integrations/nira/internal/canonical"
)

// Backend is what the HTTP layer depends on. Errors are returned
// only on genuine transport/upstream failure; "no such NIN" is not
// an error — Lookup returns (Record{}, false, nil).
type Backend interface {
	Name() string
	Lookup(ctx context.Context, nin string) (rec canonical.Record, found bool, err error)
	Health(ctx context.Context) error
}

// ErrUpstreamUnavailable signals an upstream-side failure that's
// already exhausted retries / been short-circuited by the breaker.
// HTTP layer maps this to a 503 with a structured error body.
var ErrUpstreamUnavailable = errors.New("NIRA upstream unavailable")

// ErrCircuitOpen signals the upstream breaker is open and the
// request was rejected without an attempt. Same handling as
// ErrUpstreamUnavailable from the HTTP layer's POV.
var ErrCircuitOpen = errors.New("NIRA upstream circuit breaker is open")
