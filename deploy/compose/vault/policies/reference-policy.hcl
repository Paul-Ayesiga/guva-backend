# Reference-service policy: read-only on its own KV path.
# Loaded by tools/scripts/bootstrap.sh after Vault starts.

path "secret/data/services/reference/*" {
  capabilities = ["read"]
}

path "secret/metadata/services/reference/*" {
  capabilities = ["list"]
}
