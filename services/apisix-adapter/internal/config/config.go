// Package config loads apisix-adapter configuration from environment.
package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
)

type Config struct {
	ServiceName  string
	HTTPAddr     string
	LogLevel     slog.Level
	Environment  string
	OTLPEndpoint string

	DBHost    string
	DBPort    string
	DBUser    string
	DBName    string
	DBSSLMode string

	KafkaBrokers    []string
	KafkaAuditTopic string
	ApicurioURL     string

	// SharedSecret, if non-empty, must match the X-Adapter-Secret header
	// sent by APISIX's http-logger plugin. Off by default for local dev;
	// set in production to make sure only APISIX can post access logs.
	SharedSecret string
}

func Load() (Config, error) {
	cfg := Config{
		ServiceName:     envOr("OTEL_SERVICE_NAME", "apisix-adapter"),
		HTTPAddr:        envOr("APISIX_ADAPTER_HTTP_ADDR", ":7073"),
		Environment:     envOr("OTEL_SERVICE_NAMESPACE", "local"),
		OTLPEndpoint:    envOr("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4317"),
		DBHost:          envOr("DB_HOST", "localhost"),
		DBPort:          envOr("DB_PORT", "5432"),
		DBUser:          envOr("DB_USER", "guva"),
		DBName:          envOr("DB_NAME", "guva_apisix_adapter"),
		DBSSLMode:       envOr("DB_SSLMODE", "disable"),
		KafkaBrokers:    splitCSV(envOr("KAFKA_BROKERS", "localhost:9094")),
		KafkaAuditTopic: envOr("KAFKA_AUDIT_TOPIC", "ug.go.guva.audit.entry.appended.v1"),
		ApicurioURL:     envOr("APICURIO_URL", "http://localhost:8081"),
		SharedSecret:    os.Getenv("APISIX_ADAPTER_SHARED_SECRET"),
	}

	level, err := parseLevel(envOr("APISIX_ADAPTER_LOG_LEVEL", "info"))
	if err != nil {
		return Config{}, err
	}
	cfg.LogLevel = level

	if cfg.HTTPAddr == "" {
		return Config{}, errors.New("APISIX_ADAPTER_HTTP_ADDR must not be empty")
	}
	if len(cfg.KafkaBrokers) == 0 {
		return Config{}, errors.New("KAFKA_BROKERS must list at least one broker")
	}
	return cfg, nil
}

func (c Config) DSN(password string) string {
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		c.DBHost, c.DBPort, c.DBUser, password, c.DBName, c.DBSSLMode)
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
