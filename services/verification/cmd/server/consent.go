// Bootstrap glue between the platform-level pkg/platform/consent
// client and the verification service's local ConsentChecker
// interface. Lives in cmd/ rather than internal/ so internal/server
// stays decoupled from the platform client + token-fetcher concerns.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/guva-ug/guva-backend/pkg/platform/consent"
	"github.com/guva-ug/guva-backend/services/verification/internal/server"
)

// newConsentChecker returns a server.ConsentChecker backed by
// pkg/platform/consent.Client, with a token cache to avoid fetching
// a fresh access token on every verify call. The token is refreshed
// 30s before expiry.
func newConsentChecker(baseURL, tokenURL, clientID, clientSecret string, logger *slog.Logger) server.ConsentChecker {
	tf := newTokenFetcher(tokenURL, clientID, clientSecret, logger)
	client := consent.NewClient(consent.Config{
		BaseURL: baseURL,
		Timeout: 5 * time.Second,
		TokenFn: tf.Token,
	})
	return &consentCheckerAdapter{client: client}
}

type consentCheckerAdapter struct {
	client *consent.Client
}

func (a *consentCheckerAdapter) VerifyGrant(ctx context.Context, grantID, consumerID string, attrs []string) (server.ConsentResult, error) {
	r, err := a.client.VerifyGrant(ctx, grantID, consumerID, attrs)
	if err != nil {
		return server.ConsentResult{}, err
	}
	return server.ConsentResult{
		Outcome:      string(r.Outcome),
		AssertionJWT: r.AssertionJWT,
	}, nil
}

// tokenFetcher caches a client-credentials access token and refreshes
// when it's near expiry. Single-flight under a mutex so concurrent
// callers share one in-flight refresh.
type tokenFetcher struct {
	tokenURL, clientID, clientSecret string
	logger                           *slog.Logger
	http                             *http.Client

	mu          sync.Mutex
	cachedToken string
	expiresAt   time.Time
}

func newTokenFetcher(tokenURL, clientID, clientSecret string, logger *slog.Logger) *tokenFetcher {
	return &tokenFetcher{
		tokenURL: tokenURL, clientID: clientID, clientSecret: clientSecret,
		logger: logger,
		http:   &http.Client{Timeout: 5 * time.Second},
	}
}

func (t *tokenFetcher) Token() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cachedToken != "" && time.Until(t.expiresAt) > 30*time.Second {
		return t.cachedToken, nil
	}
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", t.clientID)
	form.Set("client_secret", t.clientSecret)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := t.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("token request: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token endpoint HTTP %d: %s", resp.StatusCode, string(body))
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("decode token response: %w", err)
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("token response missing access_token")
	}
	ttl := time.Duration(out.ExpiresIn) * time.Second
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	t.cachedToken = out.AccessToken
	t.expiresAt = time.Now().Add(ttl)
	t.logger.Info("consent token refreshed", "expires_in", ttl)
	return t.cachedToken, nil
}
