package audit

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

// TestMerkleRootAndProofRoundTrip is the load-bearing case: compute a
// root, request a proof for every leaf, verify each proof reproduces
// the root.
func TestMerkleRootAndProofRoundTrip(t *testing.T) {
	for _, n := range []int{1, 2, 3, 4, 5, 7, 8, 9, 16, 17, 100} {
		leaves := makeLeaves(n)
		root, err := ComputeMerkleRoot(leaves)
		if err != nil {
			t.Fatalf("ComputeMerkleRoot(n=%d): %v", n, err)
		}
		for i := 0; i < n; i++ {
			proof, err := BuildInclusionProof(leaves, i)
			if err != nil {
				t.Fatalf("BuildInclusionProof(n=%d,i=%d): %v", n, i, err)
			}
			ok, err := VerifyInclusionProof(leaves[i], proof, root)
			if err != nil {
				t.Fatalf("VerifyInclusionProof(n=%d,i=%d): %v", n, i, err)
			}
			if !ok {
				t.Fatalf("VerifyInclusionProof(n=%d,i=%d) returned false", n, i)
			}
		}
	}
}

// TestMerkleDetectsTampering ensures changing any leaf flips the root.
func TestMerkleDetectsTampering(t *testing.T) {
	leaves := makeLeaves(8)
	root, _ := ComputeMerkleRoot(leaves)

	for i := range leaves {
		bad := append([]string{}, leaves...)
		// flip one nibble in the i-th leaf
		first := bad[i][0]
		switch first {
		case '0':
			bad[i] = "f" + bad[i][1:]
		default:
			bad[i] = "0" + bad[i][1:]
		}
		newRoot, _ := ComputeMerkleRoot(bad)
		if newRoot == root {
			t.Fatalf("tampering leaf %d did not change root", i)
		}
	}
}

// TestProofDetectsWrongRoot ensures a proof for one root rejects a
// different root.
func TestProofDetectsWrongRoot(t *testing.T) {
	leaves := makeLeaves(5)
	root, _ := ComputeMerkleRoot(leaves)
	proof, _ := BuildInclusionProof(leaves, 2)

	wrongRoot := strings.Repeat("a", 64)
	ok, _ := VerifyInclusionProof(leaves[2], proof, wrongRoot)
	if ok {
		t.Fatal("proof verified against the wrong root")
	}
	// And against the original root, it should still pass.
	ok, _ = VerifyInclusionProof(leaves[2], proof, root)
	if !ok {
		t.Fatal("proof failed against the correct root")
	}
}

func makeLeaves(n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		sum := sha256.Sum256([]byte{byte(i)})
		out[i] = hex.EncodeToString(sum[:])
	}
	return out
}
