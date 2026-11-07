# SDK installation lifecycle

This is the normative lifecycle contract for React Native/Expo, Swift/SwiftUI, and Android/Compose. The HTTP safeguards and initial Swift and Android clients are implemented; React Native and Compose adapters remain planned. See [Swift SDK verification limits](../sdks/swift/README.md) and [Android SDK verification limits](../sdks/android/README.md). Shared executable HTTP scenarios live in [installation-lifecycle.json](../tests/contracts/installation-lifecycle.json) and [installation-transfer.json](../tests/contracts/installation-transfer.json) and run in the Go API integration suite. Each SDK must run the same scenarios and its own secure-storage/crash tests before release. Android's protocol double consumes both fixtures; Swift currently consumes the original lifecycle fixtures. SDK-to-Go interoperability and signed-device validation remain release checks.

## Durable state and ownership

One coordinator owns each `(canonical endpoint, appId)` namespace, including native token listeners, foreground refresh, login, logout, and retries. Multiple views or client handles must share that coordinator. Never reuse an installation credential across namespaces.

Persist one atomic secure snapshot, or use a transactional storage adapter, containing:

- The random installation UUID and a monotonically increasing `lastAllocatedGeneration`.
- The latest desired identity or signed-out intent and an identity epoch. Persisted user IDs do not prove the user is still authenticated.
- The last confirmed registration, including its user, credential, generation, and native registration settings.
- Any prepared or unresolved PUT, including its generation, user, exact body, previous credential, and proposed credential.
- Any pending logout, including its generation and credential candidates from both the confirmed registration and unresolved PUT.

A generation is an integer from 1 through 9007199254740991, shared by PUT and DELETE for that installation. Allocate it durably before sending a new mutation. Retries reuse the same generation and exact request. Gaps are allowed; reuse for different requests is forbidden. Keep the counter across logout, process restarts, and token rotation. Exhaustion is a terminal state requiring deliberate migration, never wraparound. A migrated server installation starts at generation zero.

Credentials are `ting_ik_` followed by 32 cryptographically random bytes encoded as canonical unpadded base64url. The SDK generates and securely persists a proposed credential before creation, user reassociation, or reactivation. Same-user refresh may retain or rotate the credential; a rotation must be saved before HTTP, and exact retries always retain their original proposed credential. The server adopts the proposed credential only after authenticating the mutation and stores its SHA-256 hash. It never needs a recoverable copy of the raw credential. The initial Swift SDK rotates on each new generation to allow reactivation after provider invalidation without depending on stale local active-state metadata.

If persistence fails, do not send the mutation or report registration/revocation success. Preserve the previous durable snapshot. Never log snapshots, identity JWTs, native tokens, or credential candidates. Truly missing/unusable secure state requires a new installation identity, with the existing documented limitation that an inaccessible server association cannot be revoked by UUID alone. A timeout or `401` is not evidence that secure state was lost.

## Login and reconciliation

1. Record the latest identity intent and increment its epoch under the coordinator. A newer login or logout supersedes older intent immediately.
2. Obtain permission status, a current native token, and the host's identity proof for that user. These may be asynchronous; check the epoch after every await and immediately before preparing a request. Discard stale unsent work. Never prompt from login or background reconciliation.
3. Atomically prepare the exact PUT and its allocated generation. Save its proposed credential before issuing HTTP. Changes to the native token or environment while this request is unresolved become a later mutation, not edits to its retry body.
4. Send the PUT with a fresh identity JWT and the previous confirmed credential in `X-Ting-Installation-Token` when updating. An exact replay may use its already-persisted proposed credential in the body to recover a lost creation/rotation response. The replay still needs fresh identity proof for the same user.
5. Commit the response to secure storage before dropping the prepared request. Validate installation ID, generation, and adopted credential against the prepared request. An exact replay returns `200`; first creation returns `201`. A crash or storage failure before this commit leaves enough information to replay.
6. Apply the result to the active in-memory identity only if its epoch remains current. A stale response can still contain ownership information needed for cleanup. Save it for revocation/reconciliation without reviving the old user.

The API rejects lower generations. Reusing the current generation for different settings returns `installation_generation_conflict`. An exact replay is read-only and does not update `last_seen_at`, reactivate an invalidated installation, or retire another installation. A replay after logout/invalidation returns `installation_superseded`. Higher generations require the current credential in the header; a proposed credential in the body alone only authorizes an exact replay. Reassociation/reactivation must rotate the credential.

Transferring a native token to another UUID permanently supersedes previous UUIDs for that token within the same App, platform, and APNs environment, including logged-out or invalidated associations. After ownership verification, all PUTs to a superseded UUID return `installation_superseded`, regardless of generation or proposed token. The fence survives restart, later logout, and token changes on the replacement. DELETE with the old credential remains available for pending cleanup and cannot affect the replacement. Normal logout/provider invalidation without transfer does not permanently supersede a UUID.

Do not handle this conflict by guessing generations, discarding the secure journal, or automatically allocating another UUID. Surface the conflict for deliberate recovery; an old coordinator must never compete with the replacement for token ownership. Upgrading older database records conservatively retires inactive duplicate-token associations because their original transfer history was not recorded.

`unauthorized` indicates identity proof failed before installation processing. Refresh identity proof at most once per reconciliation pass, while checking the epoch. `installation_credential_invalid` indicates that the supplied installation proof failed. Try only the securely recorded candidates for the unresolved operation; never replace the UUID or claim another installation in response. A conflict does not authorize guessing a generation from server state. Retain the journal and expose an actionable failure if local state cannot explain it.

## Logout and uncertain HTTP outcomes

Clear the active in-memory identity and invalidate stale callbacks immediately. Before discarding any durable association, atomically persist signed-out intent and a pending revocation with a generation greater than every allocated PUT. Retain both previous and proposed credentials for any unresolved rotation. Return a structured pending-revocation state while network or storage work is incomplete. Application sign-out remains host-owned.

Drain existing network work where possible, but never treat cancellation as rollback. DELETE uses the pending revocation generation and only an installation credential, so cleanup does not depend on the host retaining a session or minting another identity JWT.

For an ambiguous rotation, try DELETE with the proposed credential first. If it returns `installation_credential_invalid`, try the previous credential, then the proposed credential once more if needed. This covers a PUT committing between attempts. Once either DELETE succeeds, its generation prevents a late lower-generation PUT from changing that installation. Retry the same DELETE after a lost response. Persist the acknowledgment before removing its journal entry.

An unresolved initial creation has no previous credential. Its candidate DELETE may return `401` before the delayed creation arrives. This does **not** prove successful revocation. Retain pending cleanup and retry on subsequent reconciliation opportunities with bounded attempts per pass. If the initial PUT never committed, there is no server association to revoke, but the client cannot declare this from an ambiguous response. Do not replay an old user's PUT after logout just to obtain a credential.

A later authenticated login may supersede unresolved work using the **same installation UUID**, a new proposed credential, and a generation above all pending mutations. Try the recorded credential candidates as needed. If the prior creation never committed, this is a new creation; if it did commit, its candidate authorizes reassociation. Only after the newer registration is durably acknowledged may the coordinator discard superseded cleanup for that installation. Delayed older PUT/DELETE requests cannot undo the newer association. Cleanup for another namespace is independent and must remain queued.

Offline logout can leave server-side pushes active until cleanup reaches Ting. Already-submitted provider messages cannot be recalled. Never report immediate server revocation merely because local identity is cleared.

## Restart and storage failures

On startup, read the journal before installing refresh listeners. Pending logout takes precedence over registration. A persisted signed-out intent forbids automatic registration of the last user. The host must explicitly restore a currently authenticated user through login before any PUT is prepared or replayed. Do not persist an identity JWT as long-lived authentication authority.

Recover prepared PUTs only when they belong to the host's current user and current identity intent. Otherwise retain their credential candidates for cleanup or a newer authenticated reassociation. Reacquire identity proof rather than replaying an expired JWT. New native tokens are reconciled after ambiguous prior work is resolved or superseded.

Every durable transition must be atomic. A crash after preparing a request but before sending it is indistinguishable from a lost response; the same prepared operation must be safe in both cases. A crash after remote logout but before local acknowledgment retries the same DELETE. A storage error during logout leaves the in-memory identity cleared, retains the previous durable credentials, and returns a storage error rather than a durable-logout success claim.

## Required SDK tests

- Pause the identity callback for A, call login for B or logout, then resume A. No stale PUT is sent and A is never restored in memory.
- Overlap token refresh with account switching/logout. Coalesce work, allocate distinct increasing generations, and preserve the latest identity intent.
- Interrupt each secure-snapshot transition before and after commit, then restart the coordinator. Preserve credential candidates and prevent signed-out registration.
- Lose the HTTP response for creation, reassociation, and reactivation. Recover the same UUID and credential, then reconcile a changed native token with a later generation.
- Logout during an ambiguous creation/rotation after the host session is gone. Retain pending cleanup until acknowledgment or a durably confirmed newer registration supersedes it.
- Deliver an older PUT or DELETE after the newer logout/login acknowledgment. Its server state cannot regress.
- Transfer a token to a replacement UUID, then deliver a prepared higher-generation PUT for the old UUID. The replacement's user, pending deliveries, and future fan-out remain intact, including after restart or replacement logout. Run both transfer fixtures for iOS and Android.
- Fail secure writes before send, after response, and during logout. Never erase the last revocable credential or report a durable transition that was not saved.
- Recreate UI clients and change namespaces. Preserve one coordinator per namespace and never send old cleanup to a new endpoint/App.

These are release requirements for each SDK. The initial Swift and Android packages implement their journals and have injected fault/recovery tests; signed-app storage and live-device push checks remain pending.
