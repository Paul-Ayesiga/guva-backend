// Package problem emits RFC 7807 Problem Details JSON responses.
//
// The full type URI catalogue lives under https://api.guva.go.ug/errors/
// and is documented in the developer portal (per §9.7 of the API
// documentation chapter). Service handlers should use this package for
// every error response so consumers get a consistent shape.
package problem

import (
	"encoding/json"
	"net/http"
)

const baseURI = "https://api.guva.go.ug/errors/"

// Details is the RFC 7807 payload shape with the platform's extensions.
type Details struct {
	Type          string `json:"type"`
	Title         string `json:"title"`
	Status        int    `json:"status"`
	Detail        string `json:"detail,omitempty"`
	Instance      string `json:"instance,omitempty"`
	CorrelationID string `json:"correlation_id,omitempty"`
}

// Write emits a Problem Details response with the given status, type
// suffix (e.g. "invalid_token"), and detail string. The full type URI is
// derived from the suffix; consumers can dereference it for the canonical
// human-readable explanation.
func Write(w http.ResponseWriter, status int, typeSuffix, detail string) {
	WriteContext(w, status, typeSuffix, detail, "", "")
}

// WriteContext is like Write but lets the caller supply the request
// instance path and a correlation ID. Pass empty strings to omit either.
func WriteContext(w http.ResponseWriter, status int, typeSuffix, detail, instance, correlationID string) {
	d := Details{
		Type:          baseURI + typeSuffix,
		Title:         typeSuffix,
		Status:        status,
		Detail:        detail,
		Instance:      instance,
		CorrelationID: correlationID,
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(d)
}
