# Configuration

| Variable | Meaning |
| --- | --- |
| `DATABASE_URL` | Required PostgreSQL connection URL. Use TLS for remote databases. |
| `TING_CREDENTIALS_ENCRYPTION_KEY` | Base64 encoding of exactly 32 random bytes. Mutually exclusive with `_FILE`. |
| `TING_CREDENTIALS_ENCRYPTION_KEY_FILE` | Path to a file containing that base64 key. Preferred for persistent deployments. |
| `TING_BIND_ADDRESS` | Defaults to `127.0.0.1:8080`. Compose uses `0.0.0.0:8080` inside the container and binds the host port to localhost. |
| `TING_ALLOW_LOCAL_HTTP` | Defaults to `false`. Development-only opt-in for private Docker bridge peers addressing localhost. Compose enables it with its host port bound to loopback. |
| `TING_TRUSTED_PROXIES` | Comma-separated CIDRs of proxies permitted to assert `X-Forwarded-Proto: https`. Empty by default. |
| `TING_TEST_DATABASE_URL` | Test-only PostgreSQL URI. Integration tests create isolated schemas. |
| `TING_ADMIN_ENABLED` | Defaults to `true`. Set `false` to disable admin HTTP endpoints; CLI operator commands remain available. |
| `TING_ADMIN_ORIGIN` | Exact browser-facing HTTPS origin, default `https://localhost:8080`. No trailing slash or path. |
| `TING_ADMIN_SESSION_TTL` | Absolute session lifetime, default `8h`, range `5m`–`24h`. |
| `TING_WORKER_CONCURRENCY` | Worker slots per process, default `4`, range 1–64. |
| `TING_WORKER_POLL_INTERVAL` | Polling and maintenance interval, default `1s`. |
| `TING_DELIVERY_LEASE_DURATION` | Recoverable ownership duration, default `1m`. |
| `TING_DELIVERY_ATTEMPT_TIMEOUT` | Maximum provider call duration, default `15s`. |
| `TING_DELIVERY_MAX_ATTEMPTS` | Maximum acquired attempts per delivery, default `8`, range 1–100. |
| `TING_DELIVERY_RETRY_BASE` | Initial exponential retry interval, default `1s`. |
| `TING_DELIVERY_RETRY_MAX` | Maximum exponential interval before jitter, default `5m`. Provider `Retry-After` can be longer. |
| `TING_DELIVERY_TTL` | Deadline from acceptance for new deliveries, default `24h`, maximum `720h`. Persisted per delivery. |
| `TING_SHUTDOWN_GRACE` | Shared HTTP and worker drain window, default `15s`, maximum `1m`. |
| `TING_QUEUE_MAX_PENDING` | Queued deliveries across the instance, default `100000`, range 1–1000000. |
| `TING_QUEUE_MAX_PENDING_PER_APP` | Queued deliveries per App, default `10000`, at most the instance capacity. |
| `TING_MAX_TARGETS_PER_SEND` | Installations per notification, default `1000`, maximum `10000` and no greater than per-App capacity. |
| `TING_APP_SENDS_PER_MINUTE` | Persistent accepted-send budget per App, default `120`, range 1–100000. |
| `TING_REQUESTS_PER_MINUTE` | Non-health requests per direct peer per process, default `120`, range 1–100000. |
| `TING_RATE_LIMIT_MAX_PEERS` | Bounded peer map size, default `10000`, range 1–100000. |
| `TING_MAX_CONCURRENT_REQUESTS` | Active non-health requests per process, default `64`, range 1–1000. |
| `TING_MAX_REQUEST_BYTES` | Body limit, default `1048576`, range 4096–1048576. Stricter endpoint limits still apply. |
| `TING_REQUEST_TIMEOUT` | Body-read and handler deadline, default `10s`, range `1s`–`30s`. |
| `TING_RETENTION` | Minimum age after the last terminal delivery update before notification deletion, default `168h`, range `1h`–`8760h`. |
| `TING_RETENTION_INTERVAL` | Cleanup cadence, default `1m`, range `1s`–`1h`. |
| `TING_RETENTION_BATCH_SIZE` | Cleanup records per table per batch, default `100`, range 1–1000. |

All implemented commands require database and encryption-key configuration, and apply embedded migrations before proceeding. Migration startup has a one-minute deadline and uses PostgreSQL advisory locking through Goose. HTTP requests use `TING_REQUEST_TIMEOUT`; shutdown allows `TING_SHUTDOWN_GRACE` for active requests and worker attempts in parallel, cancels retention cleanup, and waits before closing the shared pool. The pool is limited to ten connections.

The server uses HTTP behind an HTTPS reverse proxy. Cleartext API access is permitted only for direct loopback connections addressed to localhost or a loopback IP. Non-local API requests require TLS or a trusted peer asserting exactly `X-Forwarded-Proto: https`. Trusted proxies must overwrite that header based on the actual client connection. Do not trust arbitrary network ranges. Health probes do not require TLS and return no credentials.

Docker port forwarding changes the peer address. For local Compose development, `TING_ALLOW_LOCAL_HTTP=true` also permits private peers when the request hostname is localhost or a loopback IP. Keep the host port bound to `127.0.0.1`. Hostnames are client-controlled, so this opt-in is not a replacement for TLS or network isolation. Disable it before exposing the API beyond the local machine.

Compose currently supports local development. To serve non-local clients, terminate TLS at a proxy and explicitly configure its narrow peer CIDR. Do not expose this incomplete API publicly as a production push service.

## Compose

The default Compose file pulls the published GHCR image. Images support Linux AMD64 and ARM64.

```sh
export POSTGRES_PASSWORD="$(openssl rand -hex 24)"
export TING_CREDENTIALS_ENCRYPTION_KEY="$(openssl rand -base64 32)"
docker compose -f deploy/compose.yaml pull
docker compose -f deploy/compose.yaml up --no-build -d
```

Preserve these values if keeping the database. Changing the database password environment variable does not change an already-initialized PostgreSQL password. Changing the master key makes existing encrypted credentials unreadable.

`TING_IMAGE` selects the image, defaulting to `ghcr.io/klaraalabs/ting:edge`. Use a version or digest for a persistent deployment. For example, after a `v0.1.0` release has been published, set `TING_IMAGE=ghcr.io/klaraalabs/ting:0.1.0`. The `edge` tag follows development on the default branch.

To build the current source locally, include the build override. It uses `ting:local` and requires no published image:

```sh
docker compose -f deploy/compose.yaml -f deploy/compose.build.yaml up --build -d
```

Use the same pair of files for subsequent local-build Compose commands. All commands above run from the repository root.

The image contains one static Go binary running as a non-root user. There is no Node runtime or admin UI yet. `/admin` returns JSON `404`. API container readiness uses `/readyz`; PostgreSQL uses `pg_isready`.

Operator session endpoints are available at `/admin/api/v1/session`. They require HTTPS through a trusted reverse proxy even for local development; Compose does not provide TLS itself. Set the external `TING_ADMIN_ORIGIN` and configure the proxy to preserve Host and overwrite `X-Forwarded-Proto`. See [operator authentication](operator-auth.md) for CLI bootstrap, recovery, and cookie/CSRF requirements.

## Publishing container images

The [API workflow](../.github/workflows/api.yaml) runs its tests, PostgreSQL integration checks, image build, and startup smoke test before publishing. It publishes only on pushes to the repository's default branch or tags beginning with `v`. Pull requests and other branches run the checks without publishing.

Images are named `ghcr.io/<owner>/<repository>`, so the intended `klaraalabs/ting` repository publishes `ghcr.io/klaraalabs/ting`. Each published image contains `linux/amd64` and `linux/arm64` variants and receives these tags:

| Git reference | Image tags |
| --- | --- |
| Default branch | `edge` and `sha-<full-commit-sha>` |
| SemVer tag, such as `v0.1.0` | `0.1.0` and `sha-<full-commit-sha>` |
| Prerelease tag, such as `v0.1.0-rc.1` | `0.1.0-rc.1` and `sha-<full-commit-sha>` |

There is no automatic `latest` tag. Pin an image digest when the deployed bytes must remain fixed; tags can be republished by a workflow rerun.

Publishing authenticates with the workflow's `GITHUB_TOKEN`, scoped to `contents: read` and `packages: write` for the publishing job. No personal access token or registry secret is required. The organization must allow Actions to create packages. If an existing package was created separately, grant this repository Actions access to it.

After the first successful publish, set the container package's visibility to **Public** in GitHub's package settings. New GHCR packages default to private, even when the source repository is public. Public images can be pulled without authentication. See [GitHub's Container registry documentation](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry) for package visibility and access.

## Secret storage and recovery

Sender and installation bearer credentials are SHA-256 hashes of high-entropy random values. Identity secrets, provider keys, and native push tokens use AES-256-GCM with fresh nonces and authenticated App, purpose, and record context. The ciphertext includes a format version. Provider replacements also create a new credential version for future client-cache invalidation.

Back up PostgreSQL and the master key separately. Restore them together. The master key is never stored in PostgreSQL. Losing it makes encrypted values unrecoverable. This increment has no master-key re-encryption command; do not replace the key in place. Key rotation and automated recovery remain V1 work.

Delivery deadlines stop expired work. Retention later deletes old terminal notifications, their payloads, deliveries, and idempotency records. Queue admission and accepted-send budgets are transactional across processes. See [operational limits and retention](operations.md) for overload responses, replay behavior, cleanup exclusions, and per-process versus instance-wide limits.

Worker SQL operations have a five-second timeout. A lease must be longer than the provider attempt timeout plus two SQL timeouts, so the default attempt timeout requires a lease longer than 25 seconds. Invalid settings fail startup. Keep the container termination grace longer than `TING_SHUTDOWN_GRACE` so canceled database operations can unwind; Compose defaults to 25 seconds.

The new deadline migration assigns existing deliveries `created_at + 24 hours`. Changing `TING_DELIVERY_TTL` affects only new notifications, including when an old idempotency key is replayed. APNs and FCM are enabled in the server. Missing or unreadable App provider credentials fail that delivery with `credential_error`; they do not make the whole instance unready. Credential changes take effect before the next send without a restart. See [delivery workers](delivery.md) for retry and recovery semantics.
