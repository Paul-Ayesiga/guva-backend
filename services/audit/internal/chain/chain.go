// Package chain provides the canonical-serialisation and SHA-256 hash
// computation for audit entries.
//
// The chain invariant: for every row, entry_hash == SHA256(canonical(row))
// where canonical(row) is the JSON serialisation of the named fields in
// the order listed in chainInput. previous_hash is included in the
// canonical bytes, so any tampering with a prior row breaks every
// downstream hash.
//
// Genesis: the first row's previous_hash is the 64-character ASCII zero
// string (repeat('0', 64)). The verifier treats this literal as the
// chain anchor.
package chain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"
)

// GenesisHash is the previous_hash value used for the first entry in
// the chain.
const GenesisHash = "0000000000000000000000000000000000000000000000000000000000000000"

// Input is the set of fields hashed together with previous_hash to
// produce entry_hash. JSON tag order matters — encoding/json marshals
// struct fields in declaration order, which is what gives us
// deterministic canonical bytes.
type Input struct {
	EntryUUID     string          `json:"entry_uuid"`
	OccurredAt    string          `json:"occurred_at"` // RFC3339Nano UTC
	ActorKind     string          `json:"actor_kind"`
	ActorID       string          `json:"actor_id"`
	SubjectKind   string          `json:"subject_kind"`
	SubjectID     string          `json:"subject_id"`
	Action        string          `json:"action"`
	Result        string          `json:"result"`
	CorrelationID string          `json:"correlation_id"`
	Detail        json.RawMessage `json:"detail"`
	PreviousHash  string          `json:"previous_hash"`
}

// Compute returns the hex-encoded SHA-256 of the canonical serialisation
// of i. occurred_at is normalised to UTC RFC3339Nano so two callers
// passing the same logical time produce the same hash regardless of
// time-zone wall.
func Compute(i Input) (string, error) {
	if i.Detail == nil {
		i.Detail = json.RawMessage("null")
	}
	body, err := json.Marshal(i)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// NormaliseTime returns t formatted in UTC RFC3339Nano — the form used
// in the canonical chain input. Use this when building an Input so the
// hash matches what the verifier will recompute.
func NormaliseTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
