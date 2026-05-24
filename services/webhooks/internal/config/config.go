// Package config loads webhooks-service configuration from env.
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

	// Kafka source — same audit topic the chain consumer reads. The
	// matcher reads with its own consumer group so it doesn't fight
	// with the audit writer.
	KafkaBrokers       []string
	KafkaAuditTopic    string
	KafkaConsumerGroup string

	// RabbitMQ for fan-out + delivery + DLQ. Topology pre-defined in
	// deploy/compose/rabbitmq/definitions.json (exchange guva.webhooks,
	// queue webhooks.delivery, DLX guva.webhooks.dlx, DLQ
	// webhooks.delivery.dead).
	AMQPURL                string
	AMQPDeliveryExchange   string
	AMQPDeliveryRoutingKey string
	AMQPDeliveryQueue      string

	// Delivery semantics
	MaxAttempts       int
	BackoffBase       time.Duration // first retry delay
	BackoffMultiplier float64
	DeliveryTimeout   time.Duration
}

func Load() (Config, error) {
	cfg := Config{
		ServiceName:            envOr("OTEL_SERVICE_NAME", "webhooks"),
		HTTPAddr:               envOr("WEBHOOKS_HTTP_ADDR", ":7074"),
		Environment:            envOr("OTEL_SERVICE_NAMESPACE", "local"),
		OTLPEndpoint:           envOr("OTEL_EXPORTER_OTLP_ENDPOINT", "http://localhost:4317"),
		DBHost:                 envOr("DB_HOST", "localhost"),
		DBPort:                 envOr("DB_PORT", "5432"),
		DBUser:                 envOr("DB_USER", "guva"),
		DBName:                 envOr("DB_NAME", "guva_webhooks"),
		DBSSLMode:              envOr("DB_SSLMODE", "disable"),
		KafkaBrokers:           splitCSV(envOr("KAFKA_BROKERS", "localhost:9094")),
		KafkaAuditTopic:        envOr("KAFKA_AUDIT_TOPIC", "ug.go.guva.audit.entry.appended.v1"),
		KafkaConsumerGroup:     envOr("KAFKA_WEBHOOKS_CONSUMER_GROUP", "guva-webhooks-matcher"),
		AMQPURL:                envOr("AMQP_URL", "amqp://guva:guva@localhost:5672/"),
		AMQPDeliveryExchange:   envOr("AMQP_DELIVERY_EXCHANGE", "guva.webhooks"),
		AMQPDeliveryRoutingKey: envOr("AMQP_DELIVERY_ROUTING_KEY", "deliver"),
		AMQPDeliveryQueue:      envOr("AMQP_DELIVERY_QUEUE", "webhooks.delivery"),
		MaxAttempts:            atoiOr(envOr("WEBHOOKS_MAX_ATTEMPTS", "7"), 7),
		BackoffBase:            parseDurOr(envOr("WEBHOOKS_BACKOFF_BASE", "30s"), 30*time.Second),
		BackoffMultiplier:      parseFloatOr(envOr("WEBHOOKS_BACKOFF_MULT", "4"), 4),
		DeliveryTimeout:        parseDurOr(envOr("WEBHOOKS_DELIVERY_TIMEOUT", "10s"), 10*time.Second),
	}
	level, err := parseLevel(envOr("WEBHOOKS_LOG_LEVEL", "info"))
	if err != nil {
		return Config{}, err
	}
	cfg.LogLevel = level
	if cfg.HTTPAddr == "" {
		return Config{}, errors.New("WEBHOOKS_HTTP_ADDR must not be empty")
	}
	return cfg, nil
}

func (c Config) DSN(password string) string {
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=%s",
		c.DBHost, c.DBPort, c.DBUser, password, c.DBName, c.DBSSLMode)
}

func envOr(key, fb string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
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
func atoiOr(s string, fb int) int {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err == nil && n > 0 {
		return n
	}
	return fb
}
func parseFloatOr(s string, fb float64) float64 {
	var n float64
	if _, err := fmt.Sscanf(s, "%f", &n); err == nil && n > 0 {
		return n
	}
	return fb
}
func parseDurOr(s string, fb time.Duration) time.Duration {
	if d, err := time.ParseDuration(s); err == nil {
		return d
	}
	return fb
}
