// Package nira holds the NIRA adapter — the bridge between the
// canonical citizen model and Uganda's National Identification &
// Registration Authority. The real adapter would speak NIRA's REST or
// SOAP API behind a TLS-mutual-auth channel; this dev version returns
// canned responses for a small set of test NINs.
//
// The Adapter interface is what the HTTP layer depends on, so swapping
// in a real client later is a one-line wiring change.
package nira

import (
	"context"
	"errors"
	"strings"
	"time"
)

// Status mirrors what NIRA reports about a record. Maps onto
// canonical.Status at the HTTP layer.
type Status string

const (
	StatusActive   Status = "active"
	StatusDeceased Status = "deceased"
	StatusRevoked  Status = "revoked"
)

// Record is the canonical NIRA-side shape — what their endpoint
// returns. The adapter is responsible for translating this into the
// per-attribute match summary we ship in the canonical response.
type Record struct {
	NIN              string
	GivenName        string
	MiddleName       string
	Surname          string
	DateOfBirth      string // YYYY-MM-DD
	Sex              string // "M" | "F"
	Nationality      string
	MotherMaidenName string
	Status           Status
	LastUpdatedAt    time.Time // freshness of the underlying record
}

// Adapter is the surface area the verification HTTP layer depends on.
// Errors are returned only on genuine adapter / transport failure;
// "no such NIN" is not an error, it returns (Record{}, false, nil).
type Adapter interface {
	Lookup(ctx context.Context, nin string) (rec Record, found bool, err error)
}

// ErrUpstreamUnavailable signals the upstream itself couldn't answer.
// HTTP layer maps this to StatusError + 503 to the caller.
var ErrUpstreamUnavailable = errors.New("NIRA upstream unavailable")

// NewMock returns an Adapter backed by a small in-memory test data
// set. Useful for dev, integration tests, and the platform demo. The
// "live" adapter (against the real NIRA endpoint) ships separately
// once the integration agreement is in place.
func NewMock() Adapter {
	return &mockAdapter{records: seedRecords()}
}

type mockAdapter struct {
	records map[string]Record // keyed by NIN
}

func (m *mockAdapter) Lookup(ctx context.Context, nin string) (Record, bool, error) {
	// Real NIRA imposes a ~200 ms baseline; mimic for realism.
	select {
	case <-time.After(50 * time.Millisecond):
	case <-ctx.Done():
		return Record{}, false, ctx.Err()
	}
	rec, ok := m.records[strings.ToUpper(strings.TrimSpace(nin))]
	if !ok {
		return Record{}, false, nil
	}
	return rec, true, nil
}

// seedRecords is the curated test data set. Five NINs spanning every
// status enum so a Bruno run-through exercises the whole response
// space. The format reflects NIRA's convention: a single letter (C =
// Citizen), a letter for sex (M / F), 6-digit YYMMDD, then a numeric
// sequence.
//
// Test set:
//
//	CM91051512345001  Sarah Nakato      F  1991-05-15  active
//	CM85031298765002  John Mukasa       M  1985-03-12  active
//	CF95071587654003  Grace Achieng     F  1995-07-15  active
//	CM72042098765004  Patrick Ssali     M  1972-04-20  deceased
//	CM88010198765005  David Okello      M  1988-01-01  revoked
//
// These are FICTIONAL. They do not correspond to real Ugandans and
// are not valid against the real NIRA system.
func seedRecords() map[string]Record {
	mk := func(nin, gn, mn, sn, dob, sex string, status Status, mother string) Record {
		t, _ := time.Parse(time.RFC3339, "2026-01-10T08:00:00Z")
		return Record{
			NIN: nin, GivenName: gn, MiddleName: mn, Surname: sn,
			DateOfBirth: dob, Sex: sex, Nationality: "Ugandan",
			MotherMaidenName: mother,
			Status:           status, LastUpdatedAt: t,
		}
	}
	return map[string]Record{
		"CM91051512345001": mk("CM91051512345001", "Sarah", "Nansubuga", "Nakato", "1991-05-15", "F", StatusActive, "Nakato"),
		"CM85031298765002": mk("CM85031298765002", "John", "Wasswa", "Mukasa", "1985-03-12", "M", StatusActive, "Nansubuga"),
		"CF95071587654003": mk("CF95071587654003", "Grace", "Akello", "Achieng", "1995-07-15", "F", StatusActive, "Atim"),
		"CM72042098765004": mk("CM72042098765004", "Patrick", "Kato", "Ssali", "1972-04-20", "M", StatusDeceased, "Namutebi"),
		"CM88010198765005": mk("CM88010198765005", "David", "Ojok", "Okello", "1988-01-01", "M", StatusRevoked, "Acen"),
	}
}
