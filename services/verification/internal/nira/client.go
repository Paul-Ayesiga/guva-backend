// HTTP-client implementation of nira.Adapter. Calls the integration
// service (services/integrations/nira) over the local network and
// translates the integration's NIRA-canonical record into the
// verification-side nira.Record shape.
//
// This is the "production-shaped" path verification uses when
// NIRA_MODE=integration. The in-process mock at mock.go stays
// available for unit tests + isolated dev where standing up the
// integration service is unnecessary friction.

package nira

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// NewHTTPClient returns an Adapter that POSTs lookups to the
// integration service at baseURL. tokenFn supplies the bearer (the
// verification service uses its own client-credentials flow against
// Keycloak — exact same pattern as the consent client).
func NewHTTPClient(baseURL string, tokenFn func() (string, error)) Adapter {
	return &httpAdapter{
		baseURL: strings.TrimRight(baseURL, "/"),
		http:    &http.Client{Timeout: 8 * time.Second},
		tokenFn: tokenFn,
	}
}

type httpAdapter struct {
	baseURL string
	http    *http.Client
	tokenFn func() (string, error)
}

// integrationResponse mirrors services/integrations/nira/internal/canonical.LookupResponse.
// We keep this struct local to verification so a deploy-skew between
// the two services (different field additions) doesn't crash the
// caller — unknown fields decode-skip; required fields stay stable.
type integrationResponse struct {
	LookupID string `json:"lookup_id"`
	Found    bool   `json:"found"`
	Record   struct {
		NIN              string    `json:"nin"`
		GivenName        string    `json:"given_name"`
		MiddleName       string    `json:"middle_name"`
		Surname          string    `json:"surname"`
		DateOfBirth      string    `json:"date_of_birth"`
		Sex              string    `json:"sex"`
		Nationality      string    `json:"nationality"`
		MotherMaidenName string    `json:"mother_maiden_name"`
		Status           string    `json:"status"`
		LastUpdatedAt    time.Time `json:"last_updated_at"`
	} `json:"record"`
}

func (h *httpAdapter) Lookup(ctx context.Context, nin string) (Record, bool, error) {
	body, _ := json.Marshal(map[string]string{"nin": nin})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.baseURL+"/lookup", bytes.NewReader(body))
	if err != nil {
		return Record{}, false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if h.tokenFn != nil {
		t, err := h.tokenFn()
		if err != nil {
			return Record{}, false, fmt.Errorf("fetch token: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+t)
	}

	resp, err := h.http.Do(req)
	if err != nil {
		return Record{}, false, fmt.Errorf("integration call: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == http.StatusBadGateway, resp.StatusCode == http.StatusServiceUnavailable:
		// Pre-classified upstream issue from the integration service.
		// Surface as ErrUpstreamUnavailable so the verification
		// handler's existing 502 path triggers.
		return Record{}, false, ErrUpstreamUnavailable
	case resp.StatusCode != http.StatusOK:
		return Record{}, false, fmt.Errorf("integration HTTP %d: %s", resp.StatusCode, truncate(raw, 200))
	}

	var out integrationResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return Record{}, false, fmt.Errorf("decode integration response: %w", err)
	}
	if !out.Found {
		return Record{}, false, nil
	}
	return Record{
		NIN: out.Record.NIN, GivenName: out.Record.GivenName, MiddleName: out.Record.MiddleName,
		Surname: out.Record.Surname, DateOfBirth: out.Record.DateOfBirth, Sex: out.Record.Sex,
		Nationality: out.Record.Nationality, MotherMaidenName: out.Record.MotherMaidenName,
		Status:        mapStatus(out.Record.Status),
		LastUpdatedAt: out.Record.LastUpdatedAt,
	}, true, nil
}

func mapStatus(s string) Status {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "deceased":
		return StatusDeceased
	case "revoked":
		return StatusRevoked
	default:
		return StatusActive
	}
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}

// silence linters when only some imports are used in current branch.
var _ = errors.New
