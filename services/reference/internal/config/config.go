// Package config loads service configuration from environment variables.
// Defaults are tuned for the local docker-compose stack documented in the
// repository root.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

type Config struct {
	ServiceName string
	HTTPAddr    string
	LogLevel    slog.Level

	OTLPEndpoint string
	Environment  string

	VaultAddr  string
	VaultToken string
}

func Load() (Config, error) {
	cfg := Config{
		ServiceName:  envOr("OTEL_SERVICE_NAME", "reference"),
		HTTPAddr:     envOr("REFERENCE_HTTP_ADDR", ":7070"),
		OTLPEndpoint: envOr("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4317"),
		Environment:  envOr("OTEL_SERVICE_NAMESPACE", "local"),
		VaultAddr:    envOr("VAULT_ADDR", "http://localhost:8200"),
		VaultToken:   envOr("VAULT_TOKEN", "dev-root-token"),
	}

	level, err := parseLevel(envOr("REFERENCE_LOG_LEVEL", "info"))
	if err != nil {
		return Config{}, err
	}
	cfg.LogLevel = level

	if cfg.HTTPAddr == "" {
		return Config{}, errors.New("REFERENCE_HTTP_ADDR must not be empty")
	}
	return cfg, nil
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info", "":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("invalid log level %q", s)
	}
}
