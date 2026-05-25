package server

import "github.com/google/uuid"

// newUUID is split out so the rest of server.go doesn't need the
// google/uuid import directly — and so tests can override if they
// ever need deterministic ids.
func newUUID() string {
	return uuid.NewString()
}
