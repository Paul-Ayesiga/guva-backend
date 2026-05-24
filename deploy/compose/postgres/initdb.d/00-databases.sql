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
-- Holds the apisix-adapter's audit_outbox; the adapter ingests access
-- logs from the gateway and stages CloudEvents for the audit chain.
-- Name mirrors the service directory with hyphens mapped to underscores
-- (db-migrate.sh does the same).
CREATE DATABASE guva_apisix_adapter;
-- Holds the webhooks service: subscription registry + delivery audit
-- trail. Delivery jobs flow through RabbitMQ; this DB is the source of
-- truth for "did this consumer get notified about that event".
CREATE DATABASE guva_webhooks;

-- Keycloak's own backing store.
CREATE DATABASE keycloak;
