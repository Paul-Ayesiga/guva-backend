// Package config loads verification-service configuration from env.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

type Config struct {
	ServiceName  string
	HTTPAddr     string
	LogLevel     slog.Level
	Environment  string
	OTLPEndpoint string

	DBHost, DBPort, DBUser, DBName, DBSSLMode string

	// Vault — for DB password resolution at startup.
	VaultAddr  string
	VaultToken string

	// Audit emission
	KafkaBrokers    []string
	KafkaAuditTopic string
	ApicurioURL     string

	// Cache TTL. Zero means "no cache" — every call hits the upstream.
	CacheTTL time.Duration

	// NIRA adapter mode: "mock" (in-memory test set) or "live" (real
	// NIRA endpoint, not implemented yet). Default mock.
	NIRAMode     string
	NIRAEndpoint string
}

func Load() (Config, error) {
	cfg := Config{
		ServiceName:     envOr("OTEL_SERVICE_NAME", "verification"),
		HTTPAddr:        envOr("VERIFICATION_HTTP_ADDR", ":7075"),
		Environment:     envOr("OTEL_SERVICE_NAMESPACE", "local"),
		OTLPEndpoint:    envOr("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4317"),
		DBHost:          envOr("DB_HOST", "localhost"),
		DBPort:          envOr("DB_PORT", "5432"),
		DBUser:          envOr("DB_USER", "guva"),
		DBName:          envOr("DB_NAME", "guva_verification"),
		DBSSLMode:       envOr("DB_SSLMODE", "disable"),
		VaultAddr:       envOr("VAULT_ADDR", "http://localhost:8200"),
		VaultToken:      envOr("VAULT_TOKEN", "dev-root-token"),
		KafkaBrokers:    splitCSV(envOr("KAFKA_BROKERS", "localhost:9094")),
		KafkaAuditTopic: envOr("KAFKA_AUDIT_TOPIC", "ug.go.guva.audit.entry.appended.v1"),
		ApicurioURL:     envOr("APICURIO_URL", "http://localhost:8081"),
		CacheTTL:        parseDurOr(envOr("VERIFICATION_CACHE_TTL", "15m"), 15*time.Minute),
		NIRAMode:        envOr("NIRA_MODE", "mock"),
		NIRAEndpoint:    os.Getenv("NIRA_ENDPOINT"),
	}
	level, err := parseLevel(envOr("VERIFICATION_LOG_LEVEL", "info"))
	if err != nil {
		return Config{}, err
	}
	cfg.LogLevel = level
	if cfg.HTTPAddr == "" {
		return Config{}, errors.New("VERIFICATION_HTTP_ADDR must not be empty")
	}
	return cfg, nil
}

func (c Config) DSN(password string) string {
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		c.DBHost, c.DBPort, c.DBUser, password, c.DBName, c.DBSSLMode)
}

func envOr(k, fb string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return fb
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
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

func parseDurOr(s string, fb time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	return fb
}
