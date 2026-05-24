# Bruno collection — GUVA Audit

Open in [Bruno](https://www.usebruno.com/), pick the `Local` environment, run the requests in order.

## Prereqs

```bash
make up                 # full stack including audit + identity
make run-audit          # in one terminal
make run-identity       # in another (so events get emitted)
make trust-ca           # one-time per machine for the auth URL
```

## Requests

| # | Request | What it does |
|---|---|---|
| 00 | Get Token | Fetches a bearer token; stashes into `{{accessToken}}`. |
| 01 | Healthz | Liveness against direct port. |
| 02 | List Entries (recent) | Pulls the latest 20 entries; stashes `{{lastCursor}}`. |
| 03 | List Entries (paginate) | Uses `{{lastCursor}}` to fetch the next page. |
| 04 | Filter by action | Returns only `identity.consumer.created` events. |
| 05 | Verify Chain | End-to-end hash-chain integrity check. |

## Generating events

Open the **identity** Bruno collection in another tab and run its
`04 Create Consumer` request. Within a few seconds the new event appears in this collection's `02 List Entries` response.
