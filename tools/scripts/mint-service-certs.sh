#!/usr/bin/env bash
# =============================================================================
# mint-service-certs.sh — issue per-service mTLS certs from a one-shot dev CA.
#
# Generates a CA on first run, stashes it under .cache/dev-ca/, then
# mints a leaf cert + key for every service named in $SERVICES.
#
# Defaults to all services we run on the host today. Override via env:
#   SERVICES="identity audit" tools/scripts/mint-service-certs.sh
#
# Re-running is idempotent: the CA is reused, and a leaf cert is only
# minted if missing OR `--force` is passed. To rotate a leaf, delete
# its files under .cache/certs/<service>/ and re-run.
#
# Production path: replace this script with SPIFFE/SPIRE workload
# identity, an Istio/Linkerd auto-cert sidecar, or cert-manager +
# Vault PKI. The certificate file paths the services consume
# (GUVA_TLS_CERT / GUVA_TLS_KEY / GUVA_TLS_CA) remain the same.
# =============================================================================
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "${REPO_ROOT}"

CA_DIR=".cache/dev-ca"
CERT_DIR=".cache/certs"
mkdir -p "${CA_DIR}" "${CERT_DIR}"

SERVICES="${SERVICES:-identity audit apisix-adapter reference apisix}"
FORCE="${FORCE:-false}"
[[ "${1:-}" == "--force" ]] && FORCE=true

# ---- 1. dev CA ------------------------------------------------------------
if [[ ! -f "${CA_DIR}/ca.crt" ]]; then
  echo "==> minting dev CA (${CA_DIR})"
  openssl genrsa -out "${CA_DIR}/ca.key" 4096 2>/dev/null
  openssl req -x509 -new -nodes -sha256 -days 3650 \
    -key "${CA_DIR}/ca.key" \
    -out "${CA_DIR}/ca.crt" \
    -subj "/O=GUVA Development/CN=GUVA Dev Service CA"
  chmod 600 "${CA_DIR}/ca.key"
  echo "    CA fingerprint: $(openssl x509 -in "${CA_DIR}/ca.crt" -noout -fingerprint -sha256 | sed 's/^/    /')"
else
  echo "==> dev CA already present (${CA_DIR}/ca.crt) — reusing"
fi

# ---- 2. per-service leaf certs -------------------------------------------
mint_leaf() {
  local service="$1"
  local out="${CERT_DIR}/${service}"
  mkdir -p "${out}"
  if [[ -f "${out}/cert.pem" && "${FORCE}" != "true" ]]; then
    echo "  ✓ ${service} (existing — pass --force to rotate)"
    return
  fi

  # SAN list: the service's container DNS name, the host bind name,
  # and the host.docker.internal alias the APISIX container uses to
  # reach the host process. Add others if the call topology changes.
  local sans
  case "${service}" in
    identity)        sans="DNS:identity, DNS:guva-identity, DNS:localhost, DNS:host.docker.internal" ;;
    audit)           sans="DNS:audit, DNS:guva-audit, DNS:localhost, DNS:host.docker.internal" ;;
    apisix-adapter)  sans="DNS:apisix-adapter, DNS:guva-apisix-adapter, DNS:localhost, DNS:host.docker.internal" ;;
    reference)       sans="DNS:reference, DNS:guva-reference, DNS:localhost, DNS:host.docker.internal" ;;
    apisix)          sans="DNS:apisix, DNS:guva-apisix, DNS:localhost" ;;
    *)               sans="DNS:${service}, DNS:localhost" ;;
  esac

  openssl genrsa -out "${out}/key.pem" 2048 2>/dev/null
  openssl req -new -key "${out}/key.pem" -out "${out}/cert.csr" \
    -subj "/O=GUVA Development/OU=workload/CN=${service}" 2>/dev/null
  openssl x509 -req \
    -in "${out}/cert.csr" \
    -CA "${CA_DIR}/ca.crt" -CAkey "${CA_DIR}/ca.key" \
    -CAcreateserial \
    -days 365 -sha256 \
    -out "${out}/cert.pem" \
    -extfile <(printf "subjectAltName=%s\nextendedKeyUsage=serverAuth,clientAuth" "${sans}") \
    2>/dev/null
  rm -f "${out}/cert.csr"
  cp "${CA_DIR}/ca.crt" "${out}/ca.pem"
  chmod 600 "${out}/key.pem"
  echo "  + ${service}: ${out}/{cert,key,ca}.pem  (SANs: ${sans})"
}

echo "==> minting leaf certs for: ${SERVICES}"
for service in ${SERVICES}; do
  mint_leaf "${service}"
done

echo "==> done. To consume, set on the service:"
echo "      GUVA_TLS_CERT=${REPO_ROOT}/${CERT_DIR}/<service>/cert.pem"
echo "      GUVA_TLS_KEY=${REPO_ROOT}/${CERT_DIR}/<service>/key.pem"
echo "      GUVA_TLS_CA=${REPO_ROOT}/${CERT_DIR}/<service>/ca.pem"
