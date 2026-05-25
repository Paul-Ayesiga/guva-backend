package backend

import (
	"context"
	"strings"
	"time"

	"github.com/guva-ug/guva-backend/services/integrations/nira/internal/canonical"
)

// NewSimulator returns an in-memory Backend with the same 5
// fictional citizens the original verification mock used. Suitable
// for development, integration tests, and the platform demo; never
// for production. The "upstream" Backend is the one that talks to
// the real NIRA system.
func NewSimulator() Backend {
	return &simulator{records: seedRecords()}
}

type simulator struct {
	records map[string]canonical.Record
}

func (s *simulator) Name() string { return "simulator" }

func (s *simulator) Lookup(ctx context.Context, nin string) (canonical.Record, bool, error) {
	// Mimic NIRA's ~50ms baseline so latency-sensitive callers
	// behave the same in dev as production.
	select {
	case <-time.After(50 * time.Millisecond):
	case <-ctx.Done():
		return canonical.Record{}, false, ctx.Err()
	}
	rec, ok := s.records[strings.ToUpper(strings.TrimSpace(nin))]
	if !ok {
		return canonical.Record{}, false, nil
	}
	return rec, true, nil
}

func (s *simulator) Health(_ context.Context) error { return nil }

// seedRecords mirrors what services/verification's mock had, so the
// pre-existing Bruno test suite continues to pass when verification
// switches from in-process mock to the integration service.
func seedRecords() map[string]canonical.Record {
	updated, _ := time.Parse(time.RFC3339, "2026-01-10T08:00:00Z")
	mk := func(nin, gn, mn, sn, dob, sex, mother string, status canonical.Status) canonical.Record {
		return canonical.Record{
			NIN: nin, GivenName: gn, MiddleName: mn, Surname: sn,
			DateOfBirth: dob, Sex: sex, Nationality: "Ugandan",
			MotherMaidenName: mother, Status: status, LastUpdatedAt: updated,
		}
	}
	return map[string]canonical.Record{
		"CM91051512345001": mk("CM91051512345001", "Sarah", "Nansubuga", "Nakato", "1991-05-15", "F", "Nakato", canonical.StatusActive),
		"CM85031298765002": mk("CM85031298765002", "John", "Wasswa", "Mukasa", "1985-03-12", "M", "Nansubuga", canonical.StatusActive),
		"CF95071587654003": mk("CF95071587654003", "Grace", "Akello", "Achieng", "1995-07-15", "F", "Atim", canonical.StatusActive),
		"CM72042098765004": mk("CM72042098765004", "Patrick", "Kato", "Ssali", "1972-04-20", "M", "Namutebi", canonical.StatusDeceased),
		"CM88010198765005": mk("CM88010198765005", "David", "Ojok", "Okello", "1988-01-01", "M", "Acen", canonical.StatusRevoked),
	}
}
