-- =============================================================================
-- Database-per-service bootstrap. One database per microservice, per §8.1.
-- Each database is owned by the same operator role for local development;
-- in non-local environments each service has its own least-privileged role
-- (see infra repo).
-- =============================================================================

CREATE DATABASE guva_identity;
CREATE DATABASE guva_verification;
CREATE DATABASE guva_consent;
CREATE DATABASE guva_audit;
CREATE DATABASE guva_notification;
CREATE DATABASE guva_admin;

-- Keycloak's own backing store.
CREATE DATABASE keycloak;
