// Package config loads audit-service configuration from environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"
)

// parseDurationOr returns the parsed duration or fallback on error.
func parseDurationOr(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}

type Config struct {
	ServiceName  string
	HTTPAddr     string
	LogLevel     slog.Level
	Environment  string
	OTLPEndpoint string

	// Postgres — two roles, one connection pool each. Reader is used by
	// the HTTP read handlers (/entries, /verify); writer is used by the
	// Kafka chain consumer, the meta-audit emission, and the outbox
	// drain Worker. See docs/OPERATIONS.md for the defense-in-depth
	// rationale.
	DBHost       string
	DBPort       string
	DBName       string
	DBSSLMode    string
	DBUserReader string // default: "guva_audit_reader"
	DBUserWriter string // default: "guva_audit_writer"

	// Vault (resolved DB passwords at startup)
	VaultAddr  string
	VaultToken string

	// Kafka
	KafkaBrokers       []string
	KafkaAuditTopic    string
	KafkaConsumerGroup string
	ApicurioURL        string

	// Anchoring. AnchorInterval controls how often a new Merkle anchor
	// is computed; AnchorWitnessURL, if set, receives every new
	// anchor as a POST (best-effort, see internal/anchor).
	AnchorInterval   time.Duration
	AnchorWitnessURL string
}

func Load() (Config, error) {
	cfg := Config{
		ServiceName:        envOr("OTEL_SERVICE_NAME", "audit"),
		HTTPAddr:           envOr("AUDIT_HTTP_ADDR", ":7072"),
		Environment:        envOr("OTEL_SERVICE_NAMESPACE", "local"),
		OTLPEndpoint:       envOr("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4317"),
		DBHost:             envOr("DB_HOST", "localhost"),
		DBPort:             envOr("DB_PORT", "5432"),
		DBName:             envOr("DB_NAME", "guva_audit"),
		DBSSLMode:          envOr("DB_SSLMODE", "disable"),
		DBUserReader:       envOr("DB_USER_READER", "guva_audit_reader"),
		DBUserWriter:       envOr("DB_USER_WRITER", "guva_audit_writer"),
		VaultAddr:          envOr("VAULT_ADDR", "http://localhost:8200"),
		VaultToken:         envOr("VAULT_TOKEN", "dev-root-token"),
		KafkaBrokers:       splitCSV(envOr("KAFKA_BROKERS", "localhost:9094")),
		KafkaAuditTopic:    envOr("KAFKA_AUDIT_TOPIC", "ug.go.guva.audit.entry.appended.v1"),
		KafkaConsumerGroup: envOr("KAFKA_AUDIT_CONSUMER_GROUP", "guva-audit-writer"),
		ApicurioURL:        envOr("APICURIO_URL", "http://localhost:8081"),
		AnchorInterval:     parseDurationOr(envOr("AUDIT_ANCHOR_INTERVAL", "60s"), 60*time.Second),
		AnchorWitnessURL:   os.Getenv("AUDIT_ANCHOR_WITNESS_URL"),
	}

	level, err := parseLevel(envOr("AUDIT_LOG_LEVEL", "info"))
	if err != nil {
		return Config{}, err
	}
	cfg.LogLevel = level

	if cfg.HTTPAddr == "" {
		return Config{}, errors.New("AUDIT_HTTP_ADDR must not be empty")
	}
	if len(cfg.KafkaBrokers) == 0 {
		return Config{}, errors.New("KAFKA_BROKERS must list at least one broker")
	}
	return cfg, nil
}

// DSNFor returns a libpq-style DSN for the given role + password. Used
// by main.go to build one pool per role.
func (c Config) DSNFor(user, password string) string {
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		c.DBHost, c.DBPort, user, password, c.DBName, c.DBSSLMode)
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
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
