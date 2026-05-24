// Package consumer wraps the Kafka subscriber that drives the audit
// chain. Manual offset commit: we commit AFTER the DB write succeeds,
// so a crash mid-write replays the message on restart (the chain
// dedupes by entry_uuid).
package consumer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/guva-ug/guva-backend/services/audit/internal/store"

	"github.com/segmentio/kafka-go"
)

// Config bundles the Kafka inputs.
type Config struct {
	Brokers []string
	Topic   string
	GroupID string
}

// EventEnvelope is the on-the-wire shape producers publish. Mirrors the
// CloudEvents envelope from guva-docs/03-architecture/12-event-driven-
// messaging.md §12.3 but trimmed to the fields the audit consumer
// actually uses.
type EventEnvelope struct {
	SpecVersion   string          `json:"specversion"`
	ID            string          `json:"id"`            // becomes entry_uuid
	Source        string          `json:"source"`        // becomes actor_id
	SourceKind    string          `json:"sourcekind"`    // becomes actor_kind
	Type          string          `json:"type"`          // becomes action
	Subject       string          `json:"subject"`       // becomes subject_id (kind from subject_kind)
	SubjectKind   string          `json:"subjectkind"`   // becomes subject_kind
	Time          time.Time       `json:"time"`          // becomes occurred_at
	CorrelationID string          `json:"correlationid"` // optional
	Result        string          `json:"result"`        // ok/denied/error/...
	Data          json.RawMessage `json:"data"`          // becomes detail
}

// Consumer runs the read loop.
type Consumer struct {
	logger *slog.Logger
	store  *store.Store
	reader *kafka.Reader
}

func New(cfg Config, logger *slog.Logger, st *store.Store) *Consumer {
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers:               cfg.Brokers,
		Topic:                 cfg.Topic,
		GroupID:               cfg.GroupID,
		MinBytes:              1,
		MaxBytes:              10 << 20,
		MaxWait:               500 * time.Millisecond,
		ReadBackoffMin:        100 * time.Millisecond,
		ReadBackoffMax:        2 * time.Second,
		WatchPartitionChanges: true,
		// For a NEW consumer group with no committed offset, start at
		// the beginning. An audit consumer that missed events is the
		// worst-of-both: silent gaps in the ledger. Once a committed
		// offset exists this setting is ignored.
		StartOffset: kafka.FirstOffset,
		// Manual commit only after the DB write succeeds.
		CommitInterval: 0,
	})
	return &Consumer{logger: logger, store: st, reader: r}
}

// Run blocks until ctx is done. Errors during message processing are
// logged and the message is NOT committed — Kafka redelivers on next
// poll. Errors during Close are returned.
func (c *Consumer) Run(ctx context.Context) error {
	c.logger.Info("audit kafka consumer starting",
		"topic", c.reader.Config().Topic,
		"group", c.reader.Config().GroupID,
		"brokers", c.reader.Config().Brokers)

	for {
		m, err := c.reader.FetchMessage(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, io.EOF) {
				c.logger.Info("audit kafka consumer stopping")
				return c.reader.Close()
			}
			c.logger.Error("kafka fetch failed; backing off", "error", err)
			select {
			case <-ctx.Done():
				return c.reader.Close()
			case <-time.After(2 * time.Second):
				continue
			}
		}

		if err := c.handle(ctx, m); err != nil {
			c.logger.Error("audit event processing failed; will retry on next poll",
				"partition", m.Partition, "offset", m.Offset, "error", err)
			// Don't commit. Kafka will redeliver. Short sleep to avoid
			// hot-looping on a poison message; a real DLQ comes later.
			select {
			case <-ctx.Done():
				return c.reader.Close()
			case <-time.After(time.Second):
				continue
			}
		}

		if err := c.reader.CommitMessages(ctx, m); err != nil {
			c.logger.Error("commit offset failed; message will replay",
				"partition", m.Partition, "offset", m.Offset, "error", err)
		}
	}
}

func (c *Consumer) handle(ctx context.Context, m kafka.Message) error {
	var env EventEnvelope
	if err := json.Unmarshal(m.Value, &env); err != nil {
		return fmt.Errorf("unmarshal envelope: %w", err)
	}
	if env.ID == "" || env.Type == "" || env.Source == "" {
		return fmt.Errorf("envelope missing required fields (id/type/source)")
	}

	e := store.Entry{
		EntryUUID:     env.ID,
		OccurredAt:    env.Time,
		ActorKind:     stringOr(env.SourceKind, "service"),
		ActorID:       env.Source,
		SubjectKind:   env.SubjectKind,
		SubjectID:     env.Subject,
		Action:        env.Type,
		Result:        stringOr(env.Result, "ok"),
		CorrelationID: env.CorrelationID,
		Detail:        env.Data,
	}

	existed, err := c.store.AppendEntry(ctx, e)
	if err != nil {
		return err
	}
	if existed {
		c.logger.Debug("audit event already in chain; skipped",
			"entry_uuid", e.EntryUUID, "partition", m.Partition, "offset", m.Offset)
		return nil
	}
	c.logger.Debug("audit event appended",
		"entry_uuid", e.EntryUUID, "action", e.Action,
		"partition", m.Partition, "offset", m.Offset)
	return nil
}

func stringOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
