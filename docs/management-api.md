# Management API

The Management API is available under `/admin/api/v1`. It uses [operator sessions](operator-auth.md), not App API tokens. All operators have instance-wide access; each nested route still scopes its queries to the App in the URL. There is no dashboard yet.

Requests require HTTPS at `TING_ADMIN_ORIGIN`. Send the `__Host-ting_session` cookie issued by login. Every mutation also requires the exact `Origin` and the session's `X-Ting-CSRF` token. JSON bodies use `Content-Type: application/json`. Bearer authorization headers are rejected. Responses are never cacheable. `TING_ADMIN_ENABLED=false` disables both session and management endpoints.

## Routes

Paths below are relative to `/admin/api/v1`.

| Method | Path | Result |
| --- | --- | --- |
| GET, POST | `/apps` | List Apps or create an App. |
| GET, PATCH | `/apps/{appId}` | Read, rename, enable, or disable an App. |
| GET, POST | `/apps/{appId}/tokens` | List token metadata or issue a new sender token. |
| DELETE | `/apps/{appId}/tokens/{tokenId}` | Revoke a sender token. |
| GET, POST | `/apps/{appId}/identity-secrets` | List identity-key metadata or issue a new secret. |
| DELETE | `/apps/{appId}/identity-secrets/{kid}` | Retire an identity key. |
| GET, PUT, DELETE | `/apps/{appId}/apns` | Read metadata, replace, or remove APNs configuration. |
| GET, PUT, DELETE | `/apps/{appId}/fcm` | Read metadata, replace, or remove FCM configuration. |
| GET | `/apps/{appId}/installations` | List installation metadata for selecting a test target. |
| POST | `/apps/{appId}/test-notifications` | Queue a fixed test alert to one registered installation. |
| GET | `/apps/{appId}/notifications` | List recent sends with outcome counts and safe error categories. |
| GET | `/apps/{appId}/notifications/{notificationId}` | Inspect a send, including a test send. |

List routes accept `limit` from 1 to 100, default 50, and `offset` from 0 to 100000, default 0. They return `{"items": [...], "nextOffset": 50}`; `nextOffset` is null on the last available page. Results are ordered newest first, using the ID as a tie-breaker. Installations use last-seen time. These are live lists, so inserts can shift offset pages. APNs and FCM configuration reads use the fixed shapes described below instead of pagination.

## Apps and credentials

Create an App with `POST /apps`:

```json
{"name":"Example Production","slug":"example-production"}
```

The slug is optional. The response is 201 with `appId`, `name`, optional `slug`, `enabled`, and `createdAt`. No sender token or identity secret is created implicitly. A duplicate slug returns `409 slug_conflict`.

`PATCH /apps/{appId}` accepts `name`, `enabled`, or both:

```json
{"name":"Example Production","enabled":false}
```

Disabling atomically marks queued deliveries as skipped with `app_disabled` and clears their leases. Public authentication and new test sends stop. Re-enabling permits new work without resurrecting canceled deliveries. A provider request already in flight may still reach the device; disabling cannot recall it. Operators can inspect disabled Apps and their notification outcomes.

Issue a sender token with `POST /apps/{appId}/tokens` and `{"label":"production backend"}`. The 201 response contains `tokenId`, `token`, and `suffix`. The raw token appears only in that response. Token listings contain `tokenId`, `label`, `suffix`, `createdAt`, `lastUsedAt`, and `revokedAt`.

Issue an identity secret with `POST /apps/{appId}/identity-secrets` and `{}`. The 201 response contains `kid` and `secret`. Existing keys remain valid until explicitly retired, allowing overlap while consumers deploy the replacement. Listings contain only `kid`, `createdAt`, and `retiredAt`. Wait for old identity JWTs to expire before retiring their key unless revocation is intentional.

DELETE returns 204 for an existing token/key, including repeated revocation. An unknown or other-App token/key returns 404. GET never returns raw tokens, signing secrets, hashes, or ciphertext. Issuance requests are not idempotent; if a response is lost, inspect metadata, revoke the lost issuance, and create a replacement.

## Provider configuration

`PUT /apps/{appId}/apns` accepts:

```json
{
  "environment":"both",
  "bundleId":"com.example.app",
  "teamId":"ABCDE12345",
  "keyId":"FGHIJ67890",
  "privateKey":"<PKCS#8 P-256 PEM contents>"
}
```

`environment` is `sandbox`, `production`, or `both`. Updating both environments is atomic. The PEM field is at most 64 KiB. GET returns `status` and a `credentials` array containing environment, bundle ID, Team ID, Key ID, and update time. No configuration returns `{"status":"not_configured","credentials":[]}`. DELETE requires an explicit `?environment=sandbox`, `production`, or `both` and returns 204, including when that environment is already absent.

`PUT /apps/{appId}/fcm` accepts:

```json
{
  "projectId":"example-project",
  "serviceAccount": {
    "type":"service_account",
    "project_id":"example-project",
    "client_email":"ting@example-project.iam.gserviceaccount.com",
    "private_key":"<PKCS#8 RSA PEM contents>",
    "private_key_id":"<Google key ID>",
    "token_uri":"https://oauth2.googleapis.com/token"
  }
}
```

`serviceAccount` is a JSON object, not a JSON string or a client `google-services.json` file. Its raw JSON is at most 64 KiB. GET returns `status` and `credential` containing `projectId`, `clientEmail`, and `updatedAt`. No configuration returns `{"status":"not_configured","credential":null}`. DELETE removes the configuration and returns 204, including when already absent.

Provider request bodies are limited to 512 KiB to allow JSON escaping of the credential fields. Uploads use JSON; multipart uploads are not implemented. Both PUT operations return `{"status":"configured_untested"}`. Parsing and storing a credential does not prove provider authorization. Stored keys are encrypted, never returned by reads, and replacements take effect before the next provider attempt. An already started attempt may finish using the previous credential. See [provider behavior](providers.md).

## Installations and test sends

Installation listings expose `installationId`, `platform`, optional `apnsEnvironment`, `active`, `createdAt`, `lastSeenAt`, `tokenUpdatedAt`, and `invalidatedAt`. They omit external user IDs, push tokens, installation credentials, and encrypted values.

Send `POST /apps/{appId}/test-notifications` with:

```json
{"installationId":"<previously registered installation UUID>"}
```

The server validates that the installation belongs to this App, is active, and has its platform/environment configured. Unknown or other-App IDs return 404; inactive installations, disabled Apps, and missing provider configuration return 409. Arbitrary push tokens, recipient lists, and custom alert fields are rejected. Failed validation commits no notification or delivery rows.

The fixed alert has title `Ting test` and body `Test notification from Ting.`. The 202 response is `{"id":"<notification UUID>","status":"queued"}`. It uses the normal durable queue, retries, expiry, and installation-revision checks. It selects only the requested installation, even if the same user has other devices.

An optional `Idempotency-Key` prevents duplicate test jobs. Keys share the App's namespace with public sends. Reusing one with another installation or a public send returns `409 idempotency_conflict`. Retrying the same accepted test returns its original acknowledgment, even if the installation has since become inactive. It does not enqueue another delivery.

Test sends share the public API's queue capacity and per-App accepted-send budget. Capacity/budget rejection returns 429 or 503 with `Retry-After`; an accepted replay consumes no additional capacity or send budget. Old terminal notifications and their idempotency keys are deleted according to [retention settings](operations.md), after which a key can create a new send.

Poll `GET /apps/{appId}/notifications/{id}` for `queued`, `accepted`, `failed`, and `skipped` counts plus safe error categories. The recent list adds `createdAt` and `source`, either `api` or `operator_test`, without notification payloads or recipient identities. Provider acceptance is evidence that the provider accepted the request; it does not confirm display on the device. Provider configuration reads remain `configured_untested` because they do not associate a past send with the currently uploaded credential version.

## Errors and verification

Errors use the common JSON envelope. Typical statuses are 400 for validation, 401 for missing/expired sessions or bearer-token misuse, 403 for origin/CSRF failure, 404 for missing or other-App records, 409 for conflicts, 413 for body limits, and 415 for unsupported content types. Supported paths return JSON 405 for unsupported methods after authorization. Existing request rate limits apply.

The PostgreSQL integration tests exercise App updates, token/key rotation and revocation, redacted provider reads, upload validation, pagination, App isolation, authorization on every route, test-send idempotency, worker delivery to one installation, and cancellation when an App is disabled. Tests use a mocked sender and do not contact Apple or Google.
