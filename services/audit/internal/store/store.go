// Package store provides the Postgres-backed write/read path for the
// audit_entries table.
//
// The chain is single-writer: AppendEntry is the only writer, takes
// FOR UPDATE on the latest row to serialise concurrent appenders (even
// though in practice we run one consumer per partition).
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/guva-ug/guva-backend/services/audit/internal/chain"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store wraps two pgx pools, one per Postgres role:
//
//   - reader is used by all SELECT-only paths (HTTP /entries, /verify).
//     The role lacks INSERT/UPDATE/DELETE on the chain, so a bug in
//     the read path cannot mutate the ledger.
//   - writer is used by AppendEntry (chain INSERT) and by the meta-audit
//     emitter that stages rows in audit_outbox.
//
// See docs/OPERATIONS.md §8 for the defense-in-depth rationale.
type Store struct {
	reader *pgxpool.Pool
	writer *pgxpool.Pool
}

// Open dials both roles. Both DSNs must point at the same database; the
// only difference is the role (user) the connection authenticates as.
// readerDSN and writerDSN may be identical for single-role local dev,
// in which case the same pool is used for both — but the call sites
// continue to think of "Reader" and "Writer" as distinct, so flipping
// the roles back on later is a one-line config change.
func Open(ctx context.Context, readerDSN, writerDSN string) (*Store, error) {
	rp, err := openPool(ctx, readerDSN, "reader")
	if err != nil {
		return nil, err
	}
	wp, err := openPool(ctx, writerDSN, "writer")
	if err != nil {
		rp.Close()
		return nil, err
	}
	return &Store{reader: rp, writer: wp}, nil
}

func openPool(ctx context.Context, dsn, label string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse %s dsn: %w", label, err)
	}
	cfg.MaxConns = 8
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("%s pool init: %w", label, err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("%s ping: %w", label, err)
	}
	return pool, nil
}

func (s *Store) Close() {
	if s.writer != nil {
		s.writer.Close()
	}
	if s.reader != nil {
		s.reader.Close()
	}
}

// Reader returns the SELECT-only pool. Use for query paths that have no
// reason to mutate state.
func (s *Store) Reader() *pgxpool.Pool { return s.reader }

// Writer returns the INSERT/UPDATE-capable pool. Used by AppendEntry and
// by the meta-audit emitter (which stages rows in audit_outbox).
func (s *Store) Writer() *pgxpool.Pool { return s.writer }

// Entry is the wire shape consumed from Kafka. We don't expose the
// pgx row type to keep store boundaries clean.
type Entry struct {
	EntryUUID     string
	OccurredAt    time.Time
	ActorKind     string
	ActorID       string
	SubjectKind   string
	SubjectID     string
	Action        string
	Result        string
	CorrelationID string
	Detail        json.RawMessage
}

// chainAppendLockID is the transactional advisory-lock key all writers
// hold while computing prev_hash → INSERT. Picked arbitrarily; only
// meaningful in that every GUVA audit chain writer (this service today,
// any sharded variant tomorrow) uses the same value.
const chainAppendLockID = 8732

// AppendEntry inserts the event onto the chain. Idempotent on entry_uuid:
// a duplicate Kafka delivery resolves to (existedAlready=true, err=nil)
// without extending the chain.
//
// Ordering: the function acquires a transactional advisory lock keyed
// by chainAppendLockID before reading the previous hash, holds it
// through the INSERT, and releases on COMMIT/ROLLBACK. This serialises
// concurrent appenders without needing UPDATE privilege on the table
// (the writer role intentionally lacks UPDATE — see
// docs/OPERATIONS.md §8). Earlier versions of this code used
// SELECT ... FOR UPDATE which silently required UPDATE; switching to
// pg_advisory_xact_lock keeps the serialisation property and respects
// the role boundary.
func (s *Store) AppendEntry(ctx context.Context, e Entry) (existedAlready bool, err error) {
	tx, err := s.writer.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op if commit ran

	// Serialise chain appends. Released automatically at COMMIT/ROLLBACK.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, chainAppendLockID); err != nil {
		return false, fmt.Errorf("acquire chain lock: %w", err)
	}

	// Dedupe by entry_uuid. The unique constraint would catch this on
	// INSERT, but checking up front avoids spurious chain extensions
	// that we'd then have to roll back.
	var existing int
	if err := tx.QueryRow(ctx,
		`SELECT 1 FROM audit_entries WHERE entry_uuid = $1`, e.EntryUUID,
	).Scan(&existing); err == nil {
		return true, tx.Commit(ctx)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("dedupe lookup: %w", err)
	}

	// Lookup the latest entry_hash to use as previous_hash. The
	// advisory lock above guarantees we won't race with another
	// appender. Empty-chain case falls through with GenesisHash.
	var prev string = chain.GenesisHash
	row := tx.QueryRow(ctx,
		`SELECT entry_hash
		   FROM audit_entries
		  ORDER BY entry_id DESC
		  LIMIT 1`)
	if err := row.Scan(&prev); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("read prev hash: %w", err)
	}

	occurred := e.OccurredAt
	if occurred.IsZero() {
		occurred = time.Now().UTC()
	}
	input := chain.Input{
		EntryUUID:     e.EntryUUID,
		OccurredAt:    chain.NormaliseTime(occurred),
		ActorKind:     e.ActorKind,
		ActorID:       e.ActorID,
		SubjectKind:   e.SubjectKind,
		SubjectID:     e.SubjectID,
		Action:        e.Action,
		Result:        e.Result,
		CorrelationID: e.CorrelationID,
		Detail:        e.Detail,
		PreviousHash:  prev,
	}
	newHash, err := chain.Compute(input)
	if err != nil {
		return false, fmt.Errorf("compute hash: %w", err)
	}

	_, err = tx.Exec(ctx,
		`INSERT INTO audit_entries
		    (entry_uuid, occurred_at, actor_kind, actor_id,
		     subject_kind, subject_id, action, result,
		     correlation_id, detail, previous_hash, entry_hash)
		 VALUES ($1, $2, $3, $4,
		         NULLIF($5, ''), NULLIF($6, ''), $7, $8,
		         NULLIF($9, '')::uuid, $10, $11, $12)`,
		e.EntryUUID, occurred,
		e.ActorKind, e.ActorID,
		e.SubjectKind, e.SubjectID,
		e.Action, e.Result,
		e.CorrelationID, e.Detail, prev, newHash,
	)
	if err != nil {
		return false, fmt.Errorf("insert: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit: %w", err)
	}
	return false, nil
}

// QueryParams filters list queries. Zero-valued fields are ignored.
type QueryParams struct {
	ActorID   string
	SubjectID string
	Action    string
	Result    string
	From      time.Time
	To        time.Time
	AfterID   int64 // cursor; rows with entry_id > AfterID
	Limit     int
}

// EntryRecord is the read-side representation including the chain fields
// for verification.
type EntryRecord struct {
	EntryID       int64           `json:"entry_id"`
	EntryUUID     string          `json:"entry_uuid"`
	OccurredAt    time.Time       `json:"occurred_at"`
	ActorKind     string          `json:"actor_kind"`
	ActorID       string          `json:"actor_id"`
	SubjectKind   string          `json:"subject_kind,omitempty"`
	SubjectID     string          `json:"subject_id,omitempty"`
	Action        string          `json:"action"`
	Result        string          `json:"result"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	Detail        json.RawMessage `json:"detail,omitempty"`
	PreviousHash  string          `json:"previous_hash"`
	EntryHash     string          `json:"entry_hash"`
}

// List runs a filtered, cursor-paginated query. Returns rows in
// ascending entry_id order so the next cursor is the last row's id.
func (s *Store) List(ctx context.Context, p QueryParams) ([]EntryRecord, error) {
	if p.Limit <= 0 || p.Limit > 500 {
		p.Limit = 100
	}
	args := []any{p.AfterID, p.Limit}
	sb := `
		SELECT entry_id, entry_uuid, occurred_at, actor_kind, actor_id,
		       COALESCE(subject_kind, ''), COALESCE(subject_id, ''),
		       action, result,
		       COALESCE(correlation_id::text, ''),
		       COALESCE(detail::text, 'null'),
		       previous_hash, entry_hash
		  FROM audit_entries
		 WHERE entry_id > $1`
	if p.ActorID != "" {
		args = append(args, p.ActorID)
		sb += fmt.Sprintf(" AND actor_id = $%d", len(args))
	}
	if p.SubjectID != "" {
		args = append(args, p.SubjectID)
		sb += fmt.Sprintf(" AND subject_id = $%d", len(args))
	}
	if p.Action != "" {
		args = append(args, p.Action)
		sb += fmt.Sprintf(" AND action = $%d", len(args))
	}
	if p.Result != "" {
		args = append(args, p.Result)
		sb += fmt.Sprintf(" AND result = $%d", len(args))
	}
	if !p.From.IsZero() {
		args = append(args, p.From)
		sb += fmt.Sprintf(" AND occurred_at >= $%d", len(args))
	}
	if !p.To.IsZero() {
		args = append(args, p.To)
		sb += fmt.Sprintf(" AND occurred_at < $%d", len(args))
	}
	sb += ` ORDER BY entry_id ASC LIMIT $2`

	rows, err := s.reader.Query(ctx, sb, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	out := make([]EntryRecord, 0, p.Limit)
	for rows.Next() {
		var r EntryRecord
		var detailStr string
		if err := rows.Scan(
			&r.EntryID, &r.EntryUUID, &r.OccurredAt,
			&r.ActorKind, &r.ActorID,
			&r.SubjectKind, &r.SubjectID,
			&r.Action, &r.Result,
			&r.CorrelationID, &detailStr,
			&r.PreviousHash, &r.EntryHash,
		); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		r.Detail = json.RawMessage(detailStr)
		out = append(out, r)
	}
	return out, rows.Err()
}

// VerifyRange walks entries with entry_id between fromID and toID
// (inclusive at both ends), recomputes each hash, and confirms it
// matches the stored entry_hash and that previous_hash chains correctly.
// Returns the offending entry on the first mismatch or nil if intact.
func (s *Store) VerifyRange(ctx context.Context, fromID, toID int64) (*EntryRecord, error) {
	if fromID <= 0 {
		fromID = 1
	}
	rows, err := s.reader.Query(ctx,
		`SELECT entry_id, entry_uuid, occurred_at, actor_kind, actor_id,
		        COALESCE(subject_kind, ''), COALESCE(subject_id, ''),
		        action, result,
		        COALESCE(correlation_id::text, ''),
		        COALESCE(detail::text, 'null'),
		        previous_hash, entry_hash
		   FROM audit_entries
		  WHERE entry_id BETWEEN $1 AND $2
		  ORDER BY entry_id ASC`, fromID, toID)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	// We need each row's expected previous_hash to be the prior row's
	// entry_hash. For the very first entry (entry_id = 1), expected
	// previous_hash is the genesis.
	expectedPrev := chain.GenesisHash
	first := fromID == 1
	if !first {
		// We need to fetch the hash of the row just before fromID.
		err := s.reader.QueryRow(ctx,
			`SELECT entry_hash FROM audit_entries WHERE entry_id = $1`, fromID-1,
		).Scan(&expectedPrev)
		if err != nil {
			return nil, fmt.Errorf("anchor lookup: %w", err)
		}
	}

	for rows.Next() {
		var r EntryRecord
		var detailStr string
		if err := rows.Scan(
			&r.EntryID, &r.EntryUUID, &r.OccurredAt,
			&r.ActorKind, &r.ActorID,
			&r.SubjectKind, &r.SubjectID,
			&r.Action, &r.Result,
			&r.CorrelationID, &detailStr,
			&r.PreviousHash, &r.EntryHash,
		); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		r.Detail = json.RawMessage(detailStr)

		if r.PreviousHash != expectedPrev {
			return &r, nil
		}
		input := chain.Input{
			EntryUUID:     r.EntryUUID,
			OccurredAt:    chain.NormaliseTime(r.OccurredAt),
			ActorKind:     r.ActorKind,
			ActorID:       r.ActorID,
			SubjectKind:   r.SubjectKind,
			SubjectID:     r.SubjectID,
			Action:        r.Action,
			Result:        r.Result,
			CorrelationID: r.CorrelationID,
			Detail:        r.Detail,
			PreviousHash:  r.PreviousHash,
		}
		got, err := chain.Compute(input)
		if err != nil {
			return nil, fmt.Errorf("recompute: %w", err)
		}
		if got != r.EntryHash {
			return &r, nil
		}
		expectedPrev = r.EntryHash
	}
	return nil, rows.Err()
}
