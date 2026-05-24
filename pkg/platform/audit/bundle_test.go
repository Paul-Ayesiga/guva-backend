package audit

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestBundleRoundTrip is the load-bearing test: sign a bundle, verify
// the signature, then walk the chain — exactly what an external
// auditor would do.
func TestBundleRoundTrip(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}

	// Build two entries whose previous_hash chain is internally
	// consistent. We compute the hashes via the same helper a real
	// chain consumer would call, so the round-trip is honest.
	ts := time.Date(2026, 5, 24, 18, 0, 0, 0, time.UTC)
	e1 := BundleEntry{
		EntryID: 1, EntryUUID: "11111111-1111-1111-1111-111111111111",
		OccurredAt: ts, ActorKind: "service", ActorID: "identity",
		Action: "identity.consumer.created", Result: "ok",
		Detail: json.RawMessage(`{"x":1}`),
	}
	e1.PreviousHash = GenesisAnchorHash
	e1.EntryHash = computeEntryHash(e1, e1.PreviousHash)

	e2 := BundleEntry{
		EntryID: 2, EntryUUID: "22222222-2222-2222-2222-222222222222",
		OccurredAt: ts.Add(time.Second), ActorKind: "service", ActorID: "audit",
		Action: "audit.entries.queried", Result: "ok",
		Detail: json.RawMessage(`{"returned":3}`),
	}
	e2.PreviousHash = e1.EntryHash
	e2.EntryHash = computeEntryHash(e2, e2.PreviousHash)

	b := Bundle{
		FormatVersion: BundleFormatVersion,
		Generator:     "audit-test",
		GeneratedAt:   time.Date(2026, 5, 24, 19, 0, 0, 0, time.UTC),
		RangeFromID:   1, RangeToID: 2,
		Anchor:  BundleAnchor{AnchorEntryID: 0, AnchorEntryHash: GenesisAnchorHash},
		Entries: []BundleEntry{e1, e2},
	}

	if err := SignBundle(&b, priv); err != nil {
		t.Fatalf("SignBundle: %v", err)
	}
	if b.Signature == "" {
		t.Fatal("signature not set")
	}
	gotPub, err := base64.StdEncoding.DecodeString(b.SigningPubkey)
	if err != nil || ed25519.PublicKey(gotPub).Equal(pub) == false {
		t.Fatalf("signing_pubkey field does not match: %v", err)
	}

	if err := VerifyBundle(&b, pub); err != nil {
		t.Fatalf("VerifyBundle: %v", err)
	}
	if i, err := VerifyBundleChain(&b); err != nil {
		t.Fatalf("VerifyBundleChain returned error at index %d: %v", i, err)
	}
}

// TestSignatureDetectsTampering ensures any post-sign mutation flips
// verification to fail. Covers the whole reason the signature exists.
func TestSignatureDetectsTampering(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)

	b := Bundle{
		FormatVersion: BundleFormatVersion,
		Generator:     "audit-test",
		GeneratedAt:   time.Date(2026, 5, 24, 19, 0, 0, 0, time.UTC),
		RangeFromID:   1, RangeToID: 1,
		Anchor:  BundleAnchor{AnchorEntryHash: GenesisAnchorHash},
		Entries: []BundleEntry{{EntryID: 1, EntryUUID: "x", ActorKind: "service", ActorID: "x", Action: "x.y.z", Result: "ok", PreviousHash: GenesisAnchorHash, EntryHash: "deadbeef"}},
	}
	_ = SignBundle(&b, priv)

	// Mutate after signing.
	b.Entries[0].ActorID = "attacker"
	if err := VerifyBundle(&b, pub); err == nil {
		t.Fatal("verify accepted a tampered bundle")
	}
}

// TestChainWalkDetectsBrokenLink ensures the chain-walk reports the
// first broken row even on a correctly-signed bundle (signature OK,
// content lies).
func TestChainWalkDetectsBrokenLink(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)

	ts := time.Now().UTC()
	e1 := BundleEntry{EntryID: 1, EntryUUID: "a", OccurredAt: ts, ActorKind: "service", ActorID: "x", Action: "x.y", Result: "ok"}
	e1.PreviousHash = GenesisAnchorHash
	e1.EntryHash = computeEntryHash(e1, e1.PreviousHash)

	// Deliberately broken: previous_hash claims chaining to e1 but
	// entry_hash is bogus.
	e2 := BundleEntry{EntryID: 2, EntryUUID: "b", OccurredAt: ts, ActorKind: "service", ActorID: "x", Action: "x.y", Result: "ok"}
	e2.PreviousHash = e1.EntryHash
	e2.EntryHash = "0000000000000000000000000000000000000000000000000000000000000000"

	b := Bundle{
		FormatVersion: BundleFormatVersion,
		Anchor:        BundleAnchor{AnchorEntryHash: GenesisAnchorHash},
		Entries:       []BundleEntry{e1, e2},
	}
	_ = SignBundle(&b, priv)

	idx, err := VerifyBundleChain(&b)
	if err == nil || idx != 1 {
		t.Fatalf("expected chain failure at index 1, got idx=%d err=%v", idx, err)
	}
	if !strings.Contains(err.Error(), "entry_hash mismatch") {
		t.Fatalf("error did not mention entry_hash: %v", err)
	}
}

// TestCanonicalJSONIsStable proves the byte sequence we sign is the
// same regardless of declaration order in the source. Re-runs guard
// against a future refactor that reorders Bundle fields and silently
// changes signatures of bundles that "should be" identical.
func TestCanonicalJSONIsStable(t *testing.T) {
	type A struct {
		B int `json:"b"`
		A int `json:"a"`
	}
	type B struct {
		A int `json:"a"`
		B int `json:"b"`
	}
	c1, _ := CanonicalJSON(A{B: 2, A: 1})
	c2, _ := CanonicalJSON(B{A: 1, B: 2})
	if string(c1) != string(c2) {
		t.Fatalf("canonical JSON differs by struct field order:\n  %s\n  %s", c1, c2)
	}
	if string(c1) != `{"a":1,"b":2}` {
		t.Fatalf("canonical form unexpected: %s", c1)
	}
}
