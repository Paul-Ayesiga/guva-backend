// Package store is the consent-service Postgres persistence: grants
// (append-only except the revoke transition) and the audit outbox.
package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

// Grant is the canonical shape of a consent grant. Subject is always
// the hashed NIN — the raw NIN never lands here.
type Grant struct {
	ID                 string     `json:"id"`
	CitizenSubjectType string     `json:"citizen_subject_type"`
	CitizenSubjectHash string     `json:"citizen_subject_hash"`
	ConsumerID         string     `json:"consumer_id"`
	Upstream           string     `json:"upstream"`
	Purpose            string     `json:"purpose"`
	AllowedAttributes  []string   `json:"allowed_attributes"`
	GrantedAt          time.Time  `json:"granted_at"`
	ExpiresAt          time.Time  `json:"expires_at"`
	RevokedAt          *time.Time `json:"revoked_at,omitempty"`
	RevocationReason   string     `json:"revocation_reason,omitempty"`
	AssertionJWT       string     `json:"assertion_jwt"`
	SigningKeyID       string     `json:"signing_key_id"`
}

// CreateGrant inserts a fresh grant. The caller has already built the
// signed assertion (we don't sign here because the signer lives in
// pkg/platform and consent's server layer is the only thing that
// holds the key).
func (s *Store) CreateGrant(ctx context.Context, g Grant) (Grant, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO consent_grants
		    (citizen_subject_type, citizen_subject_hash, consumer_id, upstream,
		     purpose, allowed_attributes, expires_at, assertion_jwt, signing_key_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING id, granted_at`,
		g.CitizenSubjectType, g.CitizenSubjectHash, g.ConsumerID, g.Upstream,
		g.Purpose, g.AllowedAttributes, g.ExpiresAt, g.AssertionJWT, g.SigningKeyID)
	if err := row.Scan(&g.ID, &g.GrantedAt); err != nil {
		return Grant{}, fmt.Errorf("insert consent grant: %w", err)
	}
	return g, nil
}

// ListGrantsForCitizen returns the grants for one citizen (keyed by
// the SHA-256 hash of their NIN), newest first. Used by the citizen
// portal's dashboard. Hard-capped at 200 rows.
func (s *Store) ListGrantsForCitizen(ctx context.Context, citizenHash string, limit int) ([]Grant, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, citizen_subject_type, citizen_subject_hash, consumer_id, upstream,
		       purpose, allowed_attributes, granted_at, expires_at,
		       revoked_at, COALESCE(revocation_reason, ''),
		       assertion_jwt, signing_key_id
		  FROM consent_grants
		 WHERE citizen_subject_hash = $1
		 ORDER BY granted_at DESC
		 LIMIT $2`, citizenHash, limit)
	if err != nil {
		return nil, fmt.Errorf("list grants: %w", err)
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.ID, &g.CitizenSubjectType, &g.CitizenSubjectHash, &g.ConsumerID, &g.Upstream,
			&g.Purpose, &g.AllowedAttributes, &g.GrantedAt, &g.ExpiresAt,
			&g.RevokedAt, &g.RevocationReason,
			&g.AssertionJWT, &g.SigningKeyID); err != nil {
			return nil, fmt.Errorf("scan grant: %w", err)
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// CreateGrantWithID inserts a fresh grant with a caller-chosen id —
// required when the assertion JWT (which embeds the grant id) has to
// be built before the row exists.
func (s *Store) CreateGrantWithID(ctx context.Context, g Grant) (Grant, error) {
	row := s.pool.QueryRow(ctx, `
		INSERT INTO consent_grants
		    (id, citizen_subject_type, citizen_subject_hash, consumer_id, upstream,
		     purpose, allowed_attributes, expires_at, assertion_jwt, signing_key_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING granted_at`,
		g.ID, g.CitizenSubjectType, g.CitizenSubjectHash, g.ConsumerID, g.Upstream,
		g.Purpose, g.AllowedAttributes, g.ExpiresAt, g.AssertionJWT, g.SigningKeyID)
	if err := row.Scan(&g.GrantedAt); err != nil {
		return Grant{}, fmt.Errorf("insert consent grant: %w", err)
	}
	return g, nil
}

// GetGrant returns a single grant by id, or ErrNotFound.
func (s *Store) GetGrant(ctx context.Context, id string) (Grant, error) {
	var g Grant
	err := s.pool.QueryRow(ctx, `
		SELECT id, citizen_subject_type, citizen_subject_hash, consumer_id, upstream,
		       purpose, allowed_attributes, granted_at, expires_at,
		       revoked_at, COALESCE(revocation_reason, ''),
		       assertion_jwt, signing_key_id
		  FROM consent_grants WHERE id = $1`, id,
	).Scan(&g.ID, &g.CitizenSubjectType, &g.CitizenSubjectHash, &g.ConsumerID, &g.Upstream,
		&g.Purpose, &g.AllowedAttributes, &g.GrantedAt, &g.ExpiresAt,
		&g.RevokedAt, &g.RevocationReason,
		&g.AssertionJWT, &g.SigningKeyID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Grant{}, ErrNotFound
		}
		return Grant{}, fmt.Errorf("get consent grant: %w", err)
	}
	return g, nil
}

// RevokeGrant sets revoked_at + revocation_reason. The trigger on
// consent_grants enforces that this is the ONLY allowed mutation —
// every other field is locked.
func (s *Store) RevokeGrant(ctx context.Context, id, reason string) (Grant, error) {
	g, err := s.GetGrant(ctx, id)
	if err != nil {
		return Grant{}, err
	}
	if g.RevokedAt != nil {
		return g, nil // idempotent — already revoked
	}
	now := time.Now().UTC()
	if _, err := s.pool.Exec(ctx, `
		UPDATE consent_grants
		   SET revoked_at = $1, revocation_reason = NULLIF($2,'')
		 WHERE id = $3`, now, reason, id); err != nil {
		return Grant{}, fmt.Errorf("revoke consent grant: %w", err)
	}
	g.RevokedAt = &now
	g.RevocationReason = reason
	return g, nil
}

// HashSubject — shared canonical hashing recipe with the verification
// service. Same SHA-256(NIN) hex so cross-service joins via the hash
// work and the raw NIN never crosses a service boundary.
func HashSubject(rawNIN string) string {
	sum := sha256.Sum256([]byte(rawNIN))
	return hex.EncodeToString(sum[:])
}

// ErrNotFound is the canonical "no such grant" sentinel.
var ErrNotFound = errors.New("consent grant not found")
