# Ting

Tiny, self-hosted push notifications. See [PROJECT.md](PROJECT.md) for the V1 specification.

The API foundation is implemented with Go, `net/http`, PostgreSQL, `pgxpool`, and embedded Goose migrations. It includes App provisioning, encrypted provider configuration, identity verification, installation registration/logout, durable notification ingestion/status, and PostgreSQL delivery workers with leases, retries, deadlines, and shutdown draining. Workers send to APNs over HTTP/2 and to FCM through the official Firebase Admin Go SDK.

APNs and FCM adapters are connected to the workers. Configure each App’s provider credentials, register installations, and send notifications through the HTTP API. Adapter and worker tests use local mock providers; live-device delivery has not been verified in this repository yet. Operator authentication and the Management API are implemented, including App updates, credential management, and test sends to registered installations. Queue capacities, per-App send budgets, configurable HTTP limits, and retention cleanup are implemented. Initial Swift and Android SDKs are implemented for development integration. The dashboard, React Native/Compose packages, master-key rotation, and release verification remain pending.

## Repository layout

- `src/api/`: Go service, CLI, migrations, and package tests.
- `src/dashboard/`: planned React dashboard with its own package configuration.
- `sdks/swift/`: initial native Swift package, exposed through the root `Package.swift`, targeting iOS/iPadOS 16.4+.
- `sdks/android/`: initial native Kotlin Android library with its own Gradle wrapper, targeting API 24+, FCM forwarding, Keystore storage, and lifecycle tests. See [Android integration](sdks/android/README.md).
- `sdks/react-native/` and `sdks/compose/`: planned packages. The initial React Native SDK requires Expo SDK 57 or later in a validated compatibility range, with New Architecture enabled.
- `tests/contracts/`: shared installation lifecycle and transfer fixtures for Go and the SDKs.
- `tests/e2e/`: planned tests spanning the API and SDKs.
- `examples/`: planned consumer applications demonstrating SDK integration.
- `docs/`: API contracts and project documentation.
- `deploy/`: Docker image and Compose configuration.

Add planned directories with their implementations. Unit and integration tests stay with their application or SDK, including database tests in `src/api`. The API is one Go module; future applications and SDKs manage their dependencies independently. See [AGENTS.md](AGENTS.md) for repository conventions and [PROJECT.md](PROJECT.md#10-repository-layout) for the detailed layout.

## Run locally

Requires Go 1.26.3+ and PostgreSQL 18. Dependencies are pinned in `src/api/go.mod` and `src/api/go.sum`. Run the commands below from the repository root.

```sh
# Start only PostgreSQL. Its port is bound to localhost.
export POSTGRES_PASSWORD="$(openssl rand -hex 24)"
docker compose -f deploy/compose.yaml up -d postgres
export DATABASE_URL="postgres://ting:${POSTGRES_PASSWORD}@127.0.0.1:5432/ting?sslmode=disable"

# Generate once, keep outside the repository, and back up securely.
umask 077
openssl rand -base64 32 > /tmp/ting-encryption-key
export TING_CREDENTIALS_ENCRYPTION_KEY_FILE=/tmp/ting-encryption-key

go -C src/api build -o ../../bin/ting ./cmd/ting
bin/ting db migrate
bin/ting app create --name "Example Development" --slug example-dev
bin/ting serve
```

The development server binds to `127.0.0.1:8080`. `GET /healthz` checks liveness; `GET /readyz` checks database connectivity after successful migrations. The temporary key path above is for local development. Use a durable, backed-up secret file for a persistent deployment.

For containers, `deploy/compose.yaml` pulls `ghcr.io/klaraalabs/ting:edge` by default. Set `TING_IMAGE` to select a published version or digest. To build from this checkout, also pass `-f deploy/compose.build.yaml`. The [container instructions](docs/configuration.md#compose) cover both modes, and [publishing instructions](docs/configuration.md#publishing-container-images) describe GitHub Actions, image tags, and the first-publish visibility setting.

[API and CLI instructions](docs/api.md) cover provisioning, registration, curl sending, and status. [OpenAPI](docs/openapi.yaml) describes the implemented HTTP contract. [Configuration](docs/configuration.md) covers secrets, TLS, and Compose. [Delivery workers](docs/delivery.md) describes leases, retries, deadlines, and the sender contract. [Push providers](docs/providers.md) covers APNs/FCM behavior, credentials, and verification. [Operator authentication](docs/operator-auth.md) covers bootstrap, password reset, HTTPS, sessions, and CSRF. [Management API](docs/management-api.md) covers Apps, credentials, recent sends, and test notifications. [Operational limits](docs/operations.md) covers overload handling, capacity, request limits, and retention.

[SDK lifecycle requirements](docs/sdk-lifecycle.md) define durable credential recovery, ordered account changes, token-transfer supersession, and pending logout for all three SDK families. The installation API requires a pre-saved proposed credential and mutation generation; callers of the earlier development contract must update their registration/logout requests. Existing database installations migrate with generation zero. Token transfer permanently retires the previous UUID; migration 7 also fences ambiguous inactive duplicate-token records. See the lifecycle guide before handling a supersession conflict.

## Checks

The Swift package runs independently with `swift test` from the repository root. [Swift SDK instructions](sdks/swift/README.md) cover the iOS build check and integration.

The Android library runs independently from `sdks/android` with `./gradlew testDebugUnitTest lintRelease assembleRelease`. See [Android SDK instructions](sdks/android/README.md) for toolchain and local Maven consumption. Both native SDKs still require signed-consumer and real-device validation before public release.

```sh
go -C src/api test ./...
go -C src/api vet ./...
TING_TEST_DATABASE_URL="$DATABASE_URL" go -C src/api test -race ./...
```

Integration tests create and remove isolated schemas. The test database user needs schema creation permission. Without `TING_TEST_DATABASE_URL`, PostgreSQL tests skip explicitly. CI supplies PostgreSQL and runs the full suite.

## Next API work

1. Live-device APNs sandbox/production and FCM verification.
2. Dashboard in `src/dashboard` using the operator session and Management APIs.
3. Load testing and tuning of queue admission, cleanup, CPU, and memory use.
4. Master-key rotation/recovery tooling and additional SDK contract fixtures.

Credential configuration currently reports `configured_untested`. Parsing a key does not prove provider authorization or device delivery.
