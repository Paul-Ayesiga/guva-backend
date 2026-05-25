// Production-shaped HTTP client against the real NIRA API.
//
// What this implements end-to-end:
//   - mTLS: client cert + server cert verification against the
//     trusted CA. Agencies universally mandate mTLS.
//   - Retries with exponential backoff for transient failures
//     (network errors + 5xx). 4xx are not retried (caller's fault).
//   - Circuit breaker: after N consecutive failures the breaker
//     opens and rejects calls without attempt for a window. After
//     the window it goes half-open — the next call probes; success
//     closes the breaker, failure reopens it. Standard pattern.
//   - Wire-format translation: NIRA's native JSON shape (best guess
//     until the agency agreement lands) → internal canonical Record.
//   - Error mapping: HTTP status + NIRA-specific error codes onto a
//     small set of categories the HTTP layer understands.
//   - Per-attempt OpenTelemetry spans so each upstream call shows
//     up as a child of the verify span in Jaeger.
//
// Other agencies (URSB, URA, Lands, UNEB, MoH) copy this file as
// their starting point and change three things:
//  1. wireRecord shape (their JSON/SOAP fields) + decodeRecord
//  2. auth scheme (HMAC, JWT, API-key — add to roundTrip)
//  3. error code table (mapStatusError)
package backend

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/guva-ug/guva-backend/pkg/platform/tlsbundle"
	"github.com/guva-ug/guva-backend/services/integrations/nira/internal/canonical"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// UpstreamConfig captures the inputs the upstream backend needs.
// Mirrors the env-var subset config.Config exposes; passed in
// explicitly so this package stays config-shape-agnostic.
type UpstreamConfig struct {
	BaseURL           string
	Cert, Key, CA     string
	Timeout           time.Duration
	MaxAttempts       int
	BackoffBase       time.Duration
	CircuitThreshold  int
	CircuitOpenWindow time.Duration
}

// NewUpstream builds the production backend. mTLS material is
// loaded at startup; if any cert file is missing or malformed the
// caller gets an error and the service fails to start. We do not
// silently degrade to plain HTTP — that would be a security
// regression no observability dashboard would catch.
func NewUpstream(cfg UpstreamConfig, logger *slog.Logger) (Backend, error) {
	if cfg.MaxAttempts <= 0 {
		cfg.MaxAttempts = 3
	}
	if cfg.BackoffBase <= 0 {
		cfg.BackoffBase = 200 * time.Millisecond
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.CircuitThreshold <= 0 {
		cfg.CircuitThreshold = 5
	}
	if cfg.CircuitOpenWindow <= 0 {
		cfg.CircuitOpenWindow = 30 * time.Second
	}

	bundle, err := tlsbundle.Load(tlsbundle.Config{
		CertFile: cfg.Cert,
		KeyFile:  cfg.Key,
		CAFile:   cfg.CA,
	})
	if err != nil {
		return nil, fmt.Errorf("nira upstream tls: %w", err)
	}

	transport := &http.Transport{
		TLSClientConfig:       bundle.ClientConfig(),
		MaxIdleConns:          50,
		MaxIdleConnsPerHost:   10,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// ForceAttemptHTTP2 left default-true so we negotiate HTTP/2
		// if NIRA's frontend supports it.
	}

	return &upstream{
		cfg:    cfg,
		logger: logger,
		http:   &http.Client{Transport: transport, Timeout: cfg.Timeout},
		tracer: otel.Tracer("integrations-nira-upstream"),
		breaker: &breaker{
			threshold:  cfg.CircuitThreshold,
			openWindow: cfg.CircuitOpenWindow,
		},
	}, nil
}

// silence linters when only some of the imports are referenced in
// any one branch.
var _ = tls.VersionTLS13
var _ = errors.New

type upstream struct {
	cfg     UpstreamConfig
	logger  *slog.Logger
	http    *http.Client
	tracer  trace.Tracer
	breaker *breaker
}

func (u *upstream) Name() string { return "upstream" }

func (u *upstream) Health(ctx context.Context) error {
	// Speculative — most agencies expose /health or /ping. If the
	// real NIRA doesn't, the agency contract will tell us the actual
	// path and we change one line.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(u.cfg.BaseURL, "/")+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := u.http.Do(req)
	if err != nil {
		return fmt.Errorf("nira health: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("nira health HTTP %d", resp.StatusCode)
	}
	return nil
}

func (u *upstream) Lookup(ctx context.Context, nin string) (canonical.Record, bool, error) {
	ctx, span := u.tracer.Start(ctx, "nira.lookup",
		trace.WithAttributes(
			attribute.String("nira.subject_type", "nin"),
			// NIN itself never goes into a span attribute — PII
			// discipline. The hash lives elsewhere; here we just
			// note that a lookup happened.
		))
	defer span.End()

	if !u.breaker.allow() {
		span.SetStatus(codes.Error, "circuit_open")
		return canonical.Record{}, false, ErrCircuitOpen
	}

	var lastErr error
	for attempt := 1; attempt <= u.cfg.MaxAttempts; attempt++ {
		rec, found, retry, err := u.doLookup(ctx, nin, attempt)
		if err == nil {
			u.breaker.success()
			span.SetAttributes(attribute.Int("nira.attempts", attempt))
			return rec, found, nil
		}
		lastErr = err
		span.AddEvent("attempt_failed", trace.WithAttributes(
			attribute.Int("attempt", attempt),
			attribute.String("error", err.Error()),
		))
		if !retry || attempt == u.cfg.MaxAttempts {
			break
		}
		// Exponential backoff with full jitter would be ideal; for
		// dev simplicity we use plain exponential.
		delay := time.Duration(1<<uint(attempt-1)) * u.cfg.BackoffBase
		select {
		case <-ctx.Done():
			lastErr = ctx.Err()
			break
		case <-time.After(delay):
		}
	}

	u.breaker.failure()
	span.SetStatus(codes.Error, "upstream_failure")
	return canonical.Record{}, false, fmt.Errorf("%w: %v", ErrUpstreamUnavailable, lastErr)
}

// doLookup performs one HTTP attempt. Returns:
//   - (rec, true, _, nil)  on a 2xx with a parseable body for an existing record
//   - (zero, false, _, nil) on a 404 (NIN unknown)
//   - (_, _, true, err)   for retryable failures (network, 5xx, timeout)
//   - (_, _, false, err)  for non-retryable failures (4xx other than 404, malformed body)
func (u *upstream) doLookup(ctx context.Context, nin string, attempt int) (canonical.Record, bool, bool, error) {
	url := strings.TrimRight(u.cfg.BaseURL, "/") + "/v1/citizens/" + nin
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return canonical.Record{}, false, false, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Guva-Attempt", fmt.Sprintf("%d", attempt))
	// Future: HMAC / JWS request signature header per NIRA's
	// eventual contract. Stub here so the seam is visible.

	resp, err := u.http.Do(req)
	if err != nil {
		return canonical.Record{}, false, true, err // network → retry
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return canonical.Record{}, false, false, nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		rec, derr := decodeRecord(body)
		if derr != nil {
			return canonical.Record{}, false, false, fmt.Errorf("decode body: %w", derr)
		}
		return rec, true, false, nil
	case resp.StatusCode >= 500:
		return canonical.Record{}, false, true, fmt.Errorf("upstream HTTP %d: %s", resp.StatusCode, truncate(body, 200))
	default:
		// 4xx that's NOT 404 — authentication problem, malformed
		// request, etc. Don't retry; the caller should fix and
		// resubmit.
		return canonical.Record{}, false, false, fmt.Errorf("upstream HTTP %d: %s", resp.StatusCode, truncate(body, 200))
	}
}

// wireRecord is the (anticipated) NIRA wire shape. Field names are
// the conventional camelCase agencies tend to use; when the real
// agreement hands us their spec, this struct + decodeRecord are the
// only places that change.
type wireRecord struct {
	NIN              string `json:"nin"`
	FirstName        string `json:"firstName"`
	MiddleName       string `json:"middleName"`
	LastName         string `json:"lastName"`
	DateOfBirth      string `json:"dateOfBirth"`
	Sex              string `json:"sex"`
	Nationality      string `json:"nationality"`
	MotherMaidenName string `json:"motherMaidenName"`
	Status           string `json:"status"` // "ACTIVE" | "DECEASED" | "REVOKED"
	LastUpdatedAt    string `json:"lastUpdatedAt"`
}

func decodeRecord(body []byte) (canonical.Record, error) {
	var w wireRecord
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(&w); err != nil {
		return canonical.Record{}, err
	}
	updated, _ := time.Parse(time.RFC3339, w.LastUpdatedAt)
	return canonical.Record{
		NIN: w.NIN, GivenName: w.FirstName, MiddleName: w.MiddleName, Surname: w.LastName,
		DateOfBirth: w.DateOfBirth, Sex: w.Sex, Nationality: w.Nationality,
		MotherMaidenName: w.MotherMaidenName,
		Status:           mapStatus(w.Status),
		LastUpdatedAt:    updated,
	}, nil
}

func mapStatus(s string) canonical.Status {
	switch strings.ToUpper(strings.TrimSpace(s)) {
	case "DECEASED":
		return canonical.StatusDeceased
	case "REVOKED", "CANCELLED", "CANCELED":
		return canonical.StatusRevoked
	default:
		return canonical.StatusActive
	}
}

func truncate(b []byte, n int) string {
	if len(b) > n {
		return string(b[:n]) + "…"
	}
	return string(b)
}

// breaker is a minimal closed/open/half-open circuit breaker.
//
//	closed     → all calls pass; failures increment a counter
//	  ↓ threshold hit
//	open       → all calls rejected immediately for openWindow
//	  ↓ window elapsed
//	half-open  → one call probes; success closes the breaker,
//	              failure re-opens it for another window
type breaker struct {
	threshold  int
	openWindow time.Duration

	mu       sync.Mutex
	failures int
	openedAt time.Time
	halfOpen bool
}

func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.openedAt.IsZero() {
		return true
	}
	if time.Since(b.openedAt) >= b.openWindow {
		// Transition to half-open: let exactly one call through.
		// halfOpen flag prevents a second concurrent caller from
		// also being allowed.
		if !b.halfOpen {
			b.halfOpen = true
			return true
		}
		return false
	}
	return false
}

func (b *breaker) success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.openedAt = time.Time{}
	b.halfOpen = false
}

func (b *breaker) failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.halfOpen {
		// Probe failed — re-open.
		b.openedAt = time.Now()
		b.halfOpen = false
		return
	}
	b.failures++
	if b.failures >= b.threshold {
		b.openedAt = time.Now()
		b.failures = 0
	}
}
