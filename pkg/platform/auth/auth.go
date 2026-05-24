// Package auth provides JWT claim extraction and scope-based authorization
// middleware shared by every GUVA backend service.
//
// The gateway (APISIX with openid-connect, see deploy/compose/apisix/) has
// already verified the token's signature, issuer, audience, and expiry by
// the time a request reaches the service. This package therefore parses
// the JWT WITHOUT re-verifying the signature — services trust the gateway
// as part of the zero-trust boundary at the perimeter, not as an excuse
// to do work twice.
//
// Direct calls to a service port (bypassing the gateway) are intended for
// local development only. In production, network policies enforce that
// services are reachable only through the gateway.
//
// Usage:
//
//	mux.Handle("/widgets",
//	    auth.RequireScope("verify:widget", widgetHandler))
//
//	// inside the handler:
//	claims, _ := auth.FromContext(r.Context())
//	logger.InfoContext(r.Context(), "request", "sub", claims.Subject)
package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/guva-ug/guva-backend/pkg/platform/problem"
)

type ctxKey int

const claimsKey ctxKey = 1

// Claims holds the subset of JWT claims platform services rely on. JSON
// tags match the names emitted by Keycloak; add custom claims to your
// service's own type if you need them.
type Claims struct {
	Subject   string `json:"sub"`
	Scope     string `json:"scope"`
	Issuer    string `json:"iss"`
	ClientID  string `json:"azp"`
	Audience  string `json:"aud"`
	ExpiresAt int64  `json:"exp"`
	JTI       string `json:"jti"`
}

// Scopes returns the space-separated scope claim split into a slice.
func (c Claims) Scopes() []string {
	if c.Scope == "" {
		return nil
	}
	return strings.Fields(c.Scope)
}

// HasScope reports whether the given scope is present in the token.
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
	return RequireAnyScope([]string{scope}, next)
}

// RequireAnyScope is RequireScope with OR semantics: the request passes
// when the token carries ANY of the listed scopes. Used to wire
// endpoints that are reachable by either a consumer (e.g. holding
// `audit:read` for their own data) or a platform admin (holding the
// equivalent `admin:audit`). Reports the full required set in the
// 403 body so the caller knows what would have worked.
func RequireAnyScope(scopes []string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		claims, err := FromAuthorization(r.Header.Get("Authorization"))
		if err != nil {
			problem.Write(w, http.StatusUnauthorized, "invalid_token", err.Error())
			return
		}
		for _, s := range scopes {
			if claims.HasScope(s) {
				ctx := context.WithValue(r.Context(), claimsKey, claims)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}
		problem.Write(w, http.StatusForbidden, "insufficient_scope",
			"required any of: "+strings.Join(scopes, ", "))
	})
}
