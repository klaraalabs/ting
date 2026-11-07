# Delivery workers

`ting serve` owns the HTTP server, the PostgreSQL queue, and a bounded worker pool. The workers share the API's `pgxpool.Pool`. No broker or worker executable is required.

The binary registers APNs for iOS and FCM for Android. Both adapters load the App’s credential version before every send and reuse clients while that version remains current. Provider and worker integration tests use local mock servers and never contact Apple or Google. See [push providers](providers.md) for protocol behavior.

## Claim and send

Each available worker claims one due delivery using a short `FOR UPDATE SKIP LOCKED` statement. The claim increments `attempt_count`, creates a random lease ID, and persists `locked_until`. The worker releases the connection before loading and sending the message. It never keeps a transaction open across a provider call.

Before sending, it checks the App is enabled, the installation is active, its revision still matches the accepted target, and the lease and delivery deadline remain valid. Native push tokens are decrypted only for the attempt. Provider calls have a timeout bounded by the remaining delivery and lease budgets. The Firebase SDK may retry network errors and HTTP 503 responses internally within that timeout. One worker attempt can therefore contain several HTTP requests. Provider `Retry-After` values survive SDK timeouts and carry into the durable retry schedule.

Concurrency is bounded by `TING_WORKER_CONCURRENCY` per process. Two processes can consume the same queue safely. Each has its own concurrency limit; there is no instance-wide concurrency quota yet.

Queue admission is bounded separately by instance and per-App pending-delivery limits. Leased and retrying deliveries count toward those limits until they become terminal. Admission also caps installations per send and newly accepted notifications per App. Retention deletes only old terminal notifications; see [operational limits](operations.md).

An attempt includes an acquired lease, even when a process crashes before it can prove that a provider call started. This bounds repeated crash recovery. A claim released before starting during graceful shutdown returns its attempt budget.

## Outcomes and retries

| Result | Queue behavior |
| --- | --- |
| `accepted` | Records provider acceptance. Never means device display or read. |
| `retryable` | Reschedules with exponential backoff and jitter, subject to attempt and deadline limits. |
| `permanent_failure` | Fails the delivery without disabling the installation. |
| `invalid_token` | Fails the delivery and retires only the same current installation revision. Other pending deliveries for that revision are skipped. |

Backoff grows from the configured base to the configured maximum. Jitter chooses a delay between half and all of that interval. `Retry-After` is a minimum and can exceed the exponential maximum. If no retry fits before the delivery deadline, the job fails with `deadline_exceeded`. Reaching the attempt limit fails it with `attempts_exhausted`.

Only explicit `invalid_token` results retire an installation. An APNs unregistration timestamp older than the installation’s most recent registration does not retire it. Timeouts, generic provider failures, authentication errors, unreadable encrypted tokens, and permanent payload errors do not retire devices. Unknown error categories are replaced with a safe category. Raw provider responses and exception text are not exposed through notification status or logs.

Provider timeouts retry conservatively. Panics from a sender become bounded retries with `internal_error`. SQL failures leave the lease for recovery rather than inventing a delivery outcome.

## Recovery and deadlines

A worker can reclaim a lease after `locked_until`. Every result update checks both the lease ID and expiration. A previous worker cannot overwrite a newer attempt or invalidate a replacement token. Registration, account switching, logout, and token retirement clear pending leases when they cancel queued work.

A bounded maintenance query handles up to 100 expired or exhausted jobs per polling interval. It skips live leases and locked rows. A crashed final attempt becomes `attempts_exhausted` after its lease expires. Delivery deadlines persist in PostgreSQL, so restarts and idempotent request retries cannot extend them.

These are at-least-once attempts. A crash after provider acceptance but before the database commit can cause a duplicate push on retry. A provider request already in flight may complete after logout or an account change. Ting cannot recall an already submitted push.

## Shutdown

On SIGINT/SIGTERM, HTTP intake and new leasing stop. Existing HTTP requests and worker attempts drain in parallel for `TING_SHUTDOWN_GRACE`. If the window expires, worker contexts are canceled. Unfinished attempts retain recoverable leases; they are never reported as successful or as invalid tokens because of process shutdown. The database pool closes after workers finish cleanup.

Adapters must honor context cancellation and bound all network work. The worker pool does not spawn detached goroutines to abandon an uncooperative adapter.

## Provider interface

Both adapters implement `delivery.Sender` and are registered by installation platform in `src/api/cmd/ting/serve.go`: `ios` for APNs and `android` for FCM. `Message` supplies App ID, notification ID, installation ID, environment, decrypted native token, alert content, data, and remaining TTL. It deliberately omits external user identity.

Select provider credentials by App and environment, reuse clients/transports, and pass the supplied context to every provider operation. Map responses to the four result types and the allowed safe error categories. Convert provider `Retry-After` headers to a duration. Apply native provider expiration/TTL where supported. Never use a default cross-App credential or log message contents, private material, or device tokens.

The PostgreSQL tests exercise simultaneous consumers, lease recovery, stale-result rejection, retry scheduling, deadlines, bounded concurrency, connection release, installation changes, graceful draining, and forced cancellation.
