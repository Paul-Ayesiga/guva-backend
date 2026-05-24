// Package audit is the producer-side library every GUVA backend service
// uses to emit audit events to the platform audit chain.
//
// Design contract (matches guva-docs §12.4 transactional outbox):
//
//  1. Each service has its own `audit_outbox` table in its own database.
//     Apply OutboxMigration in your service's first migration so the
//     table comes online with the rest of the schema.
//
//  2. To emit an event, call audit.Emit(ctx, tx, event) inside an
//     existing transaction that's also doing your business write.
//     The outbox row commits or rolls back atomically with the
//     business change — no possibility of audit drift from reality.
//
//  3. Start an audit.Worker at process startup; it tails the outbox
//     table, publishes new rows to Kafka, and marks them sent. On
//     failure it leaves them unsent and retries on the next tick.
//
//  4. The audit service (services/audit) reads from Kafka and writes
//     the hash-chained ledger. Producers never know or care about the
//     chain — they only commit local intent.
//
// Failure modes:
//   - DB transaction rolls back → no outbox row, no event published. Correct.
//   - DB commit succeeds, Kafka down → row stays unsent. Worker retries.
//   - Kafka publish succeeds, sent_at update fails → next tick reads the
//     row again, publishes again. Audit consumer dedupes by event ID.
//   - Worker dies → on restart it picks up where it left off (unsent rows).
package audit

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/segmentio/kafka-go"
)

// OutboxMigration is the SQL every service includes in its migration
// suite to bring up the audit_outbox table. Identical across services
// so the drain worker can target it generically.
const OutboxMigration = `
CREATE TABLE IF NOT EXISTS audit_outbox (
    id          BIGSERIAL PRIMARY KEY,
    event_id    UUID UNIQUE NOT NULL,
    payload     JSONB NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    sent_at     TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_audit_outbox_unsent
    ON audit_outbox(id) WHERE sent_at IS NULL;
`

// Event is what a service emits. SourceKind/Source are the actor (the
// service or consumer doing the thing); SubjectKind/Subject describe
// the thing being acted on. Result is "ok" / "denied" / "error" / etc.
// Detail is service-defined; keep it small and stable.
type Event struct {
	SourceKind    string         // e.g. "service" or "consumer"
	Source        string         // e.g. "identity" or "guva-reference"
	Type          string         // e.g. "identity.consumer.created" — namespace prefix recommended
	SubjectKind   string         // e.g. "consumer" or "" if not subject-scoped
	Subject       string         // e.g. the keycloak client_id
	Result        string         // "ok" / "denied" / "error" — defaults to "ok"
	CorrelationID string         // optional; usually X-Correlation-Id from the HTTP request
	Data          map[string]any // small structured detail; will be JSON-marshalled
}

// Emit writes an audit-outbox row inside the caller's transaction. The
// event will be published to Kafka by the running Worker once tx commits.
//
// Returns the event ID assigned (a UUID) so callers can correlate. The
// time of emission is captured at Worker publish time, NOT here, because
// for the audit chain "when did this happen" is best reflected by when
// the platform observed it, not when the producer was on its way to
// observing it.
func Emit(ctx context.Context, tx pgx.Tx, e Event) (string, error) {
	if e.Type == "" {
		return "", fmt.Errorf("audit.Emit: event Type is required")
	}
	if e.Source == "" {
		return "", fmt.Errorf("audit.Emit: event Source is required")
	}
	if e.Result == "" {
		e.Result = "ok"
	}
	if e.SourceKind == "" {
		e.SourceKind = "service"
	}

	id := uuid.NewString()
	payload := envelope{
		SpecVersion:   "1.0",
		ID:            id,
		Source:        e.Source,
		SourceKind:    e.SourceKind,
		Type:          e.Type,
		Subject:       e.Subject,
		SubjectKind:   e.SubjectKind,
		Time:          time.Now().UTC(),
		CorrelationID: e.CorrelationID,
		Result:        e.Result,
	}
	if e.Data != nil {
		raw, err := json.Marshal(e.Data)
		if err != nil {
			return "", fmt.Errorf("marshal data: %w", err)
		}
		payload.Data = raw
	} else {
		payload.Data = json.RawMessage("null")
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal envelope: %w", err)
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO audit_outbox (event_id, payload) VALUES ($1, $2)`,
		id, body,
	)
	if err != nil {
		return "", fmt.Errorf("insert outbox: %w", err)
	}
	return id, nil
}

// envelope is the wire shape — must match what
// services/audit/internal/consumer.EventEnvelope decodes.
type envelope struct {
	SpecVersion   string          `json:"specversion"`
	ID            string          `json:"id"`
	Source        string          `json:"source"`
	SourceKind    string          `json:"sourcekind"`
	Type          string          `json:"type"`
	Subject       string          `json:"subject"`
	SubjectKind   string          `json:"subjectkind"`
	Time          time.Time       `json:"time"`
	CorrelationID string          `json:"correlationid"`
	Result        string          `json:"result"`
	Data          json.RawMessage `json:"data"`
}

// Worker tails the audit_outbox table and publishes rows to Kafka.
type Worker struct {
	logger    *slog.Logger
	writer    *kafka.Writer
	tickEvery time.Duration

	// db handle: a small interface so the caller can pass either a
	// *pgxpool.Pool or a *pgx.Conn (both satisfy Queryer).
	db Queryer
}

// Queryer is the minimal surface the Worker needs from a Postgres
// connection. *pgxpool.Pool satisfies this; tests can fake it.
type Queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// WorkerConfig captures the inputs to NewWorker.
type WorkerConfig struct {
	DB           Queryer
	Logger       *slog.Logger
	KafkaBrokers []string
	KafkaTopic   string // typically "ug.go.guva.audit.entry.appended.v1"
	TickEvery    time.Duration
}

// NewWorker constructs an unstarted Worker. Call Run to start.
func NewWorker(cfg WorkerConfig) *Worker {
	if cfg.TickEvery == 0 {
		cfg.TickEvery = 500 * time.Millisecond
	}
	w := &kafka.Writer{
		Addr:         kafka.TCP(cfg.KafkaBrokers...),
		Topic:        cfg.KafkaTopic,
		Balancer:     &kafka.Hash{}, // partition by message key
		RequiredAcks: kafka.RequireAll,
		Async:        false,
		BatchSize:    32,
		BatchTimeout: 100 * time.Millisecond,
	}
	return &Worker{
		logger:    cfg.Logger,
		writer:    w,
		tickEvery: cfg.TickEvery,
		db:        cfg.DB,
	}
}

// Run blocks until ctx is done. Drains the outbox on every tick.
func (w *Worker) Run(ctx context.Context) error {
	w.logger.Info("audit outbox worker starting",
		"topic", w.writer.Topic, "tick_every", w.tickEvery)
	t := time.NewTicker(w.tickEvery)
	defer t.Stop()
	defer func() { _ = w.writer.Close() }()
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("audit outbox worker stopping")
			return nil
		case <-t.C:
			if err := w.drainOnce(ctx); err != nil {
				w.logger.Error("audit drain tick failed", "error", err)
			}
		}
	}
}

// drainOnce reads up to 100 unsent rows and publishes them.
func (w *Worker) drainOnce(ctx context.Context) error {
	rows, err := w.db.Query(ctx,
		`SELECT id, event_id, payload
		   FROM audit_outbox
		  WHERE sent_at IS NULL
		  ORDER BY id ASC
		  LIMIT 100`)
	if err != nil {
		return fmt.Errorf("select outbox: %w", err)
	}

	type pending struct {
		id      int64
		eventID string
		payload []byte
	}
	var batch []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.eventID, &p.payload); err != nil {
			rows.Close()
			return fmt.Errorf("scan outbox: %w", err)
		}
		batch = append(batch, p)
	}
	rows.Close()
	if len(batch) == 0 {
		return nil
	}

	msgs := make([]kafka.Message, len(batch))
	for i, p := range batch {
		msgs[i] = kafka.Message{
			Key:   []byte(p.eventID), // hash partition key
			Value: p.payload,
		}
	}
	if err := w.writer.WriteMessages(ctx, msgs...); err != nil {
		return fmt.Errorf("kafka write: %w", err)
	}

	ids := make([]int64, len(batch))
	for i, p := range batch {
		ids[i] = p.id
	}
	if _, err := w.db.Exec(ctx,
		`UPDATE audit_outbox SET sent_at = NOW() WHERE id = ANY($1)`, ids,
	); err != nil {
		return fmt.Errorf("mark sent: %w", err)
	}
	w.logger.Debug("audit batch published", "count", len(batch))
	return nil
}
