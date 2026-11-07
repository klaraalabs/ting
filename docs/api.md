# API and CLI

This document describes the implemented increment. Full V1 requirements remain in [PROJECT.md](../PROJECT.md). The API persists sends and its workers contact APNs or FCM using each App’s configured credentials.

## App provisioning

CLI commands operate locally using the same application services as the server. Possession of the database and master-key configuration is operator access. [Operator bootstrap, password reset, and session login/logout](operator-auth.md) protect the [Management API](management-api.md) for Apps, credentials, installation metadata, and test notifications.

```sh
ting app create --name "Example Production" --slug example-prod
ting app list
ting app show --id "$APP_ID"
ting app token create --id "$APP_ID" --label "backend" --output-file ./sender.json
ting app identity-secret create --id "$APP_ID" --output-file ./identity.json
```

`ting help` and `ting app help` list commands, and `ting app <action> --help` lists that action's flags with an example. Help and flag errors work without database configuration. Each action accepts only its own flags and reports missing required flags by name. Every action prints JSON by default. Pass `--format text` for readable output: tables for `app list` and `app installations list`, and labelled fields for single results. `--format` also applies to `--output-file`. Next-step guidance goes to stderr, so stdout holds only the result.

Creation outputs JSON. `APP_ID` is the public `app_...` value from App creation. Token creation returns `tokenId`, `token`, and `suffix`. Identity creation returns `kid` and `secret`. These raw credentials are only available on creation. `--output-file` creates a new file with mode `0600` and refuses to overwrite an existing file. Without it, the result goes to stdout. Store these files outside source control.

Use the identity `secret`'s literal UTF-8 bytes as the HS256 key. Do not base64-decode it. Include `kid` in every identity JWT header, even when only one signing secret exists.

```sh
# Revoke a sender token after consumers have switched to its replacement.
ting app token revoke --id "$APP_ID" --token-id "$TOKEN_ID"
# Create a replacement signing secret, deploy its kid/secret to consumers,
# then wait at least 5 minutes and 30 seconds after the last old JWT issuance.
ting app identity-secret retire --id "$APP_ID" --kid "$OLD_KID"
```

New sender tokens and signing secrets coexist with old ones. Retiring a signing key immediately rejects all JWTs signed with that key. Existing installation credentials still permit logout. Retired keys remain encrypted in storage for now.

## Provider configuration

```sh
ting app apns configure --id "$APP_ID" --environment production \
  --bundle-id com.example.app --team-id ABCDE12345 --key-id FGHIJ67890 \
  --p8-file ./AuthKey.p8

ting app fcm configure --id "$APP_ID" --firebase-project-id example-project \
  --service-account-file ./firebase-service-account.json
```

APNs accepts `sandbox`, `production`, or `both`. Each environment has one record; configuring `both` replaces both records atomically. The key must be a PKCS#8 P-256 private key. FCM requires a service-account RSA private key, Google OAuth token URI, and matching Firebase project ID. Android `google-services.json` is rejected. Credential files are limited to 64 KiB. Neither command contacts a provider, and both report `configured_untested`.

### Test a configured provider

After a Ting SDK registers a device, choose its installation and queue the fixed test alert:

```sh
ting app installations list --id "$APP_ID"
ting app test --id "$APP_ID" --installation-id "$INSTALLATION_ID"
ting app notification status --id "$APP_ID" --notification-id "$NOTIFICATION_ID"
```

The installation list shows the 100 most recently seen installations without user IDs or push tokens. `app test` follows the [Management API test-send rules](management-api.md#installations-and-test-sends): only a registered, active installation of an enabled App with a configured provider is eligible. It queues the send; a running `ting serve` delivers it. `accepted` in the status means the provider accepted the alert, not that the device displayed it.

Provider configuration is required for installation registration. It is optional for overall readiness and accepting sends with no registered targets.

## Client identity and registration

The consuming backend authenticates its user and signs a short-lived HS256 JWT:

```json
{
  "iss": "<APP_ID>",
  "sub": "<trusted-session-user-id>",
  "aud": "ting:<APP_ID>",
  "iat": 1790000000,
  "exp": 1790000300
}
```

These timestamps are illustrative. Use the current Unix time, a lifetime of at most five minutes, and the issued `kid` in the JWT header. Ting allows 30 seconds of clock skew. The client must never hold the signing secret or sender API token.

```sh
curl -X PUT "http://localhost:8080/v1/installations/$INSTALLATION_ID" \
  -H "Authorization: Bearer $IDENTITY_JWT" \
  -H "X-Ting-App-Id: $APP_ID" \
  -H 'Content-Type: application/json' \
  -d '{"platform":"ios","pushToken":"<hex-APNs-token>","apnsEnvironment":"production","installationToken":"<pre-saved-proposed-credential>","generation":1}'
```

Generate a random UUID for `INSTALLATION_ID` and retain it in app-local storage. Generate the proposed `installationToken` from `ting_ik_` plus 32 cryptographically random bytes encoded as canonical unpadded base64url. Securely persist it and the exact request before sending. Android uses `platform: "android"`, a native FCM token, and omits `apnsEnvironment`.

`generation` is a durable counter shared by PUT and DELETE for this installation, from 1 through 9007199254740991. Allocate a higher value for each new mutation, including logout, and reuse the same value/body on retry. Preserve the counter across process restarts and logout. The current development contract requires these fields; clients of the earlier contract must update. Existing database records migrate with generation zero and retain their credential hashes.

A new registration returns `201`; an existing one returns `200`:

```json
{"installationId":"<UUID>","installationToken":"ting_ik_...","generation":1,"status":"registered"}
```

New mutations of an existing installation require both a fresh identity JWT and its current credential in `X-Ting-Installation-Token`. Same-user refresh normally proposes the current credential. User reassociation or reactivation requires a different proposed credential. Ting adopts the proposed credential after authentication and stores only its hash. Verify the returned ID/credential/generation and commit acknowledgment atomically before dropping the prepared request.

An exact retry after a lost creation/rotation response can prove ownership with the pre-saved proposed credential in its body even when the header has the previous credential or is absent. It still requires a fresh identity JWT for the same user. Exact replay returns `200` without changing registration state. Changed token/environment/user/settings require a later generation on the same UUID, not edits to an unresolved replay. Lower generations return `409 installation_superseded`; reusing a generation for different active settings returns `409 installation_generation_conflict`. Inactive installations cannot be reactivated by replay.

Changing the push token, environment, user, or active state skips queued work for the previous registration. This conservative behavior avoids starting old queued notifications for a new account. A provider call already in flight may still complete; Ting cannot recall a push already submitted to APNs/FCM. A reinstall with the same native token permanently retires previous UUIDs for that token within the App, platform, and APNs environment, including inactive associations. An authenticated PUT to a retired UUID returns `409 installation_superseded` at every generation, even with a changed token. Logout through DELETE remains available. Normal logout and provider invalidation without a transfer still permit higher-generation reactivation with a rotated credential. Clients must surface an unexplained supersession conflict and retain their journal; they must not automatically create another UUID to reclaim the token.

On upgrade, migration 7 fences inactive records that share a token with another record in the same App/platform/environment. Older data cannot distinguish transfer from ordinary logout, so ambiguous inactive duplicates are conservatively retired even if no copy is active. Active records and unique-token logout recovery are preserved.

An unknown installation ID is not proof of ownership. Existing IDs cannot be claimed without their credential. Only genuinely missing/unusable secure state warrants a new UUID. A timeout or `401` does not: use the saved request and credential candidates. `401 unauthorized` identifies failed identity authentication; `401 installation_credential_invalid` identifies failed installation proof. See the [SDK lifecycle contract](sdk-lifecycle.md) for recovery, storage errors, and account changes.

## Logout

```sh
curl -X DELETE "http://localhost:8080/v1/installations/$INSTALLATION_ID" \
  -H "Authorization: Bearer $INSTALLATION_TOKEN" \
  -H "X-Ting-App-Id: $APP_ID" \
  -H 'X-Ting-Installation-Generation: 2'
```

Persist signed-out intent, credential candidates, and a DELETE generation above every allocated PUT before discarding durable credentials. Returns `204` and cancels pending deliveries for only this installation. Repeating the request with the same credential and generation also returns `204`. Lower generations cannot revoke newer registrations. A stale credential after account switching returns `401 installation_credential_invalid`; a conflicting current generation returns `409 installation_generation_conflict`.

Logout needs no identity JWT. If a registration response was lost, use its pre-saved proposed credential, falling back to its securely retained previous credential if needed. Once a higher-generation DELETE succeeds, an older PUT cannot reactivate the installation. An unknown initial-creation outcome may return `401` before a delayed PUT arrives; retain pending cleanup rather than report revocation success. Offline logout cannot revoke the server association until the request reaches Ting. The [lifecycle contract](sdk-lifecycle.md) specifies candidate ordering, restart rules, and how a later authenticated login safely supersedes unresolved work.

## Send and inspect

```sh
curl -X POST http://localhost:8080/v1/notifications \
  -H "Authorization: Bearer $APP_API_TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: order-789-created' \
  -d '{"userIds":["user-123"],"notification":{"title":"New order","body":"Order ready"},"data":{"orderId":"789"}}'
```

Returns `202` with `{"id":"<UUID>","status":"queued"}` only after the notification and eligible installation targets commit in PostgreSQL. Unknown users are valid. Recipients and delivery deadlines are fixed at acceptance; a retry cannot add later registrations or extend the deadline. The default deadline is 24 hours, configured with `TING_DELIVERY_TTL`.

An idempotency key is scoped to its App and retained with the notification. Same key and equivalent content returns the original ID. Different content returns `409 idempotency_conflict`. Recipient order and data-key order do not matter. Missing and empty `data` normalize identically.

```sh
curl "http://localhost:8080/v1/notifications/$NOTIFICATION_ID" \
  -H "Authorization: Bearer $APP_API_TOKEN"
```

```json
{"id":"<UUID>","status":"queued","counts":{"queued":3,"accepted":0,"failed":0,"skipped":0},"errors":{}}
```

`status` is `queued` while any targets remain queued, otherwise `complete`, including sends with zero targets. `complete` does not imply success. `accepted` means provider acceptance, never device display. APNs and FCM adapters record real provider acceptance, safe failure categories, and retries. Queued work expires with `failed` counts and the safe error category `deadline_exceeded`. Replaying POST always returns the original queued acknowledgment; GET reports current counts.

## Limits and errors

- At most 1,000 unique, nonblank user IDs, each at most 256 UTF-8 bytes.
- Nonblank notification title and body. No background-only payloads.
- `data` is an object of strings. Reserved keys include `aps`, `from`, `gcm`, `google`, `message_type`, `collapse_key`, and prefixes `google.` and `gcm.`.
- The serialized APNs payload and FCM message content must each fit 4,096 bytes. FCM validation includes its Android priority and maximum TTL encoding, excluding the routing token and HTTP request envelope. Adapters check the final encoding again before sending.
- Request bodies default to a 1 MiB cap, configurable with `TING_MAX_REQUEST_BYTES`. Unknown JSON fields, trailing JSON, and non-JSON content types are rejected.
- Idempotency keys are at most 200 bytes.
- Non-health requests default to 120 requests/minute/peer/process and 64 concurrent requests per process. Both are configurable. Behind a proxy, clients share its bucket; forwarded IPs are ignored. Newly accepted sends share persistent per-App budgets and instance/App queue capacities, including operator test sends. See [operational limits](operations.md).

Errors have the shape `{"error":{"code":"unauthorized","message":"Valid credentials are required."}}`. Common statuses are `400` validation, `401` authentication, `404` missing or other-App resource, `409` idempotency/provider configuration, `413` size, `415` content type, `429` rate limit, and `503` readiness. Unknown management routes return `404`.

Responses include a server-generated `X-Request-Id`. Unexpected HTTP errors log this identifier, the route pattern, status, and a safe error category; PostgreSQL errors also include their SQLSTATE. Request contents and raw database/error messages remain private. See [HTTP diagnostics](operations.md#http-diagnostics).
