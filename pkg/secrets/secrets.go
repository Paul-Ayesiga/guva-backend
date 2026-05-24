// Package secrets provides a minimal client for fetching secrets from
// HashiCorp Vault's KV-v2 secrets engine. No external dependencies: the
// Vault HTTP API is simple enough that hashicorp/vault/api's transitive
// bloat (vault-sdk, mapstructure, multierror, …) isn't justified here.
//
// In local dev, services authenticate with a static token (Vault dev
// mode root token or a per-service token issued by a bootstrap script).
// In staging and production, services should switch to a real auth
// method (Kubernetes service-account auth, AppRole, or workload
// identity) — wrap NewClient with the relevant token-fetching dance.
//
// Usage:
//
//	c, err := secrets.NewClient(secrets.Config{
//	    Addr: os.Getenv("VAULT_ADDR"),
//	    Token: os.Getenv("VAULT_TOKEN"),
//	})
//	if err != nil { ... }
//
//	greeting, err := c.GetString(ctx, "services/reference/config", "greeting")
//	if err != nil { ... }
package secrets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Config captures the per-process inputs to NewClient.
type Config struct {
	// Addr is the base Vault URL (e.g. "http://vault:8200"). Required.
	Addr string

	// Token is the static Vault token. Required for the local-dev
	// path; ignored if the caller wires a custom RoundTripper that
	// injects auth headers another way.
	Token string

	// Mount is the KV-v2 mount path (defaults to "secret").
	Mount string

	// Timeout for each HTTP call. Defaults to 5s.
	Timeout time.Duration

	// HTTPClient overrides the underlying client; useful for tests
	// and for plumbing custom auth via a RoundTripper. Optional.
	HTTPClient *http.Client
}

// Client reads secrets from Vault's KV-v2 engine.
type Client struct {
	addr    string
	token   string
	mount   string
	timeout time.Duration
	http    *http.Client
}

// NewClient validates its config and returns a Client ready to use.
func NewClient(cfg Config) (*Client, error) {
	if cfg.Addr == "" {
		return nil, errors.New("secrets: Addr is required")
	}
	if cfg.HTTPClient == nil && cfg.Token == "" {
		return nil, errors.New("secrets: Token is required (or supply a custom HTTPClient)")
	}
	if cfg.Mount == "" {
		cfg.Mount = "secret"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: cfg.Timeout}
	}
	return &Client{
		addr:    strings.TrimRight(cfg.Addr, "/"),
		token:   cfg.Token,
		mount:   cfg.Mount,
		timeout: cfg.Timeout,
		http:    cfg.HTTPClient,
	}, nil
}

// Get returns every key/value pair at the given KV-v2 path.
func (c *Client) Get(ctx context.Context, path string) (map[string]string, error) {
	url := fmt.Sprintf("%s/v1/%s/data/%s", c.addr, c.mount, strings.TrimLeft(path, "/"))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("X-Vault-Token", c.token)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("vault GET %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("secret %q not found", path)
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("vault GET %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}

	var envelope struct {
		Data struct {
			Data map[string]string `json:"data"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, fmt.Errorf("decode vault response: %w", err)
	}
	return envelope.Data.Data, nil
}

// GetString fetches a single key from the secret at the given path.
// Returns an error if the path is missing or the key isn't present.
func (c *Client) GetString(ctx context.Context, path, key string) (string, error) {
	kv, err := c.Get(ctx, path)
	if err != nil {
		return "", err
	}
	v, ok := kv[key]
	if !ok {
		return "", fmt.Errorf("key %q not present at %q", key, path)
	}
	return v, nil
}

// MustGetString is GetString that panics on error. Use only during
// startup, in main(), where the service has no useful work without
// the secret.
func (c *Client) MustGetString(ctx context.Context, path, key string) string {
	v, err := c.GetString(ctx, path, key)
	if err != nil {
		panic(fmt.Sprintf("secrets: required secret %s:%s missing: %v", path, key, err))
	}
	return v
}
