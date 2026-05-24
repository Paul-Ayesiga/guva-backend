// Package auth handles JWT claim extraction and scope enforcement.
//
// The gateway (Kong, configured by deploy/compose/kong/kong.yml) has
// already verified the token's signature and expiry by the time a request
// reaches this service.  This package therefore parses the JWT WITHOUT
// re-verifying the signature — services trust the gateway as part of the
// zero-trust boundary at the perimeter, not as an excuse to do work twice.
//
// Direct calls to the service port (bypassing the gateway) are intended
// for local development only.  In production the service is only
// reachable through the gateway; ambient network controls enforce that.
package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

type ctxKey int

const claimsKey ctxKey = 1

// Claims holds the subset of JWT claims the service uses. JSON tags match
// the names emitted by Keycloak.
type Claims struct {
	Subject   string `json:"sub"`
	Scope     string `json:"scope"`
	Issuer    string `json:"iss"`
	ClientID  string `json:"azp"`
	ExpiresAt int64  `json:"exp"`
}

// Scopes returns the space-separated scope claim split into a slice.
func (c Claims) Scopes() []string {
	if c.Scope == "" {
		return nil
	}
	return strings.Fields(c.Scope)
}

// HasScope reports whether the given scope is present.
func (c Claims) HasScope(s string) bool {
	for _, x := range c.Scopes() {
		if x == s {
			return true
		}
	}
	return false
}

// FromAuthorization parses the Authorization header value into Claims.
// It does NOT verify the signature.
func FromAuthorization(header string) (Claims, error) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return Claims{}, errors.New("missing or non-bearer Authorization header")
	}
	parts := strings.Split(strings.TrimPrefix(header, prefix), ".")
	if len(parts) != 3 {
		return Claims{}, errors.New("malformed JWT: expected three segments")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Claims{}, err
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return Claims{}, err
	}
	return c, nil
}

// FromContext returns the Claims attached to a request context by
// RequireScope, if any.
func FromContext(ctx context.Context) (Claims, bool) {
	c, ok := ctx.Value(claimsKey).(Claims)
	return c, ok
}

// RequireScope returns a middleware that rejects requests whose JWT does
// not carry the named scope. The parsed claims are stashed on the context
// for downstream handlers via FromContext.
func RequireScope(scope string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, err := FromAuthorization(r.Header.Get("Authorization"))
		if err != nil {
			writeProblem(w, http.StatusUnauthorized, "invalid_token", err.Error())
			return
		}
		if !claims.HasScope(scope) {
			writeProblem(w, http.StatusForbidden, "insufficient_scope",
				"required scope: "+scope)
			return
		}
		ctx := context.WithValue(r.Context(), claimsKey, claims)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func writeProblem(w http.ResponseWriter, status int, typ, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"type":   "https://api.guva.go.ug/errors/" + typ,
		"title":  typ,
		"detail": detail,
	})
}
