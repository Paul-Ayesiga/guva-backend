// Package store is the integration service's Postgres persistence.
// Two responsibilities: write the operational lookup log + provide
// the pool the audit Worker drains.
package store

import (
	"context"
	"fmt"
	"time"

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

// LookupEntry is one row of lookup_log. Subject is the hashed NIN
// per platform PII convention.
type LookupEntry struct {
	LookupID           string
	Backend            string
	Caller             string
	SubjectType        string
	SubjectHash        string
	Status             string
	UpstreamStatusCode *int
	LatencyMS          int
	CorrelationID      string
}

// LogLookup inserts one row, returning the (server-assigned) lookup_id.
func (s *Store) LogLookup(ctx context.Context, e LookupEntry) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO lookup_log
		    (backend, caller, subject_type, subject_hash, status,
		     upstream_status_code, latency_ms, correlation_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7, NULLIF($8,'')::uuid)
		RETURNING lookup_id`,
		e.Backend, e.Caller, e.SubjectType, e.SubjectHash, e.Status,
		e.UpstreamStatusCode, e.LatencyMS, e.CorrelationID,
	).Scan(&id)
	if err != nil {
		return "", fmt.Errorf("insert lookup_log: %w", err)
	}
	return id, nil
}
