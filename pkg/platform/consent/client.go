// Package consent is the HTTP client other services use to talk to
// the consent service. Verification calls Client.VerifyGrant before
// every NIRA lookup; future services (consumer dashboards, citizen
// portals) will use the same client to render grant lists.
//
// Lives in pkg/platform so the consent service itself can stay
// independent of any caller — the wire shape this client implements
// is the contract both ends commit to.
package consent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// VerifyOutcome is the status field returned by /grants/{id}/verify.
type VerifyOutcome string

const (
	OutcomeGranted             VerifyOutcome = "granted"
	OutcomeExpired             VerifyOutcome = "expired"
	OutcomeRevoked             VerifyOutcome = "revoked"
	OutcomeConsumerMismatch    VerifyOutcome = "consumer_mismatch"
	OutcomeAttributeNotAllowed VerifyOutcome = "attribute_not_allowed"
	OutcomeNotFound            VerifyOutcome = "not_found"
)

// VerifyResult is the parsed response. Grant is only populated when
// Outcome is OutcomeGranted (the contract: don't leak the rest if the
// check failed). AssertionJWT is the signed assertion the caller can
// include in downstream audit chain entries for external regulator
// verification of the grant.
type VerifyResult struct {
	Outcome      VerifyOutcome
	GrantID      string
	ConsumerID   string
	ExpiresAt    time.Time
	AssertionJWT string
}

// Client is the consent-service HTTP client.
type Client struct {
	baseURL string // e.g. http://localhost:7076
	http    *http.Client
	tokenFn func() (string, error) // returns a bearer token suitable for the consent service
}

// Config carries the inputs to NewClient.
type Config struct {
	BaseURL string
	Timeout time.Duration
	// TokenFn supplies the bearer token. Allows the caller to use
	// whatever auth flow makes sense (client-credentials for a
	// service-to-service call, a real user's token for self-service).
	// Required.
	TokenFn func() (string, error)
}

func NewClient(cfg Config) *Client {
	t := cfg.Timeout
	if t <= 0 {
		t = 5 * time.Second
	}
	return &Client{
		baseURL: strings.TrimRight(cfg.BaseURL, "/"),
		http:    &http.Client{Timeout: t},
		tokenFn: cfg.TokenFn,
	}
}

// VerifyGrant calls /grants/{id}/verify on the consent service. On
// successful HTTP exchange, returns the parsed result regardless of
// outcome. Network / 5xx errors return an error.
func (c *Client) VerifyGrant(ctx context.Context, grantID, consumerID string, attributes []string) (VerifyResult, error) {
	if grantID == "" {
		return VerifyResult{}, errors.New("grant id is required")
	}
	if consumerID == "" {
		return VerifyResult{}, errors.New("consumer id is required")
	}

	q := url.Values{}
	q.Set("consumer_id", consumerID)
	if len(attributes) > 0 {
		q.Set("attributes", strings.Join(attributes, ","))
	}
	u := fmt.Sprintf("%s/grants/%s/verify?%s", c.baseURL, url.PathEscape(grantID), q.Encode())

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if c.tokenFn != nil {
		tok, err := c.tokenFn()
		if err != nil {
			return VerifyResult{}, fmt.Errorf("fetch token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return VerifyResult{}, fmt.Errorf("consent verify call: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return VerifyResult{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode >= 500 {
		return VerifyResult{}, fmt.Errorf("consent service HTTP %d: %s", resp.StatusCode, string(body))
	}

	var raw struct {
		Status string `json:"status"`
		Grant  *struct {
			ID         string    `json:"id"`
			ConsumerID string    `json:"consumer_id"`
			ExpiresAt  time.Time `json:"expires_at"`
		} `json:"grant,omitempty"`
		AssertionJWT string `json:"assertion_jwt,omitempty"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return VerifyResult{}, fmt.Errorf("decode response: %w", err)
	}
	out := VerifyResult{
		Outcome:      VerifyOutcome(raw.Status),
		AssertionJWT: raw.AssertionJWT,
	}
	if raw.Grant != nil {
		out.GrantID = raw.Grant.ID
		out.ConsumerID = raw.Grant.ConsumerID
		out.ExpiresAt = raw.Grant.ExpiresAt
	}
	return out, nil
}
