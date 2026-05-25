package server

import (
	"context"
	"time"
)

// cContext + cancelFn are small adapter types so server.go's
// interface{ Done()... } parameter pattern can still hand off to
// context.WithTimeout cleanly. context.Context already satisfies the
// interface{ Done() <-chan struct{} } shape — this file just makes
// that explicit so the production builders never need to type-assert.
type cContext = context.Context
type cancelFn = context.CancelFunc

func contextWithTimeout(ctx interface{ Done() <-chan struct{} }, d time.Duration) (cContext, cancelFn) {
	c, _ := ctx.(context.Context)
	if c == nil {
		c = context.Background()
	}
	return context.WithTimeout(c, d)
}
