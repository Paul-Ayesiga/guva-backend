// Package config loads identity-service configuration from environment
// variables. Service-specific values (HTTP addr, DB DSN) come from env;
// rotating values (DB password, Keycloak admin password) come from Vault
// at startup — see cmd/server/main.go.
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
	Environment string

	OTLPEndpoint string

	VaultAddr  string
	VaultToken string

	// DBConn is the libpq-style DSN minus the password; password is
	// fetched from Vault and joined in at startup.
	DBHost    string
	DBPort    string
	DBUser    string
	DBName    string
	DBSSLMode string

	// KeycloakBackendURL is the URL identity uses to reach Keycloak's
	// admin API. Distinct from the FRONTEND URL (the iss claim, which
	// is https://auth.guva.localhost): identity never goes through
	// Caddy because it would need to trust Caddy's local CA. It hits
	// Keycloak by its docker-network name (or by localhost when run
	// on the host).
	KeycloakBackendURL string
	KeycloakRealm      string

	// Audit emission
	KafkaBrokers    []string
	KafkaAuditTopic string
	ApicurioURL     string
}

func Load() (Config, error) {
	cfg := Config{
		ServiceName:        envOr("OTEL_SERVICE_NAME", "identity"),
		HTTPAddr:           envOr("IDENTITY_HTTP_ADDR", ":7071"),
		Environment:        envOr("OTEL_SERVICE_NAMESPACE", "local"),
		OTLPEndpoint:       envOr("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4317"),
		VaultAddr:          envOr("VAULT_ADDR", "http://localhost:8200"),
		VaultToken:         envOr("VAULT_TOKEN", "dev-root-token"),
		DBHost:             envOr("DB_HOST", "localhost"),
		DBPort:             envOr("DB_PORT", "5432"),
		DBUser:             envOr("DB_USER", "guva"),
		DBName:             envOr("DB_NAME", "guva_identity"),
		DBSSLMode:          envOr("DB_SSLMODE", "disable"),
		KeycloakBackendURL: envOr("KEYCLOAK_BACKEND_URL", "http://localhost:8080"),
		KeycloakRealm:      envOr("KEYCLOAK_REALM", "guva"),
		KafkaBrokers:       splitCSV(envOr("KAFKA_BROKERS", "localhost:9094")),
		KafkaAuditTopic:    envOr("KAFKA_AUDIT_TOPIC", "ug.go.guva.audit.entry.appended.v1"),
		ApicurioURL:        envOr("APICURIO_URL", "http://localhost:8081"),
	}

	level, err := parseLevel(envOr("IDENTITY_LOG_LEVEL", "info"))
	if err != nil {
		return Config{}, err
	}
	cfg.LogLevel = level

	if cfg.HTTPAddr == "" {
		return Config{}, errors.New("IDENTITY_HTTP_ADDR must not be empty")
	}
	return cfg, nil
}

// DSN returns the libpq-style DSN with the given password joined in.
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
