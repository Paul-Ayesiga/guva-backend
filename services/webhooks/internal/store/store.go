// Package store is the Postgres persistence for webhook subscriptions
// and delivery records.
package store

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
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

func (s *Store) Close() { s.pool.Close() }

// Subscription is the canonical shape of a webhook subscription as
// stored in the DB and returned by the HTTP API (minus `secret`, which
// is returned only on creation).
type Subscription struct {
	ID                 string     `json:"id"`
	ConsumerID         string     `json:"consumer_id"`
	TargetURL          string     `json:"target_url"`
	EventTypePatterns  []string   `json:"event_type_patterns"`
	Enabled            bool       `json:"enabled"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
	LastDeliveryAt     *time.Time `json:"last_delivery_at,omitempty"`
	LastDeliveryStatus *int       `json:"last_delivery_status,omitempty"`
	LastDeliveryError  *string    `json:"last_delivery_error,omitempty"`

	// Secret is populated only on Create (returned exactly once) and
	// surfaced internally for delivery signing. Never echoed back by
	// GET endpoints.
	Secret string `json:"secret,omitempty"`
}

// CreateSubscription generates a UUID + a fresh HMAC secret, persists,
// and returns the row INCLUDING the secret (caller is responsible for
// returning it to the consumer exactly once).
func (s *Store) CreateSubscription(ctx context.Context, in Subscription) (Subscription, error) {
	if in.TargetURL == "" || len(in.EventTypePatterns) == 0 || in.ConsumerID == "" {
		return Subscription{}, errors.New("consumer_id, target_url and event_type_patterns are required")
	}
	id := uuid.NewString()
	secret, err := generateSecret()
	if err != nil {
		return Subscription{}, fmt.Errorf("generate secret: %w", err)
	}
	if !in.Enabled {
		in.Enabled = true
	}
	row := s.pool.QueryRow(ctx, `
		INSERT INTO webhook_subscriptions
		    (id, consumer_id, target_url, event_type_patterns, secret, enabled)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, consumer_id, target_url, event_type_patterns, enabled, created_at, updated_at`,
		id, in.ConsumerID, in.TargetURL, in.EventTypePatterns, secret, in.Enabled)
	var out Subscription
	if err := row.Scan(&out.ID, &out.ConsumerID, &out.TargetURL,
		&out.EventTypePatterns, &out.Enabled, &out.CreatedAt, &out.UpdatedAt); err != nil {
		return Subscription{}, fmt.Errorf("insert subscription: %w", err)
	}
	out.Secret = secret
	return out, nil
}

func (s *Store) GetSubscription(ctx context.Context, id string) (Subscription, error) {
	var out Subscription
	err := s.pool.QueryRow(ctx, `
		SELECT id, consumer_id, target_url, event_type_patterns, enabled,
		       created_at, updated_at, last_delivery_at, last_delivery_status, last_delivery_error
		  FROM webhook_subscriptions WHERE id = $1`, id).Scan(
		&out.ID, &out.ConsumerID, &out.TargetURL, &out.EventTypePatterns, &out.Enabled,
		&out.CreatedAt, &out.UpdatedAt, &out.LastDeliveryAt, &out.LastDeliveryStatus, &out.LastDeliveryError)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Subscription{}, ErrNotFound
		}
		return Subscription{}, err
	}
	return out, nil
}

// ListSubscriptions returns the subscriptions for one consumer, or all
// if consumerID is empty (admin path).
func (s *Store) ListSubscriptions(ctx context.Context, consumerID string, limit int) ([]Subscription, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var (
		rows pgx.Rows
		err  error
	)
	q := `
		SELECT id, consumer_id, target_url, event_type_patterns, enabled,
		       created_at, updated_at, last_delivery_at, last_delivery_status, last_delivery_error
		  FROM webhook_subscriptions %s
		 ORDER BY created_at DESC LIMIT $1`
	if consumerID == "" {
		rows, err = s.pool.Query(ctx, fmt.Sprintf(q, ""), limit)
	} else {
		rows, err = s.pool.Query(ctx, fmt.Sprintf(q, "WHERE consumer_id = $2"), limit, consumerID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subscription
	for rows.Next() {
		var s Subscription
		if err := rows.Scan(&s.ID, &s.ConsumerID, &s.TargetURL, &s.EventTypePatterns, &s.Enabled,
			&s.CreatedAt, &s.UpdatedAt, &s.LastDeliveryAt, &s.LastDeliveryStatus, &s.LastDeliveryError); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// MatchingSubscriptions returns all enabled subscriptions whose
// event_type_patterns match the given event type. Glob matching:
// "*" matches everything; "identity.*" matches "identity.foo.bar";
// exact match wins always.
func (s *Store) MatchingSubscriptions(ctx context.Context, eventType string) ([]Subscription, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, consumer_id, target_url, event_type_patterns, secret, enabled
		  FROM webhook_subscriptions WHERE enabled = true`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Subscription
	for rows.Next() {
		var s Subscription
		if err := rows.Scan(&s.ID, &s.ConsumerID, &s.TargetURL, &s.EventTypePatterns,
			&s.Secret, &s.Enabled); err != nil {
			return nil, err
		}
		if matchesAny(s.EventTypePatterns, eventType) {
			out = append(out, s)
		}
	}
	return out, rows.Err()
}

// DeleteSubscription removes the subscription. Deliveries cascade.
func (s *Store) DeleteSubscription(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM webhook_subscriptions WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Delivery represents an attempt to POST one event to one subscription.
type Delivery struct {
	ID              int64      `json:"id"`
	DeliveryUUID    string     `json:"delivery_uuid"`
	SubscriptionID  string     `json:"subscription_id"`
	EventUUID       string     `json:"event_uuid"`
	EventType       string     `json:"event_type"`
	Attempt         int        `json:"attempt"`
	Status          string     `json:"status"`
	HTTPStatus      *int       `json:"http_status,omitempty"`
	ResponseExcerpt *string    `json:"response_excerpt,omitempty"`
	Error           *string    `json:"error,omitempty"`
	QueuedAt        time.Time  `json:"queued_at"`
	AttemptedAt     *time.Time `json:"attempted_at,omitempty"`
	NextRetryAt     *time.Time `json:"next_retry_at,omitempty"`
}

// RecordDelivery inserts a delivery row in pending state and returns
// the assigned UUID — used by the matcher when fanning out to Rabbit.
func (s *Store) RecordDelivery(ctx context.Context, subscriptionID, eventUUID, eventType string) (string, error) {
	deliveryUUID := uuid.NewString()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO webhook_deliveries (delivery_uuid, subscription_id, event_uuid, event_type, status)
		VALUES ($1, $2, $3, $4, 'pending')`,
		deliveryUUID, subscriptionID, eventUUID, eventType)
	return deliveryUUID, err
}

// MarkDelivered updates the delivery row + subscription's last-delivery
// summary after a successful POST.
func (s *Store) MarkDelivered(ctx context.Context, deliveryUUID string, attempt int, httpStatus int, responseExcerpt string) error {
	now := time.Now().UTC()
	if _, err := s.pool.Exec(ctx, `
		UPDATE webhook_deliveries
		   SET status='ok', attempt=$2, http_status=$3, response_excerpt=$4,
		       attempted_at=$5, next_retry_at=NULL, error=NULL
		 WHERE delivery_uuid=$1`, deliveryUUID, attempt, httpStatus, responseExcerpt, now); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE webhook_subscriptions
		   SET last_delivery_at=$1, last_delivery_status=$2, last_delivery_error=NULL, updated_at=$1
		 WHERE id = (SELECT subscription_id FROM webhook_deliveries WHERE delivery_uuid=$3)`,
		now, httpStatus, deliveryUUID); err != nil {
		return err
	}
	return nil
}

// MarkAttempt records a failed attempt with retry scheduling. If it's
// the terminal attempt, status becomes "dlq".
func (s *Store) MarkAttempt(ctx context.Context, deliveryUUID string, attempt int, httpStatus *int, errMsg string, nextRetry *time.Time, terminal bool) error {
	now := time.Now().UTC()
	status := "retry"
	if terminal {
		status = "dlq"
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE webhook_deliveries
		   SET status=$2, attempt=$3, http_status=$4, error=$5,
		       attempted_at=$6, next_retry_at=$7
		 WHERE delivery_uuid=$1`,
		deliveryUUID, status, attempt, httpStatus, errMsg, now, nextRetry); err != nil {
		return err
	}
	// Also surface the latest error onto the subscription summary so a
	// GET on the subscription shows it without joining.
	statusInt := 0
	if httpStatus != nil {
		statusInt = *httpStatus
	}
	if _, err := s.pool.Exec(ctx, `
		UPDATE webhook_subscriptions
		   SET last_delivery_at=$1, last_delivery_status=$2, last_delivery_error=$3, updated_at=$1
		 WHERE id=(SELECT subscription_id FROM webhook_deliveries WHERE delivery_uuid=$4)`,
		now, statusInt, errMsg, deliveryUUID); err != nil {
		return err
	}
	return nil
}

// ListDeliveries returns recent deliveries for a subscription.
func (s *Store) ListDeliveries(ctx context.Context, subscriptionID string, limit int) ([]Delivery, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, delivery_uuid, subscription_id, event_uuid, event_type, attempt, status,
		       http_status, response_excerpt, error, queued_at, attempted_at, next_retry_at
		  FROM webhook_deliveries WHERE subscription_id = $1
		 ORDER BY queued_at DESC LIMIT $2`, subscriptionID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.ID, &d.DeliveryUUID, &d.SubscriptionID, &d.EventUUID, &d.EventType,
			&d.Attempt, &d.Status, &d.HTTPStatus, &d.ResponseExcerpt, &d.Error,
			&d.QueuedAt, &d.AttemptedAt, &d.NextRetryAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ErrNotFound is the canonical "no such row" sentinel.
var ErrNotFound = errors.New("not found")

// matchesAny implements the glob matching used by event_type_patterns.
// Supports trailing "*" wildcard (e.g. "identity.*") and the lone "*".
func matchesAny(patterns []string, eventType string) bool {
	for _, p := range patterns {
		if p == "*" || p == eventType {
			return true
		}
		// trailing wildcard: identity.*
		if len(p) > 1 && p[len(p)-1] == '*' {
			prefix := p[:len(p)-1] // includes the trailing "." if author wrote it
			if len(eventType) >= len(prefix) && eventType[:len(prefix)] == prefix {
				return true
			}
		}
	}
	return false
}

// generateSecret returns 32 random bytes hex-encoded — 256 bits of
// entropy, suitable for HMAC-SHA256 keys.
func generateSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
