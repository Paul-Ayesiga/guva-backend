// Package canonical defines the NIRA-canonical record shape — what
// the integration service returns to callers (today: verification).
// Independent of NIRA's wire format AND independent of the platform's
// citizen canonical model. This is the agency-adapter contract: one
// shape per agency, one translation step on each end.
//
// Future agencies (URSB, URA, Lands, UNEB, MoH) define their own
// canonical record in their own integration service. The platform's
// verification.canonical model knows how to consume each agency's
// canonical via per-agency translators living in the verification
// service.
package canonical

import "time"

// Status mirrors NIRA's state enum. Mapped onto verification's
// canonical status at the verification layer.
type Status string

const (
	StatusActive   Status = "active"
	StatusDeceased Status = "deceased"
	StatusRevoked  Status = "revoked"
)

// Record is the NIRA-canonical citizen record. The integration
// service is the only place that knows how NIRA's wire format maps
// onto this shape; consumers code against this struct.
type Record struct {
	NIN              string    `json:"nin"`
	GivenName        string    `json:"given_name"`
	MiddleName       string    `json:"middle_name"`
	Surname          string    `json:"surname"`
	DateOfBirth      string    `json:"date_of_birth"` // YYYY-MM-DD
	Sex              string    `json:"sex"`           // "M" | "F"
	Nationality      string    `json:"nationality"`
	MotherMaidenName string    `json:"mother_maiden_name"`
	Status           Status    `json:"status"`
	LastUpdatedAt    time.Time `json:"last_updated_at"`
}

// LookupResponse is the wire shape /lookup returns. `found` makes
// the absence case explicit; on `found: false` Record is zero-valued.
type LookupResponse struct {
	LookupID string `json:"lookup_id"`
	Found    bool   `json:"found"`
	Record   Record `json:"record"`
}
