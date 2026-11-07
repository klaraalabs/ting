# Operational limits and retention

`ting serve` applies bounded queue admission, a persistent per-App send budget, configurable HTTP limits, and periodic retention cleanup. Defaults suit a small instance; throughput has not yet been benchmarked. These settings are available through environment variables and Compose.

## Queue admission

| Variable | Default | Range |
| --- | --- | --- |
| `TING_QUEUE_MAX_PENDING` | `100000` | 1 to 1,000,000 queued deliveries across the instance. |
| `TING_QUEUE_MAX_PENDING_PER_APP` | `10000` | 1 to the instance capacity. |
| `TING_MAX_TARGETS_PER_SEND` | `1000` | 1 to 10,000, no greater than the per-App capacity. |
| `TING_APP_SENDS_PER_MINUTE` | `120` | 1 to 100,000 newly accepted notifications per App per minute. |

A delivery counts toward capacity while its state is `queued`, including leased, retrying, and expired work awaiting the worker's expiry sweep. Terminal states release capacity. The per-send target limit counts installations, not user IDs: one user with many devices can exceed it. The separate public limit of 1,000 recipient user IDs remains unchanged.

Admission takes a shared PostgreSQL row lock before checking the App and queue. Every API process uses that lock, so simultaneous requests cannot independently consume the same available capacity. Existing workers can finish deliveries while admission runs. Configure all processes sharing the database with the same queue and send-budget values.

Both public sends and operator test sends use the same admission path. A successful transaction stores the notification, all eligible delivery targets, and its App budget charge together. A rejected request stores none of them and reserves no idempotency key. Fan-out is all-or-nothing. Lowering a capacity does not discard existing work; new work waits until the queue drains below the new limit.

Zero-target notifications consume one App send-budget unit but no delivery capacity. This also bounds repeated accepted sends to users with no installations. The budget uses a fixed one-minute window in PostgreSQL and survives restarts. Successful idempotent replays consume neither capacity nor send budget, including when those limits are full. Authentication, App enablement, HTTP rate limits, and request deadlines still apply to replays.

| HTTP status | Code | Meaning |
| --- | --- | --- |
| 413 | `fanout_too_large` | Eligible installation count exceeds the per-send limit. Reduce recipients or adjust configuration. |
| 429 | `app_queue_full` | The send will not fit within this App's queue capacity. |
| 503 | `queue_full` | The send will not fit within the instance's queue capacity. |
| 429 | `app_send_rate_limited` | The App has exhausted its accepted-send budget. |

Capacity and send-budget errors include `Retry-After: 60`. Retry with the same idempotency key. This is a backoff hint, not a guarantee that capacity will be available after one minute.

## HTTP limits

| Variable | Default | Range |
| --- | --- | --- |
| `TING_REQUESTS_PER_MINUTE` | `120` | 1 to 100,000 requests per direct peer per minute, per process. |
| `TING_RATE_LIMIT_MAX_PEERS` | `10000` | 1 to 100,000 tracked peer buckets per process. |
| `TING_MAX_CONCURRENT_REQUESTS` | `64` | 1 to 1,000 active non-health requests per process. |
| `TING_MAX_REQUEST_BYTES` | `1048576` | 4,096 to 1,048,576 bytes. |
| `TING_REQUEST_TIMEOUT` | `10s` | 1s to 30s for request-body reads and handler/database operations. |

The per-peer limit covers public and management requests. It uses the direct network peer; forwarded client-IP headers are ignored. Clients behind one proxy share its bucket. When the peer map is full, expired buckets are reclaimed; a new peer is rejected if no slot is available. These in-memory limits reset on restart and are separate from the persistent App budget and operator login throttles.

Rate-limit rejection returns `429 rate_limited` with `Retry-After: 60`. Requests arriving when every concurrency slot is occupied return `503 server_busy` with `Retry-After: 1` instead of waiting in another unbounded queue. `/healthz` and `/readyz` bypass rate, concurrency, and body limits. Queue saturation does not make a healthy process unready.

Body limits apply to known-length and streaming requests. Oversized bodies return `413 payload_too_large`, including excessive trailing data after a JSON object. Endpoint-specific limits still apply: operator login is 4 KiB, provider uploads are 512 KiB with 64 KiB credential fields, and serialized push payloads remain at most 4,096 bytes. The smallest applicable limit wins.

The request deadline covers waiting for database capacity and queue admission as well as body reads. Deadline errors return `504 request_timeout` when a response can be written. A lost response can still have an ambiguous commit outcome; use idempotency keys when retrying sends. Provider attempts retain their separate worker timeouts and delivery deadlines.

## Retention

| Variable | Default | Range |
| --- | --- | --- |
| `TING_RETENTION` | `168h` | 1h to 8760h. |
| `TING_RETENTION_INTERVAL` | `1m` | 1s to 1h. |
| `TING_RETENTION_BATCH_SIZE` | `100` | 1 to 1,000 records per table per batch. |

Cleanup runs on startup and every configured interval. It deletes a notification only when:

- The notification is older than the retention duration.
- Every delivery is terminal and its last update is older than that duration.
- No delivery has an active lease.

Notifications with no deliveries age from creation. Queued work is never deleted, even when its delivery deadline has passed; the worker first records the terminal outcome. Deleting a notification also deletes its payload, delivery records, status, and idempotency key through the existing foreign key cascade.

After deletion, status reads return 404 and the old idempotency key can create a new notification. The retention period therefore bounds deduplication history. It is not a permanent deduplication record. Changing retention applies to existing records on the next cleanup pass, while changing delivery TTL applies only to new work.

Each pass executes at most ten batches within a shared five-second SQL deadline. Batches commit separately and release the admission lock between them. Busy admission defers cleanup. Competing cleanup processes skip locked work. Retention sets the minimum age for deletion, not an exact deletion time; sustained traffic, large historical fan-outs, or a cleanup backlog can delay it. Reduce batch size if a large deletion repeatedly exceeds the SQL deadline, or shorten the interval to increase cleanup frequency.

Cleanup also removes expired/revoked operator sessions, expired login-throttle entries, and expired App send-budget windows in bounded batches. Live sessions and current budgets are preserved. App configuration, token/key metadata, and installation records are retained. In particular, inactive installations keep their credentials, mutation generations, and token-transfer supersession fences so delayed SDK requests cannot recreate an old association.

Cleanup logs safe deletion counts or a generic failure, without notification payloads or credentials. Shutdown cancels maintenance and waits for it before closing PostgreSQL connections.

## HTTP diagnostics

Every request through the API handler receives a locally generated UUID in the `X-Request-Id` response header. Incoming identifiers are ignored. Unexpected HTTP failures emit one structured `HTTP request failed` event with `requestId`, `route`, `status`, and `category`. The route is the registered pattern, such as `GET /v1/notifications/{id}`, rather than the requested URL or query string. PostgreSQL failures include a validated five-character `sqlState`; deadlines and cancellations have separate categories. Panics use a separate recovery event with the same request identifier.

Client error responses stay generic. Logs exclude raw error messages, SQL details, request bodies, cookies, bearer tokens, and provider credentials. Expected validation, authentication, conflict, and capacity responses do not emit unexpected-failure events. Use the response identifier to correlate a client failure with the server log; SQLSTATE and route identify the failure class without exposing row data.

## Verification

PostgreSQL tests exercise concurrent admission from independent service instances, atomic fan-out rejection, capacity release, persistent App budgets, accepted replay at capacity, single-device management sends, retention exclusions, cascading deletion, expired idempotency records, and background cleanup cancellation. HTTP tests cover concurrency saturation, health exemptions, body limits, slow HTTPS uploads, and database-wait deadlines. The full suite runs with the Go race detector.
