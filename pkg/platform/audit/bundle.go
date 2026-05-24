// Bundle is the off-platform exchange format for handing a slice of the
// audit chain to an external auditor or SIEM. It carries the entries,
// the anchor (the previous_hash that precedes the slice), and an
// Ed25519 signature computed over the canonical JSON of everything
// except the signature itself.
//
// Verification, given the signed bundle and the publisher's public key:
//
//  1. Parse the bundle.
//  2. Recompute the canonical JSON of the bundle with `signature`
//     replaced by "" (or removed).
//  3. Ed25519.Verify(pubkey, canonical_json, signature).
//  4. Walk the chain in `entries`: each row's previous_hash must equal
//     the prior row's entry_hash (or the bundle's anchor for the first
//     row), and each row's entry_hash must recompute from its content
//     plus previous_hash.
//
// A bundle that survives steps 3 + 4 is provably authentic AND
// internally consistent. A verifier who also obtained a previous
// bundle covering the anchor can chain validation further back.

package audit

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

// GenesisAnchorHash is the previous_hash sentinel used for the very
// first entry on the chain. Mirrors services/audit/internal/chain.GenesisHash.
const GenesisAnchorHash = "0000000000000000000000000000000000000000000000000000000000000000"

const BundleFormatVersion = "1"

// Bundle is the on-the-wire shape returned from /v1/audit/export.
type Bundle struct {
	FormatVersion string        `json:"format_version"`
	Generator     string        `json:"generator"`
	GeneratedAt   time.Time     `json:"generated_at"`
	RangeFromID   int64         `json:"range_from_id"`
	RangeToID     int64         `json:"range_to_id"`
	Anchor        BundleAnchor  `json:"anchor"`
	Entries       []BundleEntry `json:"entries"`
	SigningPubkey string        `json:"signing_pubkey"` // base64 std
	Signature     string        `json:"signature"`      // base64 std, Ed25519 over canonical bytes
}

// BundleAnchor is the link to whatever came before the first entry in
// the bundle. If the bundle starts at entry_id=1, AnchorEntryHash is
// the genesis sentinel. Otherwise it's the entry_hash of the row whose
// id is RangeFromID-1.
type BundleAnchor struct {
	AnchorEntryID   int64  `json:"anchor_entry_id"`   // 0 = genesis (no row precedes)
	AnchorEntryHash string `json:"anchor_entry_hash"` // hex SHA-256 (or 64×'0' for genesis)
}

// BundleEntry mirrors the on-chain row, scoped to what an external
// verifier needs. We intentionally keep this independent of the
// service-internal EntryRecord type so changes to the DB row don't
// silently change the export format.
type BundleEntry struct {
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

// SignBundle computes the canonical JSON of b with Signature blanked,
// signs it with priv, and stores the base64-encoded signature on the
// bundle. SigningPubkey is set to the base64-encoded public half of
// the key so a verifier can identify which key signed.
//
// The function is deterministic for a given input: canonical JSON
// uses sorted keys at every depth and no insignificant whitespace.
func SignBundle(b *Bundle, priv ed25519.PrivateKey) error {
	if len(priv) != ed25519.PrivateKeySize {
		return fmt.Errorf("signing key has wrong length: %d", len(priv))
	}
	b.SigningPubkey = base64.StdEncoding.EncodeToString(priv.Public().(ed25519.PublicKey))
	b.Signature = ""

	canonical, err := CanonicalJSON(b)
	if err != nil {
		return fmt.Errorf("canonicalise bundle: %w", err)
	}
	sig := ed25519.Sign(priv, canonical)
	b.Signature = base64.StdEncoding.EncodeToString(sig)
	return nil
}

// VerifyBundle is the inverse of SignBundle: returns nil if the
// signature is valid for the given bundle and public key. Does NOT
// walk the chain — that's VerifyBundleChain's job.
func VerifyBundle(b *Bundle, pub ed25519.PublicKey) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("public key has wrong length: %d", len(pub))
	}
	if b.Signature == "" {
		return errors.New("bundle has no signature")
	}
	sigBytes, err := base64.StdEncoding.DecodeString(b.Signature)
	if err != nil {
		return fmt.Errorf("decode signature: %w", err)
	}

	// Make a copy with Signature blanked; mutating the original would
	// surprise the caller.
	copyB := *b
	copyB.Signature = ""
	canonical, err := CanonicalJSON(&copyB)
	if err != nil {
		return fmt.Errorf("canonicalise bundle for verify: %w", err)
	}

	if !ed25519.Verify(pub, canonical, sigBytes) {
		return errors.New("bundle signature does not verify")
	}
	return nil
}

// VerifyBundleChain walks the entries inside the bundle, recomputes
// each entry_hash, and confirms previous_hash chaining. Returns the
// index of the first broken entry on failure (0-based) or -1 on
// success.
//
// Combine with VerifyBundle for the full external-auditor check:
// signature proves authenticity, chain walk proves internal
// consistency.
func VerifyBundleChain(b *Bundle) (brokenIndex int, err error) {
	expectedPrev := b.Anchor.AnchorEntryHash
	for i, e := range b.Entries {
		if e.PreviousHash != expectedPrev {
			return i, fmt.Errorf("entry[%d] (id=%d) previous_hash does not match expected; got %s want %s",
				i, e.EntryID, e.PreviousHash, expectedPrev)
		}
		// Recompute via the same chain.Compute the service uses on the
		// write path. We avoid importing services/audit/internal/chain
		// (it's internal) by inlining the SHA-256 over the same field
		// concatenation. If chain.Compute changes, this MUST change
		// too — schema_test.go covers this with a cross-check.
		recomputed := computeEntryHash(e, expectedPrev)
		if recomputed != e.EntryHash {
			return i, fmt.Errorf("entry[%d] (id=%d) entry_hash mismatch; recomputed %s, stored %s",
				i, e.EntryID, recomputed, e.EntryHash)
		}
		expectedPrev = e.EntryHash
	}
	return -1, nil
}

// CanonicalJSON serialises v with stable key ordering at every depth
// and no insignificant whitespace. Used as the byte sequence the
// Ed25519 signature commits to.
//
// Go's encoding/json already sorts map keys when marshalling a map
// (alphabetical), but it preserves struct field order. We round-trip
// through map[string]any so struct field order doesn't affect the
// canonical output — a bundle is a struct, but the canonical form is
// the same regardless of which language built it.
func CanonicalJSON(v any) ([]byte, error) {
	first, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var as any
	if err := json.Unmarshal(first, &as); err != nil {
		return nil, err
	}
	return canonicalEncode(as)
}

func canonicalEncode(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := canonicalWrite(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func canonicalWrite(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool, float64, string:
		b, err := json.Marshal(x)
		if err != nil {
			return err
		}
		buf.Write(b)
	case []any:
		buf.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := canonicalWrite(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			kb, _ := json.Marshal(k)
			buf.Write(kb)
			buf.WriteByte(':')
			if err := canonicalWrite(buf, x[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("unsupported type in canonical JSON: %T", v)
	}
	return nil
}

// chainInput mirrors services/audit/internal/chain.Input byte-for-byte.
// We keep a local copy here so an off-platform verifier built from
// pkg/platform/audit alone never has to import internal packages. The
// json struct-tag order is load-bearing — encoding/json marshals struct
// fields in declaration order, which is what gives us deterministic
// canonical bytes. Reorder these fields only if chain.Input changes,
// and update audit_test.go's regression guard at the same time.
type chainInput struct {
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

func computeEntryHash(e BundleEntry, previousHash string) string {
	in := chainInput{
		EntryUUID:     e.EntryUUID,
		OccurredAt:    e.OccurredAt.UTC().Format(time.RFC3339Nano),
		ActorKind:     e.ActorKind,
		ActorID:       e.ActorID,
		SubjectKind:   e.SubjectKind,
		SubjectID:     e.SubjectID,
		Action:        e.Action,
		Result:        e.Result,
		CorrelationID: e.CorrelationID,
		Detail:        e.Detail,
		PreviousHash:  previousHash,
	}
	if in.Detail == nil {
		in.Detail = json.RawMessage("null")
	}
	body, _ := json.Marshal(in)
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}
