// Package config loads NIRA-integration-service configuration from env.
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
	VaultAddr, VaultToken                     string

	KafkaBrokers    []string
	KafkaAuditTopic string
	ApicurioURL     string

	// Backend: "simulator" (default, in-memory canned data) or
	// "upstream" (production HTTP client against the real NIRA API).
	Backend string

	// UPSTREAM backend settings — only honoured when Backend=upstream.
	// All paths are placeholders until NIRA hands over the real
	// agreement; values are wired so the day the agreement lands,
	// the only changes are env vars + cert material.
	UpstreamBaseURL   string
	UpstreamCertFile  string // mTLS client cert (PEM)
	UpstreamKeyFile   string // mTLS client key  (PEM)
	UpstreamCAFile    string // CA we trust for the upstream's server cert
	UpstreamTimeout   time.Duration
	UpstreamRetries   int           // total attempts including the first
	UpstreamBackoff   time.Duration // base delay; doubles per attempt
	CircuitThreshold  int           // consecutive failures before tripping
	CircuitOpenWindow time.Duration // how long the breaker stays open before trying half-open
}

func Load() (Config, error) {
	cfg := Config{
		ServiceName:       envOr("OTEL_SERVICE_NAME", "integrations-nira"),
		HTTPAddr:          envOr("NIRA_INT_HTTP_ADDR", ":7080"),
		Environment:       envOr("OTEL_SERVICE_NAMESPACE", "local"),
		OTLPEndpoint:      envOr("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4317"),
		DBHost:            envOr("DB_HOST", "localhost"),
		DBPort:            envOr("DB_PORT", "5432"),
		DBUser:            envOr("DB_USER", "guva"),
		DBName:            envOr("DB_NAME", "guva_integrations_nira"),
		DBSSLMode:         envOr("DB_SSLMODE", "disable"),
		VaultAddr:         envOr("VAULT_ADDR", "http://localhost:8200"),
		VaultToken:        envOr("VAULT_TOKEN", "dev-root-token"),
		KafkaBrokers:      splitCSV(envOr("KAFKA_BROKERS", "localhost:9094")),
		KafkaAuditTopic:   envOr("KAFKA_AUDIT_TOPIC", "ug.go.guva.audit.entry.appended.v1"),
		ApicurioURL:       envOr("APICURIO_URL", "http://localhost:8081"),
		Backend:           envOr("NIRA_BACKEND", "simulator"),
		UpstreamBaseURL:   os.Getenv("NIRA_UPSTREAM_URL"),
		UpstreamCertFile:  os.Getenv("NIRA_UPSTREAM_CERT"),
		UpstreamKeyFile:   os.Getenv("NIRA_UPSTREAM_KEY"),
		UpstreamCAFile:    os.Getenv("NIRA_UPSTREAM_CA"),
		UpstreamTimeout:   parseDurOr(envOr("NIRA_UPSTREAM_TIMEOUT", "5s"), 5*time.Second),
		UpstreamRetries:   atoiOr(envOr("NIRA_UPSTREAM_RETRIES", "3"), 3),
		UpstreamBackoff:   parseDurOr(envOr("NIRA_UPSTREAM_BACKOFF", "200ms"), 200*time.Millisecond),
		CircuitThreshold:  atoiOr(envOr("NIRA_CIRCUIT_THRESHOLD", "5"), 5),
		CircuitOpenWindow: parseDurOr(envOr("NIRA_CIRCUIT_OPEN_WINDOW", "30s"), 30*time.Second),
	}
	level, err := parseLevel(envOr("NIRA_INT_LOG_LEVEL", "info"))
	if err != nil {
		return Config{}, err
	}
	cfg.LogLevel = level
	if cfg.HTTPAddr == "" {
		return Config{}, errors.New("NIRA_INT_HTTP_ADDR must not be empty")
	}
	if cfg.Backend == "upstream" {
		if cfg.UpstreamBaseURL == "" {
			return Config{}, errors.New("NIRA_BACKEND=upstream requires NIRA_UPSTREAM_URL")
		}
		if cfg.UpstreamCertFile == "" || cfg.UpstreamKeyFile == "" || cfg.UpstreamCAFile == "" {
			return Config{}, errors.New("NIRA_BACKEND=upstream requires NIRA_UPSTREAM_CERT, _KEY and _CA — agencies mandate mTLS")
		}
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
func atoiOr(s string, fb int) int {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err == nil && n > 0 {
		return n
	}
	return fb
}
