# Ting

> Tiny, self-hosted push notifications.
>
> An open-source project by Klaraa Labs.

**Status:** Development handoff / V1 specification (App management included)  
**Backend:** Go (standard `net/http` + `pgx`)  
**Product:** Ting  
**Repository (proposed):** `klaraalabs/ting`  
**JavaScript package:** `@klaraalabs/ting` (use the existing `@klaraalabs` npm organization; **not** `@klaraa`)  
**License:** Apache-2.0 (proposed for the initial open-source release)

## 1. What we are building

Ting is a lightweight, self-hostable push notification gateway for applications that already have their own users and backends.

The consuming application owns its users, authentication, and business events. Ting owns the mapping from **consuming-App user IDs** to **mobile app installations**, and delivers push notifications to those installations through **Apple Push Notification service (APNs)** and **Firebase Cloud Messaging (FCM)**.

The ideal integration is:

```ts
// React Native / Expo app, after the user has authenticated
await Ting.login(user.id);

// On sign-out
await Ting.logout();
```

And from any trusted backend, using HTTP/curl:

```bash
curl -X POST 'https://ting.example.com/v1/notifications' \
  -H 'Authorization: Bearer <APP_API_TOKEN>' \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: order-789-created' \
  -d '{
    "userIds": ["user-123", "user-456"],
    "notification": {
      "title": "New order",
      "body": "You have a new order"
    },
    "data": {
      "orderId": "order-789",
      "screen": "order-details"
    }
  }'
```

Ting resolves `userIds` to eligible installations and handles platform delivery asynchronously. Consumers never have to store device tokens themselves or select APNs versus FCM.

**Core promise:** One client-side login, one server-side send request, no hosted third-party notification platform required. The self-hosted operator creates an **App** in Ting and configures its APNs/FCM credentials once.

## 2. Product boundaries

### V1 includes

- Multiple isolated consumer **Apps** in one Ting instance, configured through a **small management UI or CLI**.
- App creation, public App ID issuance, server API token creation/revocation/rotation, and identity-signing secret management.
- Secure, per-App provider setup: APNs `.p8` key/Key ID/Team ID/bundle ID; FCM Firebase service-account JSON/target project ID.
- Encrypted provider credentials at rest, masked configuration status, and a test-notification workflow.
- iOS / iPadOS notifications via **APNs**; Android notifications via **FCM HTTP v1**.
- A simple HTTP API for sending notifications to an array of opaque user IDs.
- Multiple mobile installations per user; re-registration, token refresh, account switching, logout.
- Per-App sender authentication and verified client identity.
- Asynchronous fan-out, retries, PostgreSQL queue/worker behavior, and invalid-token cleanup.
- Minimal notification status and operational diagnostics, including a recent-delivery view in the UI.
- Three client integration families: **React Native / Expo**, **native Swift / SwiftUI**, and **native Kotlin / Jetpack Compose**.
- Docker Compose deployment with PostgreSQL and operator bootstrap documentation.
- Examples, tests, and API/SDK documentation sufficient for third-party self-hosting.

### Explicitly out of scope for V1

- A OneSignal-style campaign dashboard, segmentation, templates, marketing automation, A/B tests, or analytics dashboards.
- Notification inbox, read/unread state, messaging history for users, preferences center.
- Scheduled or recurring sends, geo-targeting, topics, or bulk marketing imports.
- Email, SMS, web push, or desktop-specific SDKs.
- Silent/background-only pushes, rich media, interactive actions, custom platform-specific payload builders.
- Organization billing, SaaS multi-tenancy, roles/permissions beyond an **instance operator** for V1, self-service public sign-up.
- Dedicated server SDKs (C#, Go, JavaScript, etc.); **curl/HTTP is sufficient**.
- Redis, Kafka, external brokers, Kubernetes, and separate admin services.

**Scope rule:** The management UI exists to configure Apps, credentials, and verify delivery. It is **not** a notification marketing product.

## 3. Terminology and tenancy

- **Ting instance:** One self-hosted deployment with an operator/admin.
- **App:** One consuming application/environment configured inside Ting (for example, Reem Production or Ajeen Development). This is the canonical user-facing term; do not call it a Ting “Project.”
- **`appId`:** A generated, immutable, **public** App identifier, e.g. `app_9f4d2c81e0ab76359c102ade847fb621` (illustrative). Not a security credential; not necessarily the consumer's slug.
- **App API token:** Secret high-entropy `ting_sk_...` bearer token used by the consuming backend to send/inspect notifications. Never embedded in mobile apps.
- **Identity-signing secret:** Separate per-App secret used by the consuming backend to sign short-lived identity JWTs for device registration.
- **External user ID / `userId`:** The consuming App's user identifier, opaque to Ting.
- **Installation:** A mobile app installation with a random installation ID and native provider token.
- **Push token:** APNs device token or FCM registration token; may rotate.
- **Notification:** One accepted server-side request to notify one or more external users.
- **Delivery:** One delivery attempt sequence to one eligible installation.
- **Instance operator:** A trusted human with access to Ting management, unrelated to consumer App users.

Identity is always scoped to an App: `(app_id, external_user_id)`. Identical user IDs in different Apps are unrelated. Ting never imports the consuming application's Users table.

An App represents one consumer environment with a bundle ID and/or target Firebase project. **Use separate Ting Apps for development and production** (for example, separate Reem Development and Reem Production Apps) and their respective provider configuration. If user IDs are tenant-local, the consumer sends an unambiguous composite ID such as `tenant-id:user-id`.

**App creation flow:** An instance operator logs into Ting, creates an App, copies the generated public App ID, explicitly creates a one-time-visible App API token and an identity-signing secret, configures APNs/FCM, and sends a test notification after registering a device. An App can support iOS-only, Android-only, or both.

## 4. Developer experience

### 4.1 React Native / Expo

Publish the TypeScript package as **`@klaraalabs/ting`** on the existing npm scope.

**Initial compatibility baseline:** Expo SDK **57**, with **New Architecture only**. Require `expo@57.0.23` or later within SDK 57 and recommend the latest SDK 57 patch. SDK 57 uses React Native 0.86, React 19.2.3, and Node.js 22.13.x or later; use the matching Expo-managed dependency versions. Its platform minimums are iOS/iPadOS 16.4 and Android 7 / API 24. Bare React Native apps still require Expo Modules and the corresponding supported Expo/React Native pairing.

SDK 57 is the oldest version used by our own apps and the minimum for the initial Ting SDK. Do not add compatibility branches or backports for SDK 56 or earlier in V1. Revisit older-version support only if adoption provides a concrete reason. Validate SDK 57 and the current stable SDK during implementation, and publish only the dependency ranges that pass the required contract and device checks. These are support requirements, not a claim that compatibility tests already exist.

References: [SDK 57 compatibility](https://docs.expo.dev/versions/v57.0.0/) and [SDK 57 fixes and Xcode 27 configuration](https://expo.dev/changelog/sdk-57). Document Expo's scene-support configuration when an SDK 57 host app builds with Xcode 27.

```ts
import { Ting } from '@klaraalabs/ting';

Ting.configure({
  endpoint: 'https://ting.example.com',
  appId: 'app_9f4d2c81e0ab76359c102ade847fb621', // Public; issued by Ting
  getIdentityToken: async (userId) => {
    // Call the consuming application's already-authenticated backend.
    // That backend must generate a short-lived Ting identity JWT.
    const response = await api.post('/integrations/ting/identity', { userId });
    return response.data.token;
  },
});

// App chooses the right moment to show the system permission prompt.
await Ting.requestPermission();

// Call only after the consuming application's user is authenticated.
await Ting.login(user.id);

// On account logout, before discarding credentials if possible:
await Ting.logout();
```

**Semantics:**

- `configure()` sets public App configuration and a callback for user-identity proof; it must not prompt for notification permission.
- `requestPermission()` explicitly requests platform notification permission. Apps can call it at a suitable UX moment.
- `login(userId)` establishes the currently authenticated external user and registers this installation when permission and a push token are available. It never accepts `userId` as proof of identity by itself.
- `login()` does **not** display a permission prompt automatically. If permission is unavailable, return a structured state such as `permission_required` rather than silently reporting successful registration. Registration is retried after permission is granted.
- `logout()` clears local user association, revokes the remote association, and must not call the application's logout endpoint itself. Handle transient network failure as described in the lifecycle section.
- On app startup or foreground/resume, refresh current push registration for a currently authenticated user; handle token rotation using native token-change listeners.
- Expose optional notification-received/opened callbacks and the message `data` payload; navigation and foreground presentation decisions remain with the app.

**Implementation direction:** Build the native Swift and Android client cores first, then expose them through a thin Expo Modules adapter. Native cores own registration, the secure journal, HTTP, retries, account switching, and logout; TypeScript forwards calls, identity-token requests, and events. Prove Expo SDK 57 dependency packaging and the asynchronous identity callback early. Platform token/notification adapters must feed the same native coordinator, never create a second JavaScript registration engine. Ting sends directly to APNs/FCM and does not use Expo's hosted push service.

This package supports Expo projects and bare React Native projects **with Expo Modules installed**. Document that bare RN setup prerequisite; the Ting wrapper uses Expo Modules rather than a separate legacy React Native bridge. Push notifications require a development/production build with native capabilities; Expo Go is not a sufficient Android push test environment.

Document the Apple push entitlement/capability, Android Firebase `google-services.json`, Android notification permission, and a development-build walkthrough. Choose the smallest required native configuration integration when building the wrapper. If an adapter uses `expo-notifications`, document its configuration, obtain native APNs/FCM tokens with `getDevicePushTokenAsync()`, never Expo-hosted push tokens, and reuse its supported config plugin where appropriate.

### 4.2 Native Swift / SwiftUI

Publish a Swift Package Manager library named **`Ting`**, distributed from this repository. No CocoaPods requirement.

**Initial platform baseline:** iOS/iPadOS **16.4+**. This is the minimum for the native Swift SDK and its SwiftUI example. Keep the reusable client independent of SwiftUI so other supported iOS application frameworks and a future Expo native adapter can consume it. The initial package uses Swift 6 language mode with Swift tools 6.0, a root SwiftPM manifest, and no third-party dependencies. See [the Swift SDK guide](sdks/swift/README.md) for the implemented APIs and current verification limits. Expo adapter packaging remains a separate decision.

```swift
import Foundation
import Ting

let configuration = try TingConfiguration(
    endpoint: URL(string: "https://ting.example.com")!,
    appID: "app_9f4d2c81e0ab76359c102ade847fb621",
    apnsEnvironment: .sandbox // Match the signed build's APNs entitlement.
)
let ting = try TingClient(configuration: configuration) { userID in
    try await api.createTingIdentityToken(userID: userID)
}

try await ting.requestPermission()
let status = try await ting.login(userID: currentUser.id)
// On sign-out: let status = try await ting.logout()
```

Provide a Swift concurrency-friendly API. The host app retains control of the app delegate / notification center delegate. Clearly document the minimal required integration for `registerForRemoteNotifications`, the APNs token callback, notification foreground presentation, and notification tap handling. Where possible, provide methods the app delegates can forward events to rather than requiring Ting to own the delegates.

Use native APNs, not Firebase on Apple platforms. Support normal iOS/iPadOS application targets for V1. Explicitly handle **APNs sandbox vs production** per installation, using a documented SDK/build configuration and the appropriate APNs server endpoint. Do not guess based solely on bundle ID.

### 4.3 Native Android / Jetpack Compose

Publish Kotlin/Android libraries to Maven Central (coordinates proposed):

- `com.klaraalabs:ting-android`: Kotlin SDK, FCM registration, HTTP and lifecycle integration.
- `com.klaraalabs:ting-compose`: small optional Jetpack Compose permission/integration helpers built on `ting-android`.

A Compose application should be able to write:

```kotlin
val ting = rememberTingClient(
    endpoint = "https://ting.example.com",
    appId = "app_9f4d2c81e0ab76359c102ade847fb621",
    identityTokenProvider = { userId -> api.createTingIdentityToken(userId) }
)

// Trigger from the application's chosen UI action.
ting.requestPermission(activity)
// After authenticating the app user:
ting.login(userId = currentUser.id)
// On sign-out: ting.logout()
```

The Compose calls above are **target public API sketches**. The initial `ting-android` core now exists in [sdks/android](sdks/android/README.md), targeting API 24+ with coroutine login/logout, StateFlow status, encrypted Keystore journaling, bounded retries, FCM forwarding, and an Activity Result permission helper. It has a standalone Gradle wrapper and local Maven publication; the Compose module, sample, Maven Central release, and real-device acceptance remain pending. Its initial build uses AGP 9.2.1, built-in Kotlin 2.2.10, JDK 21, and compile SDK 36. See the guide for the implemented signatures and verification limits.

Use Firebase Cloud Messaging and its **HTTP v1 provider path**. The host app supplies Firebase configuration. Avoid silently taking over or conflicting with an existing `FirebaseMessagingService`: document an app-owned service that forwards `onNewToken` and relevant message callbacks to Ting, with an optional reference service for apps without one. Support Android 13+ `POST_NOTIFICATIONS` runtime permission. Compose is only a UI integration layer; delivery and HTTP code belongs in the Android SDK.

### 4.4 Shared SDK behavior

All SDKs should implement the same concepts, not necessarily the exact same language syntax:

- `configure` / initialize
- `requestPermission`
- `login(userId)`
- `logout()`
- registration status / errors
- token refresh, registration reconciliation, notification-open callbacks

The SDK should maintain a random, stable **installation ID** in appropriate app-local storage, and keep installation credentials and its mutation journal in secure platform storage, scoped by endpoint and App ID. Do not use hardware identifiers or push tokens as installation IDs. Regenerate installation identity only if required associated secrets are genuinely missing or unusable, never as automatic recovery from an HTTP timeout or authentication error.

All SDKs must implement the [durable SDK lifecycle contract](docs/sdk-lifecycle.md). A single coordinator serializes mutations per namespace, invalidates stale identity callbacks, and atomically persists prepared requests, credential candidates, monotonic generations, and pending logout before network changes or credential deletion. The server rejects superseded generations. Pending revocation and signed-out intent survive process restarts; the host must explicitly restore a currently authenticated user before registration resumes. Shared [HTTP lifecycle fixtures](tests/contracts/installation-lifecycle.json) and package-local storage/crash tests are required before each SDK release.

Do not cache an APNs device token as permanently valid; refresh via Apple registration callbacks. Don't assume an Android FCM token is permanent either. Avoid sending an installation registration until a real user identity proof exists.

## 5. Security and identity protocol

`Ting.login('user-123')` is a developer convenience, **not permission to claim any arbitrary account**. The caller must be able to prove that the currently authenticated consumer-app user really is `user-123`.

### 5.1 App credentials and provider configuration

Every App has separate credentials with deliberately distinct trust levels:

| Item | Who uses it | Purpose / visibility |
|---|---|---|
| **Public App ID (`appId`)** | All three client SDKs | Non-secret routing identity; generated by Ting |
| **App API token (`ting_sk_...`)** | Consumer backend only | Authorize `POST /v1/notifications` and App-scoped status reads; **never in mobile app code** |
| **Identity-signing secret** | Consumer backend and Ting | HS256 signing for short-lived device registration proof; **not** the App API token |
| **APNs credential** | Ting worker only | Apple provider authentication for the configured bundle ID/topic and environment(s) |
| **FCM service-account JSON** | Ting worker only | Google OAuth2 credentials for the configured Firebase target project |
| **Operator account/session** | Ting's management UI / CLI | Manage all Apps and secrets; **never accepted for sending on the public API** |

Use cryptographically random App IDs (at least 128 bits of unpredictability), `ting_sk_` tokens (at least 256 random bits), and identity-signing secrets (at least 256 random bits). Persist server API tokens and installation credentials as **one-way hashes**; display raw App API tokens only once on creation. Support multiple simultaneously valid App API tokens for overlap during rotation, with named key records, creation time, last-used time, and revocation time. Expose a safe key suffix/prefix for identification, not the raw value.

Store identity-signing secrets and provider private material encrypted at rest. Show a newly generated identity secret **once** through the UI or CLI; on later views return only metadata/status. Rotation of an identity secret needs an overlap strategy (`kid`/active+previous) or an explicit consumer migration procedure; do not silently break all installed clients.

### 5.1.1 Provider credentials entered by the operator

**APNs (optional per App):**

- Upload the **APNs Auth Key `.p8`**, plus its **Key ID** and **Apple Developer Team ID**.
- Enter the App's **bundle ID** (used for `apns-topic`, e.g. `com.example.reem`).
- Select which **APNs environment(s)** the key authorizes: `sandbox`, `production`, or `both` for existing shared/legacy keys. Support separate sandbox/production keys where needed; newer keys can be environment-scoped or topic-specific.
- Validate PEM/EC private-key parsing and required IDs before storing; match installation environment to a configured, compatible APNs key and endpoint at send time.
- Keep APNs HTTP/2 connections separated by provider team and compatible key/environment/topic constraints; do not mix multiple developer teams on one authenticated connection.

**FCM (optional per App):**

- Upload the **Firebase service-account private key JSON**, not Android's `google-services.json`.
- Provide or confirm the **target Firebase Project ID** for the FCM HTTP v1 endpoint.
- Validate expected service-account JSON fields (`type`, `client_email`, `private_key`, `project_id`, `token_uri`) and securely parse the key.
- Default V1 to the **same target Firebase project as the service-account JSON's `project_id`**. Reject mismatches with a useful validation message rather than accidentally delivering under the wrong project. Cross-project impersonation/permissions are valid upstream but can be a later explicit advanced option.
- Document that the **Firebase Cloud Messaging API must be enabled** and the service account must have permission to send to the target project.

Provider credentials are uploaded **to the Ting server** through the authenticated same-origin management UI (or read locally by its CLI). The browser **must not store uploaded keys in localStorage/sessionStorage** or persist files after successful submission. Neither mobile SDKs nor the consuming backends send APNs/FCM secrets during ordinary notifications. Providers can be configured or replaced independently.

### 5.1.2 Encryption and rotation

- Require an operator-supplied `TING_CREDENTIALS_ENCRYPTION_KEY` (base64-encoded 32 random bytes), or equivalent mounted secret file, for every deployment with sensitive credentials. Never auto-generate an unbacked-up key on restart.
- Encrypt sensitive per-App secrets with authenticated encryption, e.g. **AES-256-GCM**, using a fresh random nonce for each encryption and authenticated context (App ID, secret type, format version). Store nonce, ciphertext, format/key version, and metadata; store **no master key in PostgreSQL**.
- Decrypt only inside trusted server/worker code when necessary. Do not serialize secrets into admin read responses or include them in logs/traces/panic output.
- Rotate APNs and FCM credentials by upload/replace, invalidate provider auth caches/connections where necessary, and keep delivery retries safe. Provide an explicit documented master-key re-encryption/rotation operation; **losing the master key requires re-provisioning encrypted secrets**.
- Prefer TLS end-to-end to the reverse proxy and carefully configure upload size limits. Never echo uploaded secret material in validation error messages.

### 5.2 Identity proof (V1)

Use a narrowly scoped short-lived **HS256 JWT** signed by the consuming backend with its App's identity-signing secret. The consumer backend must authenticate the calling user first and **derive the `sub` claim from its trusted session**, not trust an arbitrary request body `userId`.

Required claims:

```json
{
  "iss": "app_9f4d2c81e0ab76359c102ade847fb621",
  "sub": "user-123",
  "aud": "ting:app_9f4d2c81e0ab76359c102ade847fb621",
  "iat": 1790000000,
  "exp": 1790000300
}
```

The timestamps above illustrate a 5-minute token lifetime, not a live token. Ting enforces App issuer/audience, signature, subject, allowed algorithm, time claims, and a maximum token lifetime of five minutes with modest clock-skew tolerance. Include and validate `kid` when multiple signing secrets are temporarily active during rotation. No user records are imported into Ting.

The `getIdentityToken(userId)` callback is a **consumer-defined endpoint** or function. Ting does not host that endpoint or dictate the consumer backend framework. All three SDKs invoke this callback during `login` and when a new authenticated registration is needed.

### 5.3 Installation credential and logout

Before creation or credential rotation, the SDK generates an independent, opaque **installation credential** using 32 cryptographically random bytes and securely saves it with the prepared request. Ting adopts that proposed credential only after JWT-authenticated registration succeeds and persists only its hash. Saving the proposed credential before sending makes a lost response recoverable without exposing a server-side raw credential. The credential is scoped to exactly that App+installation. Refresh requires both a fresh identity JWT and the current installation credential; revocation requires only the installation credential. It can never send notifications, list users, or modify another installation.

Re-associating an existing installation with a *different* user requires a new valid identity JWT and possession of its current installation credential. Reassociation and reactivation require a different, pre-saved proposed credential. Exact replay with that proposed credential recovers a committed response, including when the previous credential is now stale; replay cannot change the user/settings or reactivate a revoked installation. Never allow a client to claim or move another app installation based solely on a user-supplied installation UUID.

`logout()` clears the active in-memory identity immediately, invalidates older async callbacks, and atomically persists signed-out intent plus a **pending revocation** before discarding durable credentials. Keep both prior and proposed credentials for an unresolved registration. DELETE uses a generation above all prepared PUTs and does not need the host's identity JWT. Unknown initial-creation outcomes remain pending rather than treating `401` as successful logout. A later authenticated login can supersede that work on the same UUID at a higher generation. See the lifecycle contract for candidate retries, crash ordering, storage failure, and restart rules. Server-side push can continue until revocation reaches Ting or registration is invalidated; already-submitted pushes cannot be recalled. Never promise immediate server-side logout while offline.

Enforce TLS for all non-localhost traffic, reject malformed inputs, rate-limit client registration operations, apply limits to recipient count and payload size, and avoid storing or logging user PII beyond opaque IDs and necessary operational metadata.

## 6. HTTP API V1

The public sending/installation API is `/v1`. Management uses a **separate same-origin `/admin` UI and `/admin/api/v1`**. Every endpoint returns consistent JSON error envelopes and machine-readable error codes, except static admin pages. Public registration calls never accept App API tokens; management calls never accept App API tokens.

### 6.1 Send notification

`POST /v1/notifications`

**Authentication:** `Authorization: Bearer <APP_API_TOKEN>` (implicitly scopes the request to exactly one App).

```json
{
  "userIds": ["user-123", "user-456"],
  "notification": { "title": "New order", "body": "You have a new order" },
  "data": { "orderId": "order-789", "screen": "order-details" }
}
```

- `userIds`: required non-empty array of unique, opaque user IDs. No Ting-owned user table.
- `notification.title`, `.body`: required non-empty strings for visible alerts.
- `data`: optional **string-to-string** dictionary; reject reserved keys.
- `Idempotency-Key`: optional recommended header; dedupe within one App. Same key/different payload yields conflict.
- Proposed initial limits: 1,000 distinct user IDs per send, and 4 KiB provider-compatible serialized payload cap, validated against actual APNs and FCM encoding.
- Unknown IDs/no installations are valid; never reveal whether particular consumer user IDs exist.

**Response:** `202 Accepted`

```json
{ "id": "019a1234-5678-7abc-8def-0123456789ab", "status": "queued" }
```

`202` means persisted in Ting, **not on-device delivery**.

### 6.2 Inspect notification status

`GET /v1/notifications/{id}`

**Authentication:** App API token. Return App-scoped counts (`queued`, `accepted`, `failed`, `skipped`) and safe error categories, never push tokens or full secret material. “Accepted” means APNs/FCM accepted the request, not the device displayed it.

### 6.3 Register/reconcile an installation

`PUT /v1/installations/{installationId}`

**Authentication:** `Authorization: Bearer <IDENTITY_JWT>` containing fresh valid App identity proof. Existing installation mutations also send `X-Ting-Installation-Token: <CURRENT_INSTALLATION_CREDENTIAL>`. An exact replay may prove possession using its pre-saved proposed credential in the body instead. Supply the public App ID in `X-Ting-App-Id`; it must match JWT `iss`/`aud`.

```json
{
  "platform": "ios",
  "pushToken": "<NATIVE_PUSH_TOKEN>",
  "apnsEnvironment": "production",
  "installationToken": "<PRE_SAVED_PROPOSED_INSTALLATION_CREDENTIAL>",
  "generation": 1
}
```

For Android, `platform` is `android` and `apnsEnvironment` is omitted. Derive the user ID exclusively from signed JWT `sub`, not from client-supplied user input. Return an installation credential only over TLS, never in logs.

`generation` is a durable per-installation integer from 1 through 9007199254740991, shared by PUT and DELETE. Each new mutation allocates a higher value before sending; exact retries reuse the original generation and body. Reassociation/reactivation rotate the credential, while same-user refresh normally retains it. A lower generation is rejected; the same generation cannot describe a different request or reactivate an inactive installation. The proposed credential uses `ting_ik_` plus canonical unpadded base64url encoding of 32 random bytes.

Native token transfer to a replacement UUID permanently supersedes previous UUIDs for that token within the same App/platform/APNs environment, including inactive records. After ownership verification, PUT to a superseded UUID returns `409 installation_superseded` for every generation, even with a changed token; DELETE cleanup remains available. Ordinary logout/provider invalidation without transfer still supports reactivation. SDKs retain the journal and surface unexplained conflicts instead of automatically creating a UUID that could reclaim the replacement's token. Shared [transfer fixtures](tests/contracts/installation-transfer.json) cover delayed mutations and restart on both platforms.

**Response:** `200 OK` (existing or exact replay) or `201 Created` (new) with `{ "installationId": "...", "installationToken": "...", "generation": 1, "status": "registered" }`. Replays are read-only. The SDK confirms the returned credential/generation and saves acknowledgment atomically. Token/environment changes during an unresolved request become a later mutation on the same UUID. Reject provider/platform settings not enabled for the App with a machine-readable configuration error.

### 6.4 Revoke an installation

`DELETE /v1/installations/{installationId}`

**Authentication:** `Authorization: Bearer <INSTALLATION_CREDENTIAL>`, `X-Ting-App-Id`, and `X-Ting-Installation-Generation: <GENERATION>`. Persist a generation above all prepared PUTs. Repeating a successful DELETE with that credential/generation returns `204`; lower generations cannot revoke a newer registration, and a late PUT cannot undo an acknowledged newer logout. Revoke only that installation; keep the user's other devices registered. Unknown/stale credentials return `401 installation_credential_invalid`, not an acknowledgment that an ambiguous creation was revoked. Conflicting/reordered requests return `409 installation_generation_conflict` or `409 installation_superseded` after ownership verification.

### 6.5 Health

- `GET /healthz`: liveness.
- `GET /readyz`: database and essential startup configuration checks. An App without APNs/FCM setup must not make the whole Ting instance unready.

### 6.6 Management API (operator-only)

Provide the smallest API needed by the UI and CLI's shared storage/application layer (the CLI invokes the same Go application services **locally**, without HTTP):

| Method | Path | Responsibility |
|---|---|---|
| `POST` | `/admin/api/v1/session` | Authenticate operator and issue session cookie |
| `GET` | `/admin/api/v1/session` | Inspect the current operator session and obtain its CSRF token |
| `DELETE` | `/admin/api/v1/session` | Log out and expire session |
| `GET, POST` | `/admin/api/v1/apps` | List / create Apps |
| `GET, PATCH` | `/admin/api/v1/apps/{appId}` | App details / rename / enable-disable |
| `POST, GET` | `/admin/api/v1/apps/{appId}/tokens` | Create one-time server token / list masked metadata |
| `DELETE` | `/admin/api/v1/apps/{appId}/tokens/{tokenId}` | Revoke token |
| `GET, POST` | `/admin/api/v1/apps/{appId}/identity-secrets` | List key metadata or issue/rotate identity secret (once-visible response) |
| `DELETE` | `/admin/api/v1/apps/{appId}/identity-secrets/{kid}` | Retire an identity-signing key |
| `GET, PUT, DELETE` | `/admin/api/v1/apps/{appId}/apns` | Safe config/status, upload/replace/remove APNs key metadata and material |
| `GET, PUT, DELETE` | `/admin/api/v1/apps/{appId}/fcm` | Safe config/status, upload/replace/remove FCM service account |
| `POST` | `/admin/api/v1/apps/{appId}/test-notifications` | Send a small visible test push to an existing installation |
| `GET` | `/admin/api/v1/apps/{appId}/installations` | List non-secret installation metadata for test selection |
| `GET` | `/admin/api/v1/apps/{appId}/notifications` | Recent operational sends and provider outcome summaries |
| `GET` | `/admin/api/v1/apps/{appId}/notifications/{notificationId}` | Inspect an App-scoped send or test-send outcome |

The implemented request/response shapes, pagination, and disable behavior are documented in [Management API](docs/management-api.md).

The UI may use authenticated `multipart/form-data` for private-key files with strict size limits. Token issuance responses are one-time exceptions; all subsequent `GET` responses expose **only configuration status and non-secret metadata**, e.g. APNs key ID, Team ID, Bundle ID, environment, upload time; FCM service-account email, target Firebase project, configured status. Do not expose raw keys, service-account JSON, JWT signing secrets, installation tokens, or device push tokens.

**Operator authorization:** No public registration endpoint. Bootstrap the first operator with a CLI command that securely prompts for a password. Store an Argon2id password hash. Use short-lived server-side sessions with `Secure` + `HttpOnly` + `SameSite=Strict` cookies, session rotation on login, CSRF protection and Origin checking for mutations, login throttling, and safe reverse-proxy/TLS configuration. Never grant App API tokens access to management; protect the `/admin` routes independently. The management UI can be disabled via a configuration flag for headless deployments.

**Test push contract:** The operator selects a **previously registered installation** (ID, with non-sensitive platform and last-seen status) or pastes its installation ID. Ting sends a visible alert to **only that device** using the App's credentials, then displays actual provider response/diagnostics. Do **not** allow arbitrary unverified tokens to be used for tests. Distinguish `credential_parse_valid`, `auth_or_permission_verified`, and `provider_accepted` from `displayed_on_device` (which Ting cannot verify). Upload/parse success **must not** be presented as successful APNs/FCM delivery.

## 7. Data model

PostgreSQL is the only stateful dependency. Use explicit migrations, App-scoped uniqueness, minimal secrets exposure, and strict operator-only tables.

### `apps`

- `id` (generated, immutable public App ID, primary key), `name`, optional unique slug, enabled flag, created/updated timestamps.
- Display name can be changed without changing ID; don't use a human-readable slug as authorization proof.

### `app_api_tokens`

- `id`, `app_id`, **hash** of high-entropy token, masked prefix/suffix, optional label, `created_at`, `last_used_at`, `revoked_at`.
- Index token lookup and enforce App-scoped authorization. Multiple valid tokens permit overlapping rotation.

### `app_identity_secrets`

- `app_id`, key ID/`kid`, **encrypted** HS256 key material, active/retiring state, created/retired timestamps.
- Support documented key transition with overlapping verification windows where possible.

### `apns_credentials`

- `app_id`, environment capability, bundle ID/topic, Team ID, Key ID, **encrypted `.p8`**, created/updated timestamps, last validated/tested status (non-secret).
- Support an entry per environment or a compatible existing key covering both. Reject ambiguous configurations; select key based on installation environment.

### `fcm_credentials`

- `app_id`, target Firebase project ID, service-account email (metadata), **encrypted JSON**, created/updated timestamps, last validated/tested status.
- No persistence of short-lived Google access tokens; cache in process memory only.

### `admin_users` and `admin_sessions`

- Local operator accounts with Argon2id password hash; random session IDs stored hashed with creation/expiry/revocation metadata. No public sign-up; initial admin is CLI-bootstrapped.

### `installations`

- `id` random UUID; `app_id`, `external_user_id`, `platform` (`ios`, `android`).
- Native `push_token` (encrypt at rest where practical), `apns_environment` (`sandbox`/`production`, null for Android).
- Hash of installation credential (never plaintext), state/invalidated flag.
- `mutation_generation`: durable client mutation order shared by registration/logout, separate from the server's delivery `revision`; legacy rows start at zero. Exact replay never advances it or rewrites registration state.
- `created_at`, `last_seen_at`, `token_updated_at`, `invalidated_at`.
- Unique `(app_id, id)` and scoped index `(app_id, external_user_id, active)`.
- Prevent duplicate active tokens within an App/platform/environment; safely reassign tokens when devices reinstall.

### `notifications`

- `id` UUID, `app_id`, title/body, normalized data JSON, status, timestamps.
- Idempotency key + canonical request fingerprint; unique `(app_id, idempotency_key)` when present.

### `deliveries`

- `id` UUID, `notification_id`, `installation_id`, provider, state.
- `attempt_count`, `next_attempt_at`, `locked_until`, last safe error category/provider reference, timestamps.
- Unique `(notification_id, installation_id)` prevents duplicate fan-out records.
- Optional `source` (`api`, `admin_test`) to distinguish test sends from production API sends.

Persist eligible installation targets when accepting a notification (or through a durable fan-out job). Do not retarget already-accepted notifications to new registrations. Retain payloads and error metadata for a configurable short period with cleanup. Schema migrations and indexes must be tested; never store plaintext secret copies in audit/debug fields.

## 8. Delivery engine

Keep the architecture as **one Go application and one deployable binary**. The standard `ting serve` process runs the **HTTP API, admin UI, and a bounded background delivery worker pool** together. The same binary provides `ting admin ...`, `ting app ...`, and `ting db migrate`. No separate broker, worker executable, or microservice is required in V1.

- Share one `pgxpool.Pool` and a small typed application state across handlers, management operations, and workers. Reuse long-lived HTTP transports and cached provider clients.
- Use a fixed or configurable number of worker goroutines (or a bounded semaphore), not unbounded goroutines per notification. Limit provider concurrency, connection counts, and overall pending work.
- Propagate `context.Context` to SQL and network operations, use explicit timeouts, and implement coordinated shutdown: stop HTTP intake and leasing, allow an in-flight grace period, and leave uncompleted leases safely recoverable.
- One `ting serve` process runs API and workers by default. A worker-only command or multi-instance scaling is not necessary for V1, but PostgreSQL leasing must remain correct if a second process ever runs.

Flow:

1. Authenticate the App API token, validate payload and quotas, and resolve idempotency.
2. Persist the notification and eligible device delivery targets within a database transaction. The DB commit is the durability boundary for HTTP `202 Accepted`.
3. Return `202 Accepted` after the transaction commits.
4. Workers lease due deliveries via short PostgreSQL transactions using `FOR UPDATE SKIP LOCKED` and lease expirations, then release DB connections before network calls.
5. Send to APNs or FCM with bounded concurrency and reused clients/transports.
6. Record provider acceptance or classify failures as retryable, permanent, or invalid-token.
7. Retry transient failures with capped exponential backoff, jitter, and `Retry-After` when supplied. Release or recover expired leases after worker crashes.
8. Invalidate only demonstrably expired/unregistered tokens. Do **not** mistake malformed payloads, auth errors, or temporary provider failures for invalid devices.
9. Expire delivery work after a configurable deadline and aggregate App-scoped status counts.

**Delivery semantics:** At-least-once *attempts*, best-effort platform delivery. Provider acceptance is **not a device receipt**. Crashes and ambiguous network timeouts may create duplicate attempts. Request idempotency and unique delivery rows reduce duplicates but cannot guarantee exactly-once device display.

**APNs:** Token-based `.p8` provider authentication; HTTP/2 with `apns-topic`, `apns-push-type: alert`, explicit sandbox/production hostname, and reusable connections. Classify Apple error reasons/statuses carefully, including 410 invalidation, 429 throttling, and transient 5xx.

**FCM:** Use the **official Firebase Admin SDK for Go** to deliver visible notifications to eligible registered devices through FCM HTTP v1. Initialize **App-scoped Firebase clients with that App's own decrypted service-account credentials and target Firebase Project ID**. Never let one Ting App inherit another App's credentials or accidentally fall back to server-wide default credentials. Token lifecycle and send errors must be classified conservatively.

No external queue in V1. PostgreSQL-backed jobs and a bounded goroutine worker pool suffice.

## 9. Tech choices and deployment

The server, management API, worker, and CLI use **Go**. The small management UI remains TypeScript/React/Vite, compiled to static assets served from the Go binary. Prefer the Go standard library and a few focused packages over frameworks, code generation, or new infrastructure.

| Area | V1 choice |
|---|---|
| Backend / CLI | Go (maintained stable release; pin toolchain in CI and Docker builds) |
| HTTP API / routing | `net/http` with Go 1.22+ method-aware `ServeMux` routes; no Gin/Fiber requirement |
| Concurrency / shutdown | Goroutines, channels or semaphores, `context.Context`, `http.Server.Shutdown` |
| PostgreSQL | `github.com/jackc/pgx/v5/pgxpool` (or maintained compatible successor); plain SQL |
| Database migrations | Versioned SQL migrations using `pressly/goose/v3` with embedded migration files |
| Delivery queue | PostgreSQL `FOR UPDATE SKIP LOCKED` + recoverable leases |
| HTTP provider transport | Reused `net/http.Client` and `http.Transport`, with explicit HTTP/2 for APNs |
| APNs authentication | ES256 provider JWT using Go `crypto/ecdsa` and a vetted JWT library |
| FCM authentication and sends | Official `firebase.google.com/go/v4` Admin SDK; per-App service-account credentials |
| Serialization | `encoding/json` |
| Identity JWT validation | `github.com/golang-jwt/jwt/v5`, fixed HS256 algorithm and strict claim checks |
| Configuration / CLI | Environment variables and mounted secrets; standard `flag`/subcommands initially |
| Management UI | React + TypeScript + Vite static SPA, served with `embed.FS` and `net/http` |
| Operator authentication | Argon2id via `golang.org/x/crypto/argon2`, server-side sessions, CSRF and Origin protection |
| Secret encryption | Standard `crypto/aes` + AES-256-GCM, operator-supplied 32-byte master key |
| Logs / observability | `log/slog` with structured and redacted output; health endpoints |
| JavaScript SDK | TypeScript **`@klaraalabs/ting`** |
| iOS SDK | Swift Package Manager **`Ting`** |
| Android SDK | Kotlin SDK + optional Compose helpers, Maven Central |
| Deployment | Multi-stage Go Docker build + Docker Compose |

The versions above name **library families**, not mandatory fixed release tags; choose maintained versions at implementation time and commit `go.mod` and `go.sum`. Prefer standard packages unless an external dependency demonstrably reduces complexity or risk.

### 9.1 Go implementation rules

- Use **one Go module** (`github.com/klaraalabs/ting/src/api`) in `src/api` and **one executable** at `src/api/cmd/ting`. Keep business code in small domain-aligned `internal/` packages. No premature plugin architecture or multi-module workspace.
- Use typed structs and explicit errors; return consistent, machine-readable JSON errors from HTTP handlers. Never `panic` on malformed inputs, uploaded credentials, or provider errors.
- Ensure request and worker `context.Context` propagation, sensible SQL/network timeouts, bounded response body reads, and connection-pool limits. Avoid holding PostgreSQL transactions or connections across outbound APNs/FCM calls.
- Use `pgxpool` and reviewed parameterized SQL rather than an ORM. Keep queries near the relevant domain package or in a small `internal/storage` layer; test transactional fan-out, idempotency, and lease recovery with actual PostgreSQL.
- Use `goose` SQL migrations embedded into the binary (`embed.FS`). Place the SQL files **under the embedding Go package directory** (Go cannot embed `../` paths). The migration runner may use the pgx `database/sql` adapter while normal API/worker queries use `pgxpool`. Serialize migrations with a PostgreSQL advisory lock or verified migration locking; expose `ting db migrate` and run migrations before readiness. Avoid runtime SQL schema generation.
- Keep runtime configuration typed and validated on startup. Read master keys and other secrets from mounted files where possible; never log their values. Distinguish required instance-level config from optional per-App provider config.
- Have a tiny common provider result taxonomy (`accepted`, `retryable`, `permanent_failure`, `invalid_token`) and separate APNs/FCM adapters, not a heavyweight generic notification pipeline.
- Put provider calls behind small, mockable Go interfaces. Use `httptest.Server` and stubbed messaging clients in tests; ordinary CI must never require real Apple/Google credentials.
- Reuse APNs HTTP transports and JWT caches. Cache FCM Firebase Admin/messaging clients **by App ID and credential version**; invalidate/recreate them on credential changes. Do not use a singleton default Firebase app or environment-wide `GOOGLE_APPLICATION_CREDENTIALS` for all consumer Apps.
- Use explicit concurrency limits and backoff. Never automatically retry permanent credential/configuration errors indefinitely; preserve useful, sanitized diagnostics.
- Implement graceful SIGINT/SIGTERM shutdown with `signal.NotifyContext`, `http.Server.Shutdown`, and draining/leasing controls. Correlate logs by App/notification/delivery IDs without logging identity JWTs, push tokens, private keys, user IDs, or notification bodies by default.
- Prefer straightforward synchronous Go code over adding a separate task framework. Do not engineer custom asynchronous runtimes, background message brokers, or extensive abstractions.

### 9.2 Provider notes

**APNs:** Use `net/http` over HTTPS with **HTTP/2 enabled and verified in tests**, explicit sandbox/production hosts, `apns-topic`, and `apns-push-type: alert`. Authenticate using ES256 signed JWTs with the App's Team ID, Key ID, and `.p8` key, refreshing the token well before its validity limit. Reuse provider connections safely, keeping compatible teams/credentials/environments isolated. On credential replacement/revocation, invalidate affected JWT caches and idle connections. Map APNs error reasons/status codes accurately; a generic HTTP 400 does not automatically invalidate the target device.

**FCM:** Use the **official Firebase Admin SDK for Go** and FCM HTTP v1. Create the SDK client explicitly from each App's decrypted service-account JSON and configured Firebase project, not Application Default Credentials. Prefer passing credential bytes in memory to initialization instead of writing decrypted service-account JSON to disk. Cache clients for normal delivery and reinitialize after App credential replacement. Use the SDK's provider-specific error classification when available; write tests for unregistered tokens, invalid payloads, permissions, quotas, and transient failures. Do not use legacy server keys. The V1 SDK registration contract uses native FCM registration tokens; review Google's ongoing token/FID migration guidance before public release so the gateway can adapt without changing `Ting.login(userId)`.

### 9.3 Docker and operations

**Docker Compose baseline:** one `ting` container and one `postgres` container, persistent PostgreSQL storage, health checks, and sensible resource limits. Use a multi-stage Dockerfile: build the React/Vite UI with Node, copy compiled assets into the Go embed directory, build the binary with `go build -trimpath`, and copy only the binary, static assets embedded within it, and CA certificates into a small non-root runtime image. Cross-build Linux `amd64` and `arm64` images. Use `CGO_ENABLED=0` when supported by all selected packages; do not require musl or a complex cross-compilation pipeline.

Ting runs behind an HTTPS reverse proxy such as Nginx. **Never expose the admin UI on public cleartext HTTP.** Support an explicit flag to disable it for headless operation. Configure trusted reverse-proxy headers carefully so `Secure`/`HttpOnly` cookies behave correctly behind TLS termination. Restrict admin exposure further via firewall/access control/VPN where practical. Never ship default operator passwords.

The implemented capacity, HTTP, send-budget, and retention settings are documented in [operational limits](docs/operations.md).

Configuration includes `DATABASE_URL`, bind address, trusted proxy setup, retention, worker concurrency, request/rate limits, `TING_CREDENTIALS_ENCRYPTION_KEY` (or `_FILE`), `TING_ADMIN_ENABLED`, operator session keys, and log level. Fail early for invalid essential instance settings. An App without push-provider credentials remains a normal provisioning state, not an instance-wide startup error.

Run embedded `goose` migrations before readiness on `ting serve`, with safe migration serialization, and expose `ting db migrate`. Document PostgreSQL backup/restore and master-key recovery. A clean instance cannot use the operator web UI until `ting admin create` securely bootstraps an admin account.

### 9.4 CLI administration

Use the same `ting` Go binary and internal application services for CLI and management API:

```bash
# Instance operator bootstrap (password securely prompted, not a flag)
ting admin create --username admin
ting admin reset-password --username admin

# App lifecycle (App ID generated automatically)
ting app create --name "Reem Production" --slug reem-prod
ting app list
ting app show --id <APP_ID>

# Server token and identity signing secret (one-time output)
ting app token create --id <APP_ID> --label "Reem API"
ting app token revoke --id <APP_ID> --token-id <TOKEN_ID>
ting app identity-secret create --id <APP_ID>

# APNs environment: sandbox, production, or both
ting app apns configure --id <APP_ID> \
  --environment production \
  --bundle-id com.example.reem --team-id TEAM_ID \
  --key-id KEY_ID --p8-file ./AuthKey.p8

# FCM service-account JSON (NOT Android google-services.json)
ting app fcm configure --id <APP_ID> \
  --firebase-project-id FIREBASE_PROJECT_ID \
  --service-account-file ./firebase-service-account.json

# Send a diagnostic test to an existing installation
ting app installations list --id <APP_ID>
ting app test --id <APP_ID> --installation-id <INSTALLATION_ID>
ting app notification status --id <APP_ID> --notification-id <NOTIFICATION_ID>

ting serve
ting db migrate
```

These are **target commands**, not already-implemented commands. Sensitive operations must require deliberate invocation, show newly issued tokens/secrets only once, and never send secrets to logs. Provide an explicit output-file alternative for secure automation with strict permissions. Avoid passwords or raw secrets in command-line flags and shell history. File uploads via the CLI read local files only; no global service-account file paths should be required for normal operation.

### 9.5 Minimal management UI (required in V1)

Build only these screens with accessible forms and clear error states:

1. **Sign in:** Instance operator login. No registration page or email password reset; bootstrap and recovery through CLI.
2. **Apps:** List/create Apps; show public App ID; enable/disable and rename.
3. **App settings:** Create/copy one-time server API tokens; list masked tokens; revoke/rotate with overlap. Issue/rotate identity-signing secrets and explain how a consumer backend uses them.
4. **Push providers:** APNs `.p8` + Team ID + Key ID + bundle ID + supported environment(s); FCM service-account JSON + target Firebase project. Show only non-secret metadata and statuses: `Not configured`, `Configured, untested`, `Last test accepted`, `Last test failed`.
5. **Test delivery:** Choose a **previously registered installation** and send a visible title/body test. Show provider acceptance or sanitized failure (not a device-display claim), with recent per-App operational failure summaries.

Serve only static built UI files via `embed.FS` in the existing Go HTTP server. No Node server in production. Ensure the embedding directory contains build assets before compiling (CI/Docker build order and local build script); don't introduce a second runtime service to serve the UI.

Never ask for an App API token in the push-provider form; it belongs to the consuming backend. Never put APNs/FCM provider credentials into a mobile SDK. Consumer apps must configure their own platform-side capabilities, entitlements, and Firebase client files.

**Create App → copy App ID → issue API token and identity-signing secret → configure APNs/FCM → register device → test → send via curl.**

## 10. Repository layout

Use a **single public monorepo**. Keep the Go backend as **one module and one deployable binary**. Swift/Kotlin/TypeScript SDK contributors should not need a Go toolchain to work on their packages.

The layout below includes planned components. Add their directories when implementing them.

```text
ting/
├── PROJECT.md
├── README.md
├── LICENSE
├── src/
│   ├── api/                         # One Go module and deployable binary
│   │   ├── go.mod
│   │   ├── go.sum
│   │   ├── cmd/ting/                # CLI: serve, admin, app, db
│   │   └── internal/
│   │       ├── config/
│   │       ├── server/
│   │       ├── auth/
│   │       ├── apps/
│   │       ├── installations/
│   │       ├── notifications/
│   │       ├── delivery/
│   │       ├── providers/
│   │       │   ├── apns/
│   │       │   └── fcm/
│   │       ├── admin/              # Operator auth and management API
│   │       ├── secrets/
│   │       ├── dbmigrations/       # embed.go and versioned SQL migrations
│   │       └── dashboard/          # Planned embed.go and generated dist/
│   └── dashboard/                  # Planned React + TypeScript + Vite UI
├── sdks/                           # Independently published packages
│   ├── react-native/               # npm: @klaraalabs/ting
│   ├── swift/                      # Swift Package: Ting
│   ├── android/                    # Kotlin Android core
│   └── compose/                    # Jetpack Compose helpers
├── examples/
│   ├── expo/
│   ├── swiftui/
│   └── compose/
├── docs/                           # API contracts and self-hosting guides
├── tests/
│   ├── contracts/                  # Shared API contract fixtures across languages
│   └── e2e/                        # Tests spanning the API and SDKs
├── deploy/
│   ├── compose.yaml
│   ├── compose.build.yaml
│   └── Dockerfile
└── .github/workflows/
```

Keep applications under `src/` and independently published SDKs under root-level `sdks/`. Each application or SDK owns its dependencies and build configuration.

Keep unit and integration tests with the application or SDK they exercise. Go tests, including database integration tests, stay alongside their packages in `src/api` (`*_test.go`). Dashboard tests stay in `src/dashboard`; SDK tests stay within each SDK. Reserve root-level `tests/contracts` for shared API contract fixtures and `tests/e2e` for tests spanning the API and SDKs. Create those directories when adding their contents; do not move existing API tests there.

Run Go commands from `src/api`, or use `go -C src/api` from the repository root.

When implementing the dashboard, build `src/dashboard` and copy its compiled assets into `src/api/internal/dashboard/dist` before building the Go binary. An `embed.go` in that Go package can then use `//go:embed dist/*`. Provide a committed minimal `dist/index.html` placeholder or a documented bootstrap script so fresh-checkout Go builds/tests work before the full SPA is built. No separate Node runtime in production. Avoid speculative `core`, `shared`, or plugin frameworks; split packages only where domain boundaries justify it.

## 11. Required SDK / API contracts

Define **one** versioned wire protocol, with explicit test fixtures shared across Go, TypeScript, Swift, and Kotlin:

- JSON payload formats and common error envelope.
- Signature verification cases (valid, expired, wrong audience, wrong issuer, wrong App, incorrect algorithm).
- Register, refresh, account-switch, and logout semantics.
- Installation credential handling and revocation.
- Durable proposed credentials, exact replay after lost responses, generation ordering, and secure pending-logout recovery, using [the lifecycle contract](docs/sdk-lifecycle.md) and [shared fixtures](tests/contracts/installation-lifecycle.json).
- Provider token replacement and invalidation behavior.
- Notification `data` key/value normalization and size limits.

Each SDK must own only native platform concerns, identity callback, storage, and HTTP networking; the Go server owns all routing and delivery decisions.

**Backends integrate via curl/HTTP only in V1.** Publish a clear cURL example and OpenAPI schema. Do not implement first-party .NET, Go, Node, or other sending SDKs until genuine demand emerges.

## 12. Development milestones

### Phase 1: Go foundation, App provisioning, and curl-first API

- Initialize one Go module (`net/http`, `pgxpool`, `goose`), CLI, migrations, and Docker Compose.
- App data model (public App ID, server API tokens, identity-signing secrets), operator bootstrap, and AES-256-GCM encryption with an operator-supplied master key.
- CLI App lifecycle, APNs/FCM credential setup/rotation, and a known-installation test-send command.
- Verified client identity JWT, installation register/delete, send/status API, and idempotency enforcement.
- Shared installation lifecycle fixtures and replay/generation safeguards before client SDK implementation.
- PostgreSQL queue with bounded goroutine workers, recoverable leases, and graceful shutdown.
- APNs HTTP/2 + ES256 adapter and **App-scoped Go Firebase Admin SDK** messaging clients, with provider mock tests.
- CI: `gofmt`, `go vet`, `go test ./...`, selected `go test -race ./...` suites, migration checks, Docker image smoke test.

**Acceptance:** CLI-create App, configure APNs/FCM, register a fixture installation, send by curl, inspect status, and see accurate mock-provider results. No manual database editing.

### Phase 2: Minimal secure management UI

- React + TypeScript + Vite static UI embedded and served by the Go HTTP server.
- Operator sign-in/session/logout, CSRF safeguards, login throttling, and CLI password reset/bootstrap.
- Apps list/create/details; one-time server API token issuance/revocation; identity-secret issuance/rotation.
- APNs `.p8` and FCM service-account upload/replace/validation, showing masked status only.
- Test delivery against registered installation and show safe recent failure summaries.
- Verify that App API tokens never grant access to management APIs.

**Acceptance:** Fresh self-hoster can provision an App in the UI after CLI bootstrap, without ever retrieving stored private keys via GET.

### Phase 3: Swift SDK

- SPM package with Swift concurrency, APNs delegate forwarding, secure installation credential storage, and permission API.
- SwiftUI example and real-device APNs sandbox/production tests.

**Acceptance:** Native SwiftUI installation receives a notification through the same App send API.

### Phase 4: Android / Compose SDK

- Kotlin SDK with FCM token refresh, secure storage, coroutines, and permission handling.
- Compose helpers, sample app, and `FirebaseMessagingService` forwarding instructions.

**Acceptance:** Native Compose installation receives a notification through FCM using the same App send API.

### Phase 5: React Native / Expo SDK

- Publishable `@klaraalabs/ting` with public `appId` and identity callback.
- Thin Expo Modules adapters over the Swift/Android cores, including asynchronous identity callbacks, status/events, and lifecycle forwarding. Validate Expo SDK 57 packaging with an early integration proof before finalizing native APIs.
- Expo and bare React Native + Expo Modules examples/docs.
- Real iOS and Android device tests for registration, push, account switch, and logout.

**Acceptance:** Expo app calls `Ting.login(userId)` and receives curl-sent notification on an actual device.

### Phase 6: Public-release hardening

- Complete shared wire conformance across all SDKs, migration/retry/load tests, race detector, admin security and secret-redaction review. Installation lifecycle fixtures and server safeguards start in Phase 1 and gate every SDK release.
- App-specific FCM-client isolation tests and provider key rotation/reconfiguration tests.
- Backup/restore, master encryption key rotation, operator recovery, self-hosting docs.
- Quickstart targeted under ~15 minutes **after** Apple/Google credentials are available.
- Publish Docker image, npm package, Swift tags, Maven artifacts, OpenAPI schema, and contribution/security docs.
- Benchmark representative notification ingestion, queue throughput, CPU, and RSS on small VPS sizes. Optimize only demonstrated bottlenecks; do not promise a particular sends/second number without measured APNs/FCM results.

## 13. Non-negotiable acceptance cases

1. One user with three registered devices receives three delivery attempts from one send.
2. Same external user ID across two Apps cannot cross-receive notifications.
3. Client cannot claim another user without a valid App-scoped identity JWT.
4. Installation credential cannot send notifications, manage Apps, or revoke another installation.
5. User switching on one device stops notifications for prior user after successful reassociation.
6. Logging out device A leaves device B registered.
7. APNs/FCM token rotation updates active installation without duplicate fan-out.
8. Invalid tokens retire correctly; generic 5xx/auth/config failures do not disable all devices.
9. Idempotent send retries reuse original notification; different payload under same key conflicts.
10. Crash/restart preserves queued work and recovers expired leases.
11. Permission denial is not falsely reported as successful device registration.
12. Provider-accepted response never labeled on-device `delivered` or `read`.
13. An operator creates two Apps via UI/CLI; each has distinct App ID and separate sender credentials.
14. An App API token cannot read/change provider keys, other Apps, or any admin API.
15. Raw API tokens and generated identity secrets are displayed only once. Provider secrets never appear in management reads, logs, or browser storage.
16. APNs config accepts appropriate per-environment keys; a production-only key is never used for sandbox delivery.
17. FCM setup rejects `google-services.json` as a server credential and validates service-account JSON and target project.
18. A configured-but-untested provider is not falsely shown as authenticated or confirmed working.
19. Test send targets a known registered installation and reports provider acceptance vs failure honestly.
20. Operator UI has secure sessions, CSRF protection, login throttling, and no open initial sign-up.
21. Removing or rotating a provider key invalidates relevant cached tokens/connections without leaking secrets.
22. Backend is one Go executable + PostgreSQL (with embedded admin UI assets) in Compose, including migrations, startup, and graceful restart.
23. Expo, native Swift and Android/Compose SDKs share the same wire semantics and use the public `appId` rather than a private API token.
24. Losing a creation, reassociation, or reactivation response preserves the pre-saved credential and installation UUID. Exact replay is read-only; native token changes use a later generation.
25. An older registration or revocation cannot undo a newer acknowledged logout/account association. Proposed-credential replay requires fresh identity proof and cannot change the user/settings or reactivate an inactive installation.
26. SDK process death or secure-storage failure cannot discard the last revocable credential. Persisted signed-out intent and pending cleanup survive restart; stale callbacks never restore an old user. SDK-local crash/storage tests must pass before publication.

## 14. Guidance for the coding agent

- **Prioritize simplicity and correctness.** Ting is a push gateway, not a OneSignal clone.
- **App** is the canonical management term; use `appId`, not `projectId`, in the Ting SDK and public headers. “Firebase project” is still the official Google term.
- Implement App provisioning and APNs/FCM credential storage, admin authorization, and test delivery as **V1 requirements**; do not postpone them to future work.
- Ship a minimal same-origin management UI and matching CLI operations, without building a separate admin backend or marketing UI.
- Distinguish public App ID, private App API token, per-App identity signing secret, provider credentials, and operator credentials. Never treat them as interchangeable.
- Implement short-lived JWT proof for `login(userId)`; the consumer backend derives user identity from its authenticated session.
- Require operator-provided master encryption key; redact secrets, hash bearer tokens and sessions, defend admin mutations against CSRF.
- APNs auth keys may be environment-scoped/topic-specific. Support proper per-App environment selection and safe connections.
- FCM requires service-account JSON, **not** client `google-services.json`; use the official Go Firebase Admin SDK (FCM HTTP v1) and explicit **per-App** Firebase project/client configuration.
- Do not conflate syntactically valid provider credentials, actual authorization, provider acceptance, and device display.
- Use one Go module and binary, `net/http`, `pgxpool`, `goose` SQL migrations, and bounded goroutine workers with PostgreSQL leases. Commit `go.mod` and `go.sum`. Keep build and CI feedback fast.
- Implement curl-based sending API before client SDKs; no server SDK required for V1.
- Maintain all client SDKs: React Native/Expo (`@klaraalabs/ting`), Swift/SwiftUI, Kotlin/Compose. Each sample must receive real push on a device.
- Keep the existing **`@klaraalabs`** npm scope; do not use `@klaraa`.
- Go was chosen deliberately because Ting is I/O-bound, Go builds iterate quickly, and its official Firebase Admin SDK simplifies provider integration. Do not reintroduce Rust or a Go HTTP framework without a concrete need.
- Prefer small domain-aligned modules and concise documentation; no generic plugin architecture, organization management, or messaging platform features.
- If a choice is unspecified, choose the smallest secure extensible implementation and document the trade-off. Do not silently expand V1.

## 15. Platform and implementation references

- Go `net/http` HTTP/2 and transport reuse: https://pkg.go.dev/net/http
- Go `embed.FS` for the admin UI: https://pkg.go.dev/embed
- Go structured logs (`log/slog`): https://pkg.go.dev/log/slog
- Go database driver/pool `pgx`: https://pkg.go.dev/github.com/jackc/pgx/v5/pgxpool
- Goose SQL migrations: https://github.com/pressly/goose
- Firebase Admin SDK for Go: https://firebase.google.com/docs/admin/setup
- Firebase Admin SDK sending: https://firebase.google.com/docs/cloud-messaging/send/admin-sdk
- Firebase Admin SDK Go package: https://pkg.go.dev/firebase.google.com/go/v4/messaging
- Apple APNs token authentication: https://developer.apple.com/documentation/usernotifications/establishing-a-token-based-connection-to-apns
- Apple APNs HTTP/2 connections: https://developer.apple.com/documentation/usernotifications/establishing-a-connection-to-apns
- Expo native APNs/FCM tokens: https://docs.expo.dev/push-notifications/sending-notifications-custom/
- Expo notifications SDK: https://docs.expo.dev/versions/latest/sdk/notifications/
- Expo Modules in bare React Native: https://docs.expo.dev/bare/installing-expo-modules/
- Apple app registration: https://developer.apple.com/documentation/usernotifications/registering-your-app-with-apns
- Apple sending notifications: https://developer.apple.com/documentation/usernotifications/sending-notification-requests-to-apns
- Firebase FCM HTTP v1: https://firebase.google.com/docs/cloud-messaging/send/v1-api
- Firebase Android push token lifecycle: https://firebase.google.com/docs/cloud-messaging/android/get-started
- Firebase registration-token management: https://firebase.google.com/docs/cloud-messaging/manage-tokens

---

**Ting's design filter:** If a feature is not necessary to create/configure a consuming App, map an authenticated external user to its installations, or deliver and troubleshoot a push, it probably does not belong in V1.
