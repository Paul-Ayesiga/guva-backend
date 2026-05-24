// audit-verify — standalone verifier for GUVA audit export bundles.
//
// Given a bundle file and the publisher's Ed25519 public key, this
// tool:
//
//  1. Decodes the bundle.
//  2. Verifies the Ed25519 signature against canonical JSON.
//  3. Walks the chain inside the bundle, recomputing every entry_hash.
//  4. Reports pass / fail with the first broken entry's id if any.
//
// It depends only on pkg/platform/audit (and the Go stdlib). Drop the
// binary on any machine and you have an offline verifier — no DB,
// no network, no access to the platform.
//
// Examples:
//
//	# Public key fetched from the platform's /export/pubkey endpoint:
//	curl -s http://audit.example/v1/audit/export/pubkey | jq -r .public_key_b64 > pub.b64
//	audit-verify --bundle bundle.json --pubkey-file pub.b64
//
//	# Pass the key inline:
//	audit-verify --bundle bundle.json --pubkey-b64 TjHIrxmw...
//
//	# Fetch the bundle through the gateway in one step (export requires
//	# audit:read; this tool stays offline by default — wrap with curl).
//
// Exit codes: 0 = ok, 1 = signature failure, 2 = chain break,
// 3 = usage/io error.

package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/guva-ug/guva-backend/pkg/platform/audit"
)

func main() {
	var (
		bundlePath = flag.String("bundle", "", "path to the bundle JSON file (or '-' for stdin)")
		pubB64     = flag.String("pubkey-b64", "", "Ed25519 public key, base64 std-encoded")
		pubFile    = flag.String("pubkey-file", "", "path to a file containing the base64-encoded Ed25519 public key (only the first non-empty line is read)")
		pubURL     = flag.String("pubkey-url", "", "URL to fetch the public key from (expects the /export/pubkey JSON shape)")
		quiet      = flag.Bool("quiet", false, "suppress success output; exit code is the only signal")
	)
	flag.Parse()

	if *bundlePath == "" {
		fmt.Fprintln(os.Stderr, "audit-verify: --bundle is required")
		flag.Usage()
		os.Exit(3)
	}

	bundle, err := readBundle(*bundlePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "audit-verify: read bundle: %v\n", err)
		os.Exit(3)
	}

	pub, err := loadPubKey(*pubB64, *pubFile, *pubURL, bundle)
	if err != nil {
		fmt.Fprintf(os.Stderr, "audit-verify: load pubkey: %v\n", err)
		os.Exit(3)
	}

	if err := audit.VerifyBundle(&bundle, pub); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL signature: %v\n", err)
		os.Exit(1)
	}

	if idx, err := audit.VerifyBundleChain(&bundle); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL chain at entry index %d (entry_id=%d): %v\n",
			idx, bundle.Entries[idx].EntryID, err)
		os.Exit(2)
	}

	if *quiet {
		return
	}
	fmt.Printf("OK\n")
	fmt.Printf("  format_version: %s\n", bundle.FormatVersion)
	fmt.Printf("  generated_at:   %s\n", bundle.GeneratedAt.UTC().Format(time.RFC3339))
	fmt.Printf("  generator:      %s\n", bundle.Generator)
	fmt.Printf("  range:          %d..%d (%d entries)\n",
		bundle.RangeFromID, bundle.RangeToID, len(bundle.Entries))
	fmt.Printf("  anchor:         entry_id=%d hash=%s\n",
		bundle.Anchor.AnchorEntryID, shortHash(bundle.Anchor.AnchorEntryHash))
	fmt.Printf("  signing_pubkey: %s\n", shortHash(bundle.SigningPubkey))
	fmt.Printf("  signature:      verified Ed25519\n")
	fmt.Printf("  chain:          %d entries verified\n", len(bundle.Entries))
}

func readBundle(path string) (audit.Bundle, error) {
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		return audit.Bundle{}, err
	}
	var b audit.Bundle
	if err := json.Unmarshal(data, &b); err != nil {
		return audit.Bundle{}, fmt.Errorf("unmarshal: %w", err)
	}
	return b, nil
}

// loadPubKey resolves a base64 public key from the highest-priority
// source the caller provided. If none, it falls back to the
// signing_pubkey field embedded in the bundle — convenient but
// strictly weaker (an attacker who controls the bundle controls the
// embedded key). Always prefer a key from an independent channel.
func loadPubKey(inline, file, url string, bundle audit.Bundle) (ed25519.PublicKey, error) {
	var b64 string
	switch {
	case inline != "":
		b64 = inline
	case file != "":
		raw, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				b64 = line
				break
			}
		}
	case url != "":
		resp, err := http.Get(url)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		var doc struct {
			PublicKeyB64 string `json:"public_key_b64"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
			return nil, fmt.Errorf("decode pubkey response: %w", err)
		}
		b64 = doc.PublicKeyB64
	case bundle.SigningPubkey != "":
		// Fallback only — and a printed warning so the user knows we're
		// trusting the very thing we're verifying.
		fmt.Fprintln(os.Stderr, "WARN: no --pubkey source given; falling back to signing_pubkey embedded in the bundle (anyone who forged the bundle can forge this too).")
		b64 = bundle.SigningPubkey
	default:
		return nil, fmt.Errorf("no public key provided (use --pubkey-b64 / --pubkey-file / --pubkey-url)")
	}

	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("decode base64: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("expected %d pubkey bytes, got %d", ed25519.PublicKeySize, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}

func shortHash(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:8] + "…" + s[len(s)-8:]
}
