// Package signing builds + verifies the Ed25519-signed assertion that
// rides on every consent grant.
//
// The assertion is a minimal JWT-like envelope: a base64url-encoded
// header + payload + signature, joined with dots. It's NOT a full
// JWT (no alg negotiation, no issuer discovery, no nbf) — we
// control both ends, so a lean format is better than a full RFC
// implementation that brings dependencies.
//
// Format:  base64url(header_json) . base64url(payload_json) . base64url(sig)
//
//	header_json  = {"alg":"Ed25519","kid":"<key id>"}
//	payload_json = the Assertion struct, canonical JSON (sorted keys)
//	sig          = Ed25519(header_json + "." + payload_json)
//
// Reuses the same dev-key bootstrap pattern as audit signing: load
// from Vault on startup, generate-on-first-boot if missing, log the
// fingerprint so operators can spot rotation in audit chains.
package signing

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Assertion is the payload an external auditor verifies. Carries the
// substantive fields that bind consumer + citizen + scope + window
// together. The JWT-like envelope around it provides authenticity.
type Assertion struct {
	GrantID            string    `json:"grant_id"`
	Issuer             string    `json:"iss"` // "guva-consent"
	IssuedAt           int64     `json:"iat"` // unix epoch seconds
	ExpiresAt          int64     `json:"exp"`
	CitizenSubjectType string    `json:"sub_type"`
	CitizenSubjectHash string    `json:"sub_hash"`
	ConsumerID         string    `json:"consumer_id"`
	Upstream           string    `json:"upstream"`
	Purpose            string    `json:"purpose"`
	AllowedAttributes  []string  `json:"allowed_attributes"`
	IssuedAtTime       time.Time `json:"-"` // convenience for handlers; not serialised
}

// Signer holds the active private key + its identifier ("fingerprint
// of the public key", first 8 hex chars). Rotation = swap in a new
// Signer; old assertions stay verifiable as long as a Verifier is
// constructed with the historical public key.
type Signer struct {
	priv  ed25519.PrivateKey
	keyID string
}

// NewSigner wraps a private key and computes its key id.
func NewSigner(priv ed25519.PrivateKey) *Signer {
	pub := priv.Public().(ed25519.PublicKey)
	sum := sha256.Sum256(pub)
	return &Signer{priv: priv, keyID: hex.EncodeToString(sum[:8])}
}

func (s *Signer) KeyID() string                { return s.keyID }
func (s *Signer) PublicKey() ed25519.PublicKey { return s.priv.Public().(ed25519.PublicKey) }

// Sign serialises the assertion + computes the JWT-like compact form.
func (s *Signer) Sign(a Assertion) (string, error) {
	header := map[string]string{"alg": "Ed25519", "kid": s.keyID}
	hb, _ := json.Marshal(header)
	pb, err := json.Marshal(a)
	if err != nil {
		return "", fmt.Errorf("marshal assertion: %w", err)
	}
	signingInput := base64.RawURLEncoding.EncodeToString(hb) +
		"." + base64.RawURLEncoding.EncodeToString(pb)
	sig := ed25519.Sign(s.priv, []byte(signingInput))
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Verifier checks a token against a known public key. Use the same
// Signer's public key in-process, or pass a historical public key
// when validating older grants.
type Verifier struct {
	pub   ed25519.PublicKey
	keyID string
}

func NewVerifier(pub ed25519.PublicKey) *Verifier {
	sum := sha256.Sum256(pub)
	return &Verifier{pub: pub, keyID: hex.EncodeToString(sum[:8])}
}

// Verify returns the parsed assertion on success, error on bad
// signature / malformed token / wrong key id.
func (v *Verifier) Verify(token string) (Assertion, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return Assertion{}, errors.New("malformed assertion: expected 3 segments")
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return Assertion{}, fmt.Errorf("decode header: %w", err)
	}
	var header struct {
		Alg, Kid string
	}
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return Assertion{}, fmt.Errorf("parse header: %w", err)
	}
	if header.Alg != "Ed25519" {
		return Assertion{}, fmt.Errorf("unsupported alg %q", header.Alg)
	}
	if header.Kid != v.keyID {
		return Assertion{}, fmt.Errorf("kid mismatch: token=%s verifier=%s", header.Kid, v.keyID)
	}
	sigBytes, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return Assertion{}, fmt.Errorf("decode signature: %w", err)
	}
	if !ed25519.Verify(v.pub, []byte(parts[0]+"."+parts[1]), sigBytes) {
		return Assertion{}, errors.New("signature does not verify")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Assertion{}, fmt.Errorf("decode payload: %w", err)
	}
	var a Assertion
	if err := json.Unmarshal(payload, &a); err != nil {
		return Assertion{}, fmt.Errorf("parse payload: %w", err)
	}
	a.IssuedAtTime = time.Unix(a.IssuedAt, 0).UTC()
	return a, nil
}

// VaultReadWriter is the small surface of pkg/secrets.Client the
// loader needs. Mirrors pkg/platform/audit.SecretReadWriter so we
// don't take a hard dep on the secrets package from this internal pkg.
type VaultReadWriter interface {
	GetString(ctx interface{ Done() <-chan struct{} }, path, key string) (string, error)
	Put(ctx interface{ Done() <-chan struct{} }, path string, data map[string]string) error
}

// EncodePublicKey is the std-base64 form of a public key, suitable
// for the /signing-key endpoint and external verifier configuration.
func EncodePublicKey(pub ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub)
}

// ParsePublicKey is the inverse of EncodePublicKey.
func ParsePublicKey(b64 string) (ed25519.PublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("decode base64: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("expected %d bytes, got %d", ed25519.PublicKeySize, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}

// Generate creates a fresh Ed25519 keypair. Use during dev when Vault
// has nothing; the caller is responsible for persisting it.
func Generate() (ed25519.PrivateKey, error) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	return priv, err
}

// EncodePrivateKey returns the standard 64-byte Go format (32 seed +
// 32 public) as base64-std, matching how audit signer stores its key.
func EncodePrivateKey(priv ed25519.PrivateKey) string {
	return base64.StdEncoding.EncodeToString(priv)
}

// ParsePrivateKey decodes from the same format.
func ParsePrivateKey(b64 string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("decode base64: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("expected %d bytes, got %d", ed25519.PrivateKeySize, len(raw))
	}
	return ed25519.PrivateKey(raw), nil
}
