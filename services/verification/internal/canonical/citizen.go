// Package canonical defines the platform's universal verification
// response shape. Every agency adapter (NIRA, URA, URSB, Lands, UNEB,
// MoH) renders its native data into these structs; the HTTP layer
// returns them unchanged so consumers code against ONE schema.
//
// Design principle: minimum disclosure. The response carries no
// citizen attributes the caller didn't claim. For each claimed
// attribute we return only `match: true|false`. The actual value
// (e.g. the real surname when the caller's claim was wrong) is
// withheld unless the caller has a separate scope authorising
// retrieval of corrected data. This contract is the difference
// between "verification" (yes/no) and "lookup" (give me everything).
package canonical

import "time"

// Status is the verification outcome enum.
type Status string

const (
	StatusVerified       Status = "verified"        // every claimed attribute matched
	StatusMismatch       Status = "mismatch"        // record found, at least one attribute did not match
	StatusNotFound       Status = "not_found"       // upstream has no record for the subject
	StatusDeceased       Status = "deceased"        // upstream record marked as deceased
	StatusRevoked        Status = "revoked"         // upstream record cancelled or revoked
	StatusConsentInvalid Status = "consent_invalid" // consent reference unrecognised or expired
	StatusError          Status = "error"           // adapter-side or upstream-side failure
)

// SubjectIdentifier names what kind of identifier we're verifying
// against. NIN for citizens (NIRA); other types will be added by
// future endpoints (passport for visa verification, BRN for businesses
// via URSB, etc).
type SubjectIdentifier struct {
	Type  string `json:"type"`  // "nin", "passport", "brn", ...
	Value string `json:"value"` // raw value the caller supplied
}

// AttributeMatch is the per-claim verification result. `Match` is the
// load-bearing field; the others are advisory.
type AttributeMatch struct {
	Match  bool   `json:"match"`
	Source string `json:"source"`         // "NIRA", "URSB", ...
	Note   string `json:"note,omitempty"` // optional adapter-side hint, e.g. "case-insensitive"
}

// CitizenAttributes is the catalogue of fields a verify-citizen
// request may claim. Each field is optional; absent fields are not
// checked. The response mirrors the same key set with AttributeMatch
// values for whichever fields the caller provided.
type CitizenAttributes struct {
	NIN              string `json:"nin,omitempty"`
	GivenName        string `json:"given_name,omitempty"`
	MiddleName       string `json:"middle_name,omitempty"`
	Surname          string `json:"surname,omitempty"`
	DateOfBirth      string `json:"date_of_birth,omitempty"` // ISO 8601 YYYY-MM-DD
	Sex              string `json:"sex,omitempty"`           // "M" | "F"
	Nationality      string `json:"nationality,omitempty"`
	MotherMaidenName string `json:"mother_maiden_name,omitempty"`
}

// VerificationResponse is the canonical response shape. Subject
// identifier is echoed back as supplied (so the caller doesn't need
// to correlate by request id); attributes carries the per-field match
// results; metadata describes the run.
type VerificationResponse struct {
	VerificationID string                    `json:"verification_id"`
	Consumer       string                    `json:"consumer"`
	Subject        SubjectIdentifier         `json:"subject"`
	CheckedAt      time.Time                 `json:"checked_at"`
	Status         Status                    `json:"status"`
	Attributes     map[string]AttributeMatch `json:"attributes"`
	Metadata       Metadata                  `json:"metadata"`
}

// Metadata describes the verification run: who said it was OK, how
// fresh the data is, which adapter handled it.
type Metadata struct {
	ConsentReference  string    `json:"consent_reference,omitempty"`
	DataFreshness     time.Time `json:"data_freshness"` // when the upstream record was last updated (best effort)
	Source            string    `json:"source"`         // "NIRA" / "URSB" / ...
	UpstreamLatencyMS int64     `json:"upstream_latency_ms"`
	CorrelationID     string    `json:"correlation_id,omitempty"`
}
