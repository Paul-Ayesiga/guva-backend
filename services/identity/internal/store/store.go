// Package store provides Postgres-backed persistence for the identity
// service. Thin wrapper over pgx; no ORM. Each query is explicit so the
// generated SQL is easy to reason about under load.
package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Store holds the pgx connection pool. Construct with Open and close
// with Close (idempotent).
type Store struct {
	pool *pgxpool.Pool
}

// Open dials Postgres and runs SELECT 1 to confirm the connection is
// live. Caller is responsible for invoking Close.
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

// Close releases pool resources. Safe to call multiple times.
func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// ConsumerRegistration is the data carried by /consumers endpoints.
// This is the audit/intent record kept on our side; the corresponding
// Keycloak client is the source of truth for credentials and scopes.
type ConsumerRegistration struct {
	ID               string    `json:"id"`
	AgencyName       string    `json:"agency_name"`
	ContactEmail     string    `json:"contact_email"`
	KeycloakClientID string    `json:"keycloak_client_id"`
	Scopes           []string  `json:"scopes"`
	Status           string    `json:"status"`
	CreatedAt        time.Time `json:"created_at"`
}

// CountConsumers returns the number of recorded consumer registrations.
// Used by the readiness probe and the /consumers index endpoint.
func (s *Store) CountConsumers(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM consumer_registrations`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count consumers: %w", err)
	}
	return n, nil
}

// CreateConsumer inserts a registration row. The pre-generated ID lets
// the handler return the resource location immediately without a second
// round-trip.
func (s *Store) CreateConsumer(ctx context.Context, c ConsumerRegistration) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO consumer_registrations
		    (id, agency_name, contact_email, keycloak_client_id, scopes, status, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		c.ID, c.AgencyName, c.ContactEmail, c.KeycloakClientID, c.Scopes, c.Status, c.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("insert consumer: %w", err)
	}
	return nil
}

// GetConsumer reads a registration by ID. Returns ErrNotFound when the
// row doesn't exist so handlers can map it to HTTP 404.
func (s *Store) GetConsumer(ctx context.Context, id string) (ConsumerRegistration, error) {
	var c ConsumerRegistration
	err := s.pool.QueryRow(ctx,
		`SELECT id, agency_name, contact_email, keycloak_client_id, scopes, status, created_at
		   FROM consumer_registrations
		  WHERE id = $1`, id,
	).Scan(&c.ID, &c.AgencyName, &c.ContactEmail, &c.KeycloakClientID, &c.Scopes, &c.Status, &c.CreatedAt)
	if err != nil {
		if err == pgx.ErrNoRows {
			return ConsumerRegistration{}, ErrNotFound
		}
		return ConsumerRegistration{}, fmt.Errorf("get consumer: %w", err)
	}
	return c, nil
}

// ErrNotFound is returned by GetConsumer when no row matches the ID.
var ErrNotFound = fmt.Errorf("consumer registration not found")
