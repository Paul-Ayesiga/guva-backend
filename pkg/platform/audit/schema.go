// Schema validation for audit envelopes.
//
// Apicurio Registry holds the canonical JSON Schema for the envelope
// (group=guva-audit, artifactId=audit-event-envelope). At startup each
// producer constructs a Validator that fetches the latest version and
// caches it in memory. If the registry is unreachable, the binary's
// embedded copy of the same file is used as a fallback — this keeps
// services bootable in air-gapped or smoke-test scenarios.
//
// Validation is invoked by Emit before the outbox insert: a non-conforming
// envelope returns an error and the caller's transaction rolls back,
// so the audit chain only ever receives well-formed events.

package audit

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

//go:embed schemas/audit-event-envelope-v1.json
var EmbeddedEnvelopeSchema []byte

// EmbeddedEnvelopeSchemaSHA256 is a stable digest of the bytes a binary
// shipped with. Logged at Validator construction so operators can spot
// drift between the binary and the registry-served version.
var EmbeddedEnvelopeSchemaSHA256 = func() string {
	h := sha256.Sum256(EmbeddedEnvelopeSchema)
	return hex.EncodeToString(h[:])
}()

// Validator compiles a JSON Schema and validates envelopes against it.
// Construct one per process; the zero value is not usable.
type Validator struct {
	schema       *jsonschema.Schema
	source       string // "registry" or "embedded"
	digest       string // sha256 of the schema bytes actually loaded
	envelopeOnly bool
}

// ValidatorConfig captures the inputs to NewValidator.
type ValidatorConfig struct {
	// RegistryURL is the Apicurio base URL (e.g. http://localhost:8081).
	// Empty disables registry lookup and forces the embedded schema.
	RegistryURL string
	// Group + ArtifactID locate the schema in the registry.
	Group      string // typical: "guva-audit"
	ArtifactID string // typical: "audit-event-envelope"
	// Logger is used to report which source the schema came from and
	// to warn on fallback.
	Logger *slog.Logger
	// FetchTimeout caps how long we wait for the registry on startup.
	// Defaults to 5s.
	FetchTimeout time.Duration
}

// NewValidator fetches the schema from Apicurio (with fallback to the
// embedded copy) and compiles it. Returns an error only if both the
// registry fetch AND the embedded schema fail to compile, which would
// indicate a programmer error in the schema file shipped with the
// binary — failing fast is correct.
func NewValidator(ctx context.Context, cfg ValidatorConfig) (*Validator, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.FetchTimeout == 0 {
		cfg.FetchTimeout = 5 * time.Second
	}

	var (
		raw    []byte
		source string
	)

	if cfg.RegistryURL != "" && cfg.Group != "" && cfg.ArtifactID != "" {
		fetched, err := fetchSchema(ctx, cfg)
		if err == nil {
			raw = fetched
			source = "registry"
			cfg.Logger.Info("audit schema loaded from registry",
				"url", cfg.RegistryURL,
				"group", cfg.Group,
				"artifact", cfg.ArtifactID)
		} else {
			cfg.Logger.Warn("audit schema registry fetch failed; falling back to embedded",
				"error", err,
				"url", cfg.RegistryURL,
				"embedded_sha256", EmbeddedEnvelopeSchemaSHA256)
		}
	}

	if raw == nil {
		raw = EmbeddedEnvelopeSchema
		source = "embedded"
	}

	compiled, err := compileSchema(raw)
	if err != nil {
		return nil, fmt.Errorf("compile envelope schema (%s): %w", source, err)
	}

	h := sha256.Sum256(raw)
	digest := hex.EncodeToString(h[:])

	v := &Validator{schema: compiled, source: source, digest: digest, envelopeOnly: true}
	if source == "registry" && digest != EmbeddedEnvelopeSchemaSHA256 {
		cfg.Logger.Warn("registry schema differs from embedded copy",
			"registry_sha256", digest,
			"embedded_sha256", EmbeddedEnvelopeSchemaSHA256,
			"action", "using registry; ship a new binary to align embed")
	}
	return v, nil
}

// Source reports where the active schema came from ("registry" or "embedded").
func (v *Validator) Source() string { return v.source }

// Digest reports a hex SHA-256 of the schema bytes the Validator compiled.
func (v *Validator) Digest() string { return v.digest }

// Validate checks that the given envelope JSON conforms to the schema.
// Returns nil on success. The returned error includes the failing
// JSON Pointer so producers can surface "which field is wrong" without
// guessing.
func (v *Validator) Validate(envelope []byte) error {
	var doc any
	if err := json.Unmarshal(envelope, &doc); err != nil {
		return fmt.Errorf("envelope is not valid JSON: %w", err)
	}
	if err := v.schema.Validate(doc); err != nil {
		return fmt.Errorf("envelope failed schema validation: %w", err)
	}
	return nil
}

// fetchSchema retrieves the latest version of the artifact body from
// Apicurio v2. The body is the schema JSON itself (Apicurio returns the
// artifact content directly, not wrapped).
func fetchSchema(ctx context.Context, cfg ValidatorConfig) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, cfg.FetchTimeout)
	defer cancel()

	url := strings.TrimRight(cfg.RegistryURL, "/") +
		"/apis/registry/v2/groups/" + cfg.Group +
		"/artifacts/" + cfg.ArtifactID
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("registry GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MB cap
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("registry returned HTTP %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

// compileSchema turns raw bytes into a compiled JSON Schema. The compiler
// is given a stable URL so error messages reference the artifact, not
// an anonymous string.
func compileSchema(raw []byte) (*jsonschema.Schema, error) {
	if len(raw) == 0 {
		return nil, errors.New("empty schema bytes")
	}
	compiler := jsonschema.NewCompiler()
	// JSON Schema 2020-12 treats `format` as advisory by default; we
	// rely on uuid and date-time being enforced (id must parse, time
	// must be ISO 8601), so flip on assertion.
	compiler.AssertFormat = true
	if err := compiler.AddResource("audit-envelope.json", strings.NewReader(string(raw))); err != nil {
		return nil, fmt.Errorf("add resource: %w", err)
	}
	return compiler.Compile("audit-envelope.json")
}

// Default validator wiring — each producer's main calls
// audit.SetDefaultValidator(v) at startup. Emit consults this on every
// call; nil means "no validation" (preserves backward compat for the
// rare caller that doesn't wire one up).
var defaultValidator atomic.Pointer[Validator]

// SetDefaultValidator installs the package-level validator used by Emit.
// Safe to call from multiple goroutines; the most recent call wins.
// Passing nil disables validation.
func SetDefaultValidator(v *Validator) { defaultValidator.Store(v) }

// DefaultValidator returns the currently-installed validator, or nil.
func DefaultValidator() *Validator { return defaultValidator.Load() }
