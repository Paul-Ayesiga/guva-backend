// Package store is the verification-service Postgres persistence —
// operational log + idempotency cache.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	cfg.MaxConns = 10
	cfg.ConnConfig.ConnectTimeout = 5 * time.Second
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pool init: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close()              { s.pool.Close() }
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// LogEntry is one row in verification_log. Subject is hashed (PII
// never lands here in plain text).
type LogEntry struct {
	VerificationID      string
	ConsumerID          string
	SubjectType         string
	SubjectHash         string
	ConsentReference    string
	Upstream            string
	Status              string
	RequestedAttributes []string
	MatchCount          int
	MismatchCount       int
	UpstreamLatencyMS   int
	CorrelationID       string
}

// Log inserts one verification_log row. Returns the verification_id
// (the row generates one if VerificationID is empty).
func (s *Store) Log(ctx context.Context, e LogEntry) (string, error) {
	if e.VerificationID == "" {
		// Let the DB generate via DEFAULT.
		return s.logInsert(ctx, e, false)
	}
	return s.logInsert(ctx, e, true)
}

func (s *Store) logInsert(ctx context.Context, e LogEntry, withID bool) (string, error) {
	var id string
	var query string
	var args []any
	if withID {
		query = `
		INSERT INTO verification_log (
		    verification_id, consumer_id, subject_type, subject_hash,
		    consent_reference, upstream, status, requested_attributes,
		    match_count, mismatch_count, upstream_latency_ms, correlation_id)
		VALUES ($1,$2,$3,$4, NULLIF($5,''),$6,$7,$8,$9,$10,$11, NULLIF($12,'')::uuid)
		RETURNING verification_id`
		args = []any{e.VerificationID, e.ConsumerID, e.SubjectType, e.SubjectHash,
			e.ConsentReference, e.Upstream, e.Status, e.RequestedAttributes,
			e.MatchCount, e.MismatchCount, e.UpstreamLatencyMS, e.CorrelationID}
	} else {
		query = `
		INSERT INTO verification_log (
		    consumer_id, subject_type, subject_hash,
		    consent_reference, upstream, status, requested_attributes,
		    match_count, mismatch_count, upstream_latency_ms, correlation_id)
		VALUES ($1,$2,$3, NULLIF($4,''),$5,$6,$7,$8,$9,$10, NULLIF($11,'')::uuid)
		RETURNING verification_id`
		args = []any{e.ConsumerID, e.SubjectType, e.SubjectHash,
			e.ConsentReference, e.Upstream, e.Status, e.RequestedAttributes,
			e.MatchCount, e.MismatchCount, e.UpstreamLatencyMS, e.CorrelationID}
	}
	if err := s.pool.QueryRow(ctx, query, args...).Scan(&id); err != nil {
		return "", fmt.Errorf("insert verification_log: %w", err)
	}
	return id, nil
}

// CacheKey is the composite key used by Get/Put.
type CacheKey struct {
	ConsumerID         string
	SubjectType        string
	SubjectHash        string
	RequestFingerprint string
}

// Get returns a cached response body if one exists and hasn't expired.
// Returns (nil, false, nil) on miss (not an error).
func (s *Store) Get(ctx context.Context, k CacheKey) (json.RawMessage, bool, error) {
	var body string
	err := s.pool.QueryRow(ctx, `
		SELECT response_body::text
		  FROM verification_cache
		 WHERE consumer_id = $1 AND subject_type = $2 AND subject_hash = $3 AND request_fingerprint = $4
		   AND expires_at > NOW()`,
		k.ConsumerID, k.SubjectType, k.SubjectHash, k.RequestFingerprint,
	).Scan(&body)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("cache get: %w", err)
	}
	return json.RawMessage(body), true, nil
}

// Put writes a cache row, replacing any prior row for the same key.
func (s *Store) Put(ctx context.Context, k CacheKey, body json.RawMessage, ttl time.Duration) error {
	if ttl <= 0 {
		return nil
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO verification_cache (consumer_id, subject_type, subject_hash, request_fingerprint, response_body, expires_at)
		VALUES ($1, $2, $3, $4, $5::jsonb, NOW() + ($6::text || ' seconds')::interval)
		ON CONFLICT (consumer_id, subject_type, subject_hash, request_fingerprint)
		DO UPDATE SET response_body = EXCLUDED.response_body, expires_at = EXCLUDED.expires_at, cached_at = NOW()`,
		k.ConsumerID, k.SubjectType, k.SubjectHash, k.RequestFingerprint, string(body), fmt.Sprintf("%d", int(ttl.Seconds())))
	if err != nil {
		return fmt.Errorf("cache put: %w", err)
	}
	return nil
}

// HashSubject is the canonical hashing scheme for "subject identifier
// goes on the audit chain". Hex SHA-256 of the trimmed upper-cased
// raw identifier. Same recipe in store + audit so cross-referencing
// works.
func HashSubject(rawNIN string) string {
	sum := sha256.Sum256([]byte(rawNIN))
	return hex.EncodeToString(sum[:])
}

// FingerprintRequest hashes the canonical request body. Used as the
// cache discriminator so changing any claimed attribute busts the
// cache for the same subject.
func FingerprintRequest(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
