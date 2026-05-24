// Binary Merkle tree over hex-encoded entry hashes.
//
// Leaf hashing:  leaf  = SHA256( hex_bytes(entry_hash) )
// Internal:      node  = SHA256( left || right )
// Odd-leaf rule: duplicate the last leaf (standard RFC 6962-style)
//
// The root commits to every leaf — flipping one byte anywhere in any
// entry flips the root. Publishing the root to an external witness
// (consortium ledger, blockchain, Sigstore Rekor) lets a third party
// later confirm any individual entry was in the chain at anchor time,
// given the entry + the inclusion proof.
//
// Proof shape:
//
//	[]ProofStep{ {Side: "L"|"R", Hash: hex}, ... }
//
// Verification:
//
//	h := SHA256( hex_bytes(entry_hash) )
//	for step in proof:
//	    h = SHA256(step.Hash || h)  if step.Side == "L"
//	    h = SHA256(h || step.Hash)  if step.Side == "R"
//	assert h == root

package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

const MerkleAlgorithm = "sha256-binary-merkle-v1"

// ComputeMerkleRoot returns the root for an ordered slice of
// hex-encoded leaf hashes (the chain's entry_hash values, in
// entry_id order). Empty input returns an error — anchoring an
// empty range is meaningless and the caller should skip.
func ComputeMerkleRoot(leafHexes []string) (string, error) {
	level, err := leavesAsBytes(leafHexes)
	if err != nil {
		return "", err
	}
	if len(level) == 0 {
		return "", errors.New("empty leaf set")
	}
	for len(level) > 1 {
		level = nextLevel(level)
	}
	return hex.EncodeToString(level[0]), nil
}

// ProofStep is one sibling on the path from a leaf to the root.
type ProofStep struct {
	Side string `json:"side"` // "L" if sibling is to the left, "R" if to the right
	Hash string `json:"hash"` // hex SHA-256
}

// BuildInclusionProof returns the proof for leafIndex (0-based) in the
// leaf set. Verification re-applies the steps in order starting from
// the target leaf's hash.
func BuildInclusionProof(leafHexes []string, leafIndex int) ([]ProofStep, error) {
	if leafIndex < 0 || leafIndex >= len(leafHexes) {
		return nil, fmt.Errorf("leaf index %d out of range [0,%d)", leafIndex, len(leafHexes))
	}
	level, err := leavesAsBytes(leafHexes)
	if err != nil {
		return nil, err
	}
	idx := leafIndex
	var proof []ProofStep
	for len(level) > 1 {
		var sibling []byte
		var side string
		if idx%2 == 0 {
			// We are left; sibling is right (or duplicate of self on odd tail).
			if idx+1 < len(level) {
				sibling = level[idx+1]
			} else {
				sibling = level[idx]
			}
			side = "R"
		} else {
			sibling = level[idx-1]
			side = "L"
		}
		proof = append(proof, ProofStep{Side: side, Hash: hex.EncodeToString(sibling)})
		level = nextLevel(level)
		idx /= 2
	}
	return proof, nil
}

// VerifyInclusionProof returns true if applying steps to leafHex
// reproduces rootHex.
func VerifyInclusionProof(leafHex string, steps []ProofStep, rootHex string) (bool, error) {
	leaf, err := hex.DecodeString(leafHex)
	if err != nil {
		return false, fmt.Errorf("decode leaf: %w", err)
	}
	h := sha256.Sum256(leaf)
	cur := h[:]
	for i, s := range steps {
		sib, err := hex.DecodeString(s.Hash)
		if err != nil {
			return false, fmt.Errorf("decode step[%d]: %w", i, err)
		}
		switch s.Side {
		case "L":
			cur = combine(sib, cur)
		case "R":
			cur = combine(cur, sib)
		default:
			return false, fmt.Errorf("step[%d] invalid side %q", i, s.Side)
		}
	}
	return hex.EncodeToString(cur) == rootHex, nil
}

func leavesAsBytes(leafHexes []string) ([][]byte, error) {
	out := make([][]byte, 0, len(leafHexes))
	for i, h := range leafHexes {
		raw, err := hex.DecodeString(h)
		if err != nil {
			return nil, fmt.Errorf("leaf %d: %w", i, err)
		}
		sum := sha256.Sum256(raw)
		out = append(out, sum[:])
	}
	return out, nil
}

func nextLevel(level [][]byte) [][]byte {
	next := make([][]byte, 0, (len(level)+1)/2)
	for i := 0; i < len(level); i += 2 {
		if i+1 < len(level) {
			next = append(next, combine(level[i], level[i+1]))
		} else {
			// odd tail: pair the last leaf with itself
			next = append(next, combine(level[i], level[i]))
		}
	}
	return next
}

func combine(a, b []byte) []byte {
	h := sha256.New()
	h.Write(a)
	h.Write(b)
	return h.Sum(nil)
}
