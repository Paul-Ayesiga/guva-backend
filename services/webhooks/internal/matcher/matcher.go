// Package matcher reads audit events off Kafka, looks up which
// subscriptions match each event's type, and publishes one delivery
// job per (event, subscription) pair to RabbitMQ for the delivery
// worker to consume.
//
// Why this split: the matcher is read-heavy (one Kafka message, many
// DB lookups, many AMQP publishes); the delivery worker is I/O-heavy
// (one HTTP call per job, may retry). Decoupling via RabbitMQ lets us
// scale each independently and gives RabbitMQ's retry/DLQ semantics
// to delivery without touching Kafka.
package matcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/guva-ug/guva-backend/services/webhooks/internal/store"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/segmentio/kafka-go"
)

// EventEnvelope is the wire shape on the audit Kafka topic. We trim it
// to the fields the matcher actually needs (id + type for matching).
type EventEnvelope struct {
	ID   string          `json:"id"`
	Type string          `json:"type"`
	Raw  json.RawMessage `json:"-"` // re-marshalled when publishing to Rabbit
}

// DeliveryJob is the AMQP payload the matcher publishes and the
// delivery worker consumes. Carries enough context to perform the
// outbound POST + write back the delivery row.
type DeliveryJob struct {
	DeliveryUUID   string          `json:"delivery_uuid"`
	SubscriptionID string          `json:"subscription_id"`
	TargetURL      string          `json:"target_url"`
	SecretHex      string          `json:"secret_hex"`
	EventUUID      string          `json:"event_uuid"`
	EventType      string          `json:"event_type"`
	Event          json.RawMessage `json:"event"` // the full audit envelope
	Attempt        int             `json:"attempt"`
}

type Config struct {
	Brokers    []string
	Topic      string
	GroupID    string
	AMQPURL    string
	Exchange   string
	RoutingKey string
}

type Matcher struct {
	cfg    Config
	logger *slog.Logger
	store  *store.Store
}

func New(cfg Config, logger *slog.Logger, st *store.Store) *Matcher {
	return &Matcher{cfg: cfg, logger: logger, store: st}
}

// Run blocks until ctx is done. It opens an AMQP channel, reads from
// Kafka, and publishes matched delivery jobs. Manual offset commits
// only after the publish succeeds, so a crash mid-publish replays.
func (m *Matcher) Run(ctx context.Context) error {
	conn, err := amqp.Dial(m.cfg.AMQPURL)
	if err != nil {
		return fmt.Errorf("amqp dial: %w", err)
	}
	defer conn.Close()
	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("amqp channel: %w", err)
	}
	defer ch.Close()

	reader := kafka.NewReader(kafka.ReaderConfig{
		Brokers:        m.cfg.Brokers,
		Topic:          m.cfg.Topic,
		GroupID:        m.cfg.GroupID,
		MinBytes:       1,
		MaxBytes:       10 << 20,
		MaxWait:        500 * time.Millisecond,
		CommitInterval: 0,
		StartOffset:    kafka.FirstOffset,
	})
	defer reader.Close()

	m.logger.Info("webhooks matcher starting",
		"topic", m.cfg.Topic, "group", m.cfg.GroupID, "exchange", m.cfg.Exchange)

	for {
		msg, err := reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
				return nil
			}
			m.logger.Error("kafka fetch failed", "error", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(2 * time.Second):
			}
			continue
		}

		var env EventEnvelope
		if err := json.Unmarshal(msg.Value, &env); err != nil {
			m.logger.Error("envelope unmarshal failed; skipping",
				"error", err, "partition", msg.Partition, "offset", msg.Offset)
			_ = reader.CommitMessages(ctx, msg) // skip poison
			continue
		}
		env.Raw = msg.Value

		if err := m.fanOut(ctx, ch, env); err != nil {
			m.logger.Error("matcher fan-out failed; will retry on next poll",
				"error", err, "event_uuid", env.ID)
			// don't commit; redeliver
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
			}
			continue
		}

		if err := reader.CommitMessages(ctx, msg); err != nil {
			m.logger.Error("commit offset failed", "error", err)
		}
	}
}

// fanOut writes one delivery row + one Rabbit message per matching sub.
// Delivery rows are inserted first so the worker can find them via
// delivery_uuid regardless of which order it processes things.
func (m *Matcher) fanOut(ctx context.Context, ch *amqp.Channel, env EventEnvelope) error {
	subs, err := m.store.MatchingSubscriptions(ctx, env.Type)
	if err != nil {
		return fmt.Errorf("match: %w", err)
	}
	if len(subs) == 0 {
		return nil
	}
	for _, sub := range subs {
		deliveryUUID, err := m.store.RecordDelivery(ctx, sub.ID, env.ID, env.Type)
		if err != nil {
			return fmt.Errorf("record delivery: %w", err)
		}
		job := DeliveryJob{
			DeliveryUUID:   deliveryUUID,
			SubscriptionID: sub.ID,
			TargetURL:      sub.TargetURL,
			SecretHex:      sub.Secret,
			EventUUID:      env.ID,
			EventType:      env.Type,
			Event:          env.Raw,
			Attempt:        0,
		}
		body, err := json.Marshal(job)
		if err != nil {
			return fmt.Errorf("marshal job: %w", err)
		}
		pubCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = ch.PublishWithContext(pubCtx, m.cfg.Exchange, m.cfg.RoutingKey, false, false,
			amqp.Publishing{
				ContentType:  "application/json",
				DeliveryMode: amqp.Persistent,
				MessageId:    deliveryUUID,
				Type:         env.Type,
				Body:         body,
			})
		cancel()
		if err != nil {
			return fmt.Errorf("amqp publish: %w", err)
		}
		m.logger.Debug("queued delivery",
			"delivery_uuid", deliveryUUID, "subscription_id", sub.ID,
			"target_url", sub.TargetURL, "event_type", env.Type)
	}
	return nil
}
