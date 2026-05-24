-- consumer_registrations: tracks the platform's record of who's
-- onboarded onto the API. The Keycloak realm is the source of truth
-- for client credentials and scopes; this table is the audit/intent
-- record kept on the identity service's side.
--
-- The keycloak_client_id is the linkage key — it matches the clientId
-- in the Keycloak realm. When we add the admin-API wiring to create
-- the client, this field is what we'll thread through.

CREATE TABLE consumer_registrations (
    id                  UUID PRIMARY KEY,
    agency_name         VARCHAR(255) NOT NULL,
    contact_email       VARCHAR(255) NOT NULL,
    keycloak_client_id  VARCHAR(255) NOT NULL UNIQUE,
    scopes              TEXT[] NOT NULL DEFAULT '{}',
    status              VARCHAR(32) NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'active', 'suspended', 'revoked')),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_consumer_registrations_agency ON consumer_registrations(agency_name);
CREATE INDEX idx_consumer_registrations_status ON consumer_registrations(status);
