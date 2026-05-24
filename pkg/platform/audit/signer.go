// Signer loading for SIEM export bundles.
//
// The audit service signs every exported bundle with Ed25519. The
// private key lives in Vault at secret/services/audit/config:
// signing-key-b64 (base64 of the 64-byte Ed25519 private key — 32 seed
// bytes concatenated with the 32-byte public key, the Go stdlib format).
//
// At startup the service:
//   1. Tries to read the key from Vault.
//   2. If present + parseable: uses it. Logs the fingerprint.
//   3. If missing or unparseable: generates a fresh key, writes it back
//      to Vault, and logs a loud warning. This keeps dev bootable
//      without a manual key-seeding step; production operators seed
//      the key once at provisioning time so the key is stable and
//      verifiers know the public key in advance.
//
// The fallback "generate-on-startup" path will produce a fresh key on
// every Vault reset (Vault dev mode is in-memory). External verifiers
// must always re-fetch the current public key from /v1/audit/export/pubkey
// rather than caching one. The pubkey field inside every bundle also
// names exactly which key signed.

package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
)

// SecretReadWriter is the small surface of pkg/secrets.Client that the
// signer loader needs. Declaring it as an interface here keeps audit
// from importing the secrets package directly (preserves the platform
// package's loose-coupling rule).
type SecretReadWriter interface {
	GetString(ctx context.Context, path, key string) (string, error)
	Put(ctx context.Context, path string, data map[string]string) error
}

// SignerConfig captures the inputs to LoadOrCreateSigner.
type SignerConfig struct {
	Vault     SecretReadWriter
	VaultPath string // typical: "services/audit/config"
	VaultKey  string // typical: "signing-key-b64"
	Logger    *slog.Logger
}

// LoadOrCreateSigner returns an ed25519 private key from Vault, or
// generates and persists one if Vault has none. Always returns a
// usable key on success; only returns error if Vault is reachable
// AND rejects the write of a freshly-generated key.
func LoadOrCreateSigner(ctx context.Context, cfg SignerConfig) (ed25519.PrivateKey, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	raw, err := cfg.Vault.GetString(ctx, cfg.VaultPath, cfg.VaultKey)
	if err == nil && raw != "" {
		if priv, perr := parsePrivateKey(raw); perr == nil {
			cfg.Logger.Info("audit signing key loaded from vault",
				"path", cfg.VaultPath, "key", cfg.VaultKey,
				"pubkey_fingerprint", fingerprint(priv.Public().(ed25519.PublicKey)))
			return priv, nil
		} else {
			cfg.Logger.Warn("audit signing key in vault is unparseable; regenerating",
				"path", cfg.VaultPath, "key", cfg.VaultKey, "error", perr)
		}
	}

	// Generate a fresh key and stash it back so subsequent restarts in
	// the same Vault session reuse it. This is the dev path; in prod
	// the operator seeds the key once at provisioning and rotates by
	// re-running the same seed step under change control.
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate ed25519 key: %w", err)
	}
	encoded := base64.StdEncoding.EncodeToString(priv)
	if perr := cfg.Vault.Put(ctx, cfg.VaultPath, map[string]string{cfg.VaultKey: encoded}); perr != nil {
		// Don't fail startup on Vault write — the key is still usable
		// for this process. Just be loud.
		cfg.Logger.Error("could not persist audit signing key to vault; will regenerate on next restart",
			"path", cfg.VaultPath, "key", cfg.VaultKey, "error", perr)
	} else {
		cfg.Logger.Warn("audit signing key auto-generated and stashed in vault — dev only",
			"path", cfg.VaultPath, "key", cfg.VaultKey,
			"pubkey_fingerprint", fingerprint(priv.Public().(ed25519.PublicKey)),
			"action", "for production, seed this key out-of-band before first start")
	}
	return priv, nil
}

func parsePrivateKey(b64 string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("decode base64: %w", err)
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("expected %d bytes, got %d", ed25519.PrivateKeySize, len(raw))
	}
	return ed25519.PrivateKey(raw), nil
}

// fingerprint returns the first 8 bytes of SHA-256(pubkey) as hex —
// a compact identifier for logs that's unambiguous without exposing
// the whole key.
func fingerprint(pub ed25519.PublicKey) string {
	if len(pub) == 0 {
		return ""
	}
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:8])
}

// PublicKeyOf returns the public half of a private key. Convenience
// for handlers that need to expose the pubkey alone.
func PublicKeyOf(priv ed25519.PrivateKey) (ed25519.PublicKey, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, errors.New("private key has wrong length")
	}
	return priv.Public().(ed25519.PublicKey), nil
}
