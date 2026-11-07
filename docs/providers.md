# Push providers

`ting serve` sends iOS notifications directly to APNs and Android notifications through the official Firebase Admin Go SDK’s HTTP v1 path. The adapters run inside the existing bounded worker pool. PostgreSQL remains the only stateful dependency.

Provider acceptance increments `accepted`. It is not proof that a device displayed the notification. Uploading credentials still reports `configured_untested`; only a real send can verify provider authorization.

## APNs

The adapter selects the configured credential for the installation’s explicit `sandbox` or `production` environment. It sends to `api.sandbox.push.apple.com` or `api.push.apple.com` respectively. A production-only key is never used as a sandbox fallback.

Requests use HTTP/2, `apns-push-type: alert`, priority 10, the configured bundle ID as `apns-topic`, and the notification UUID as `apns-id`. The payload contains `aps.alert.title/body` and the string data values at the root. The native expiration uses the remaining persisted delivery lifetime. Payloads above 4,096 serialized bytes are rejected.

Provider authentication uses ES256 with the App’s P-256 `.p8` key, Key ID, and Team ID. JWTs are reused for up to 50 minutes. An `ExpiredProviderToken` response clears that JWT and schedules a retry. Connections are separate for every App, environment, and credential version, so different developer teams never share an authenticated transport.

| APNs result | Handling |
| --- | --- |
| HTTP 200 | Provider accepted. |
| HTTP 410 with `Unregistered` | Invalid token; retire only the current installation revision. An older provider timestamp does not override a newer registration. |
| HTTP 400 or 413 | Permanent payload/configuration failure. `BadDeviceToken` and `DeviceTokenNotForTopic` do not automatically retire a device because environment/topic mismatches can cause them. |
| HTTP 401/403 | Permanent authentication failure, except `ExpiredProviderToken`, which refreshes and retries. |
| HTTP 429 | Retry with backoff and `Retry-After`. Excess provider JWT updates wait at least 20 minutes. |
| HTTP 5xx or network failure | Retry within the delivery deadline and attempt limit. |

Apple’s [token authentication guidance](https://developer.apple.com/documentation/usernotifications/establishing-a-token-based-connection-to-apns) and [APNs protocol reference](https://developer.apple.com/library/archive/documentation/NetworkingInternet/Conceptual/RemoteNotificationsPG/CommunicatingwithAPNs.html) describe the authentication and response contract.

## FCM

Each client has an explicit Firebase project and service-account credential. The adapter does not use Application Default Credentials, `GOOGLE_APPLICATION_CREDENTIALS`, `FIREBASE_CONFIG`, or a singleton default Firebase client. Client configuration cannot cross App boundaries.

The operator-provided service-account JSON must contain a valid RSA key, a service-account email, the matching target project ID, and `https://oauth2.googleapis.com/token`. Only the required, validated credential fields reach Google’s auth library. Additional upload fields cannot redirect token exchange to another host. The Firebase Cloud Messaging API must be enabled and the service account must have permission to send for that project.

OAuth tokens are generated using Google’s JWT OAuth implementation with the `firebase.messaging` scope. Tokens live only in the App/version client cache. Refresh uses the current request context, so a client remains usable after the request that created it ends. Auth and messaging transports have timeouts, bounded response reads, and no redirects.

Messages use the native FCM registration token, visible title/body, string data, Android high priority, and a whole-second TTL bounded by the remaining delivery lifetime and FCM’s 28-day maximum. A lifetime below one second produces TTL zero, requesting immediate delivery without storage. Final SDK JSON content is checked against 4,096 bytes, excluding the routing token and request envelope.

The official SDK classifies responses. Only `UNREGISTERED` retires an installation. `INVALID_ARGUMENT` is a permanent payload error; sender-ID mismatch and permissions/authentication errors are permanent configuration errors. Quotas retry after at least one minute, respecting longer `Retry-After` values. Internal/unavailable/network errors retry without invalidating devices. These distinctions follow [FCM’s error guidance](https://firebase.google.com/docs/cloud-messaging/error-codes).

The SDK can perform up to four internal retries on network failures or HTTP 503 responses within the worker’s attempt context. The adapter preserves the largest observed `Retry-After` if the SDK times out. Permanent OAuth failures stop those inner retries immediately; OAuth throttling also returns to the durable worker schedule.

## Credential replacement and removal

Before every send, each adapter reads the current credential record from PostgreSQL. An unchanged version reuses the client. A new version retires the old client and closes its idle connections. In-flight calls may finish; they close their retired transport when they return. A removed or unreadable credential evicts the cached client and prevents another send using it. Transient database errors prevent sending but retain the cache for recovery.

Each adapter caches at most 128 App/environment entries. Provider key rotation through the existing CLI creates a new version. There is no restart requirement or global service-account path.

## Verification

Run `go -C src/api test ./...` from the repository root for local mock protocol tests. With `TING_TEST_DATABASE_URL`, integration tests also provision encrypted credentials, register installations, run the actual workers through the adapters, verify acceptance/invalid-token outcomes, and replace/remove credential records.

The APNs tests use a TLS HTTP/2 server and verify signed JWTs, headers, environments, connection reuse, cancellation, and error handling. FCM tests use the actual Firebase Admin SDK with local mock OAuth/messaging endpoints, verify RSA assertions and App isolation, and check expiry, rotation, quotas, and authentication failures.

Live Apple/Google delivery has not been tested here. To verify an installation, configure real credentials, register its native token with a valid identity JWT, send through the curl API, inspect provider status, and confirm display on the device separately. Native capabilities, notification permissions, and Firebase client configuration remain the consuming application’s responsibility.
