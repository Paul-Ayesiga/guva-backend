# Bruno collection — GUVA Identity

A self-contained API collection for the identity service. Open it in [Bruno](https://www.usebruno.com/), point it at the running local stack, and exercise every endpoint.

## Prereqs

1. The platform stack is running:

   ```bash
   make up                      # from guva-backend/ root
   make run-identity            # in another terminal
   ```

2. Caddy's local root CA is trusted by your machine:

   ```bash
   make trust-ca                # one-time per machine
   ```

   Without this, request `00 Get Token` will fail with a TLS verification error because it hits `https://auth.guva.localhost`.

## Import into Bruno

1. **Bruno → Open Collection**
2. Point it at the `services/identity/bruno/` directory.
3. Pick the `Local` environment in the top-right dropdown.

The collection appears with a single `Identity` folder containing seven requests, numbered in run order.

## Run the requests in order

| # | Request | What it does |
|---|---|---|
| 00 | Get Token | Fetches a client-credentials access token from Keycloak; stashes it in `{{accessToken}}`. |
| 01 | Healthz | Liveness check against the service directly (`:7071`). |
| 02 | Readyz | Readiness check. |
| 03 | List Scopes | `GET /v1/identity/scopes` — confirms the gateway → service auth path. |
| 04 | Create Consumer | `POST /v1/identity/consumers` — creates a Keycloak client + records the registration. Stashes `consumerId`, `newClientId`, and `generatedClientSecret`. |
| 05 | Get Consumer | `GET /v1/identity/consumers/{{consumerId}}` — reads it back. Confirms `generated_client_secret` is **not** returned. |
| 06 | Verify New Client Can Issue Tokens | Uses the secret from request 04 to fetch a token for the newly-created client. End-to-end proof that identity → Keycloak provisioning works. |

Run them in order the first time (00 → 06). After that, requests 01–05 can be re-run in any order.

## Environment variables

Set in `environments/Local.bru`:

| Var | Meaning |
|---|---|
| `authBaseUrl` | Where to fetch tokens (`https://auth.guva.localhost`) |
| `apiBaseUrl`  | Identity routes through APISIX (`http://localhost:8000/v1/identity`) |
| `directBaseUrl` | Identity directly (`http://localhost:7071`) — used for probes that aren't gateway-routed |
| `realm`       | Keycloak realm (`guva`) |
| `clientId` / `clientSecret` | The `guva-reference` dev client used for fetching the operator token |
| `accessToken` | Populated by request 00 |
| `consumerId` / `newClientId` / `generatedClientSecret` | Populated by request 04 |

## Tests

Each request has assertions in the `tests` block. Bruno runs them automatically and shows pass/fail in the right-hand panel.

## Switching environments

Create additional environment files under `environments/` (e.g. `Staging.bru`) with the same variable keys, point at staging URLs, and switch in the dropdown. The collection itself doesn't change.
