// Webhooks service — subscribes consumers to audit events and POSTs
// HMAC-signed deliveries to their configured URLs.
//
// Goroutines:
//
//  1. HTTP server — subscription CRUD + delivery list
//  2. Matcher — Kafka consumer → match subs → publish to RabbitMQ
//  3. Delivery worker — RabbitMQ consumer → POST → retry/DLQ
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/guva-ug/guva-backend/pkg/platform/health"
	"github.com/guva-ug/guva-backend/pkg/platform/httpserver"
	"github.com/guva-ug/guva-backend/pkg/platform/observability"
	"github.com/guva-ug/guva-backend/services/webhooks/internal/config"
	"github.com/guva-ug/guva-backend/services/webhooks/internal/delivery"
	"github.com/guva-ug/guva-backend/services/webhooks/internal/matcher"
	"github.com/guva-ug/guva-backend/services/webhooks/internal/server"
	"github.com/guva-ug/guva-backend/services/webhooks/internal/store"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config load failed", "error", err)
		os.Exit(1)
	}
	logger := observability.NewLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	shutdownTracing, err := observability.InitTracing(ctx, observability.TracingConfig{
		ServiceName:  cfg.ServiceName,
		Namespace:    "guva",
		Environment:  cfg.Environment,
		OTLPEndpoint: cfg.OTLPEndpoint,
	})
	if err != nil {
		logger.Warn("tracing init failed; continuing without traces", "error", err)
	}
	defer func() {
		c, cn := context.WithTimeout(context.Background(), 5*time.Second)
		defer cn()
		_ = shutdownTracing(c)
	}()

	dbPassword := envOr("POSTGRES_PASSWORD", "guva")
	dbCtx, dbCancel := context.WithTimeout(ctx, 15*time.Second)
	st, err := store.Open(dbCtx, cfg.DSN(dbPassword))
	dbCancel()
	if err != nil {
		logger.Error("db connect failed", "error", err, "host", cfg.DBHost, "db", cfg.DBName)
		os.Exit(1)
	}
	defer st.Close()
	logger.Info("db connected", "host", cfg.DBHost, "db", cfg.DBName)

	probes := health.New()
	probes.MarkReady()
	srv := server.New(cfg, logger, probes, st)

	m := matcher.New(matcher.Config{
		Brokers:    cfg.KafkaBrokers,
		Topic:      cfg.KafkaAuditTopic,
		GroupID:    cfg.KafkaConsumerGroup,
		AMQPURL:    cfg.AMQPURL,
		Exchange:   cfg.AMQPDeliveryExchange,
		RoutingKey: cfg.AMQPDeliveryRoutingKey,
	}, logger, st)

	w := delivery.New(delivery.Config{
		AMQPURL:           cfg.AMQPURL,
		Exchange:          cfg.AMQPDeliveryExchange,
		RoutingKey:        cfg.AMQPDeliveryRoutingKey,
		Queue:             cfg.AMQPDeliveryQueue,
		MaxAttempts:       cfg.MaxAttempts,
		BackoffBase:       cfg.BackoffBase,
		BackoffMultiplier: cfg.BackoffMultiplier,
		DeliveryTimeout:   cfg.DeliveryTimeout,
	}, logger, st)

	var wg sync.WaitGroup
	wg.Add(3)

	go func() {
		defer wg.Done()
		logger.Info("webhooks service listening", "addr", cfg.HTTPAddr)
		if err := httpserver.ListenAndServeAny(srv); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server failed", "error", err)
			cancel()
		}
	}()

	go func() {
		defer wg.Done()
		if err := m.Run(ctx); err != nil {
			logger.Error("matcher exited with error", "error", err)
		}
	}()

	go func() {
		defer wg.Done()
		if err := w.Run(ctx); err != nil {
			logger.Error("delivery worker exited with error", "error", err)
		}
	}()

	<-ctx.Done()
	logger.Info("shutdown signal received")
	probes.MarkNotReady()
	c, cn := context.WithTimeout(context.Background(), 10*time.Second)
	defer cn()
	if err := srv.Shutdown(c); err != nil {
		logger.Error("graceful shutdown failed", "error", err)
	}
	wg.Wait()
	logger.Info("bye")
}

func envOr(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
