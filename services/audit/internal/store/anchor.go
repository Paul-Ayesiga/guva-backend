// Anchor-side store helpers. Kept separate from store.go because the
// anchor concept is orthogonal to the chain itself — anchors summarise
// the chain, but the chain doesn't depend on them.

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// AnchorRecord mirrors a row of audit_anchors.
type AnchorRecord struct {
	AnchorID      int64           `json:"anchor_id"`
	RangeFromID   int64           `json:"range_from_id"`
	RangeToID     int64           `json:"range_to_id"`
	LeafCount     int64           `json:"leaf_count"`
	MerkleRoot    string          `json:"merkle_root"`
	Algorithm     string          `json:"algorithm"`
	ComputedAt    time.Time       `json:"computed_at"`
	ExternalProof json.RawMessage `json:"external_proof,omitempty"`
}

// LatestAnchor returns the most recently computed anchor, or
// (zero-value, ErrNoAnchors) if the table is empty.
func (s *Store) LatestAnchor(ctx context.Context) (AnchorRecord, error) {
	var r AnchorRecord
	var proofStr string
	err := s.reader.QueryRow(ctx, `
		SELECT anchor_id, range_from_id, range_to_id, leaf_count,
		       merkle_root, algorithm, computed_at,
		       COALESCE(external_proof::text, '')
		  FROM audit_anchors
		 ORDER BY anchor_id DESC
		 LIMIT 1`).Scan(
		&r.AnchorID, &r.RangeFromID, &r.RangeToID, &r.LeafCount,
		&r.MerkleRoot, &r.Algorithm, &r.ComputedAt, &proofStr,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AnchorRecord{}, ErrNoAnchors
		}
		return AnchorRecord{}, fmt.Errorf("latest anchor: %w", err)
	}
	if proofStr != "" {
		r.ExternalProof = json.RawMessage(proofStr)
	}
	return r, nil
}

// ListAnchors returns anchors in reverse-chronological order. Cursor
// pagination via after (anchor_id < after). limit defaults to 50,
// hard cap 500.
func (s *Store) ListAnchors(ctx context.Context, after int64, limit int) ([]AnchorRecord, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := s.reader.Query(ctx, `
		SELECT anchor_id, range_from_id, range_to_id, leaf_count,
		       merkle_root, algorithm, computed_at,
		       COALESCE(external_proof::text, '')
		  FROM audit_anchors
		 WHERE ($1 = 0 OR anchor_id < $1)
		 ORDER BY anchor_id DESC
		 LIMIT $2`, after, limit)
	if err != nil {
		return nil, fmt.Errorf("list anchors: %w", err)
	}
	defer rows.Close()
	var out []AnchorRecord
	for rows.Next() {
		var r AnchorRecord
		var proofStr string
		if err := rows.Scan(
			&r.AnchorID, &r.RangeFromID, &r.RangeToID, &r.LeafCount,
			&r.MerkleRoot, &r.Algorithm, &r.ComputedAt, &proofStr,
		); err != nil {
			return nil, fmt.Errorf("scan anchor: %w", err)
		}
		if proofStr != "" {
			r.ExternalProof = json.RawMessage(proofStr)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetAnchor returns one anchor by id, or (zero, ErrNoAnchors) when missing.
func (s *Store) GetAnchor(ctx context.Context, id int64) (AnchorRecord, error) {
	var r AnchorRecord
	var proofStr string
	err := s.reader.QueryRow(ctx, `
		SELECT anchor_id, range_from_id, range_to_id, leaf_count,
		       merkle_root, algorithm, computed_at,
		       COALESCE(external_proof::text, '')
		  FROM audit_anchors WHERE anchor_id = $1`, id).Scan(
		&r.AnchorID, &r.RangeFromID, &r.RangeToID, &r.LeafCount,
		&r.MerkleRoot, &r.Algorithm, &r.ComputedAt, &proofStr,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return AnchorRecord{}, ErrNoAnchors
		}
		return AnchorRecord{}, fmt.Errorf("get anchor: %w", err)
	}
	if proofStr != "" {
		r.ExternalProof = json.RawMessage(proofStr)
	}
	return r, nil
}

// EntryHashRange returns the entry_hash values for entries in
// [fromID, toID] in ascending entry_id order. Used by the anchor
// job to build leaves, and by /anchors/{id}/proof to rebuild the
// Merkle tree just-in-time.
func (s *Store) EntryHashRange(ctx context.Context, fromID, toID int64) ([]string, error) {
	rows, err := s.reader.Query(ctx, `
		SELECT entry_hash
		  FROM audit_entries
		 WHERE entry_id BETWEEN $1 AND $2
		 ORDER BY entry_id ASC`, fromID, toID)
	if err != nil {
		return nil, fmt.Errorf("entry hash range: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// MaxEntryID returns the largest entry_id on the chain, or 0 if empty.
func (s *Store) MaxEntryID(ctx context.Context) (int64, error) {
	var n *int64
	err := s.reader.QueryRow(ctx, `SELECT MAX(entry_id) FROM audit_entries`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("max entry id: %w", err)
	}
	if n == nil {
		return 0, nil
	}
	return *n, nil
}

// EntryIDPosition returns the 0-based index of entryID within the
// inclusive range [fromID, toID]. Used by proof builders that need
// "where does this entry sit in the leaf set?".
//
// Returns ErrNotFound when entryID is outside the range.
func (s *Store) EntryIDPosition(ctx context.Context, fromID, toID, entryID int64) (int, error) {
	if entryID < fromID || entryID > toID {
		return 0, ErrNotFound
	}
	// We have to count rows, not subtract IDs — the chain may grow
	// without gaps today but partitioning + DELETE-protected schema
	// could one day expose holes; the position is "what fraction
	// of the leaf set comes before me", which is rank-based.
	var n int
	err := s.reader.QueryRow(ctx, `
		SELECT COUNT(*)
		  FROM audit_entries
		 WHERE entry_id >= $1 AND entry_id < $2`, fromID, entryID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("position lookup: %w", err)
	}
	return n, nil
}

// InsertAnchor stages a new anchor row. The anchor job calls this
// after computing the Merkle root over the new range.
func (s *Store) InsertAnchor(ctx context.Context, fromID, toID, leafCount int64, root, algorithm string) (int64, error) {
	var id int64
	err := s.writer.QueryRow(ctx, `
		INSERT INTO audit_anchors (range_from_id, range_to_id, leaf_count, merkle_root, algorithm)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING anchor_id`,
		fromID, toID, leafCount, root, algorithm,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert anchor: %w", err)
	}
	return id, nil
}

// ErrNoAnchors signals an empty anchor table.
var ErrNoAnchors = errors.New("no anchors recorded")

// ErrNotFound is returned by lookups that hit nothing (e.g.
// EntryIDPosition called with an entry_id outside the requested range).
var ErrNotFound = errors.New("not found")
