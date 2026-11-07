# Ting for Android

The initial native Kotlin SDK targets Android 7.0 / API 24+. It provides coroutine login/logout, a durable encrypted installation journal, FCM token registration, explicit permission helpers, and status/notification flows. It is independent of Go and Compose. Keep one client at application scope in the application's main process.

This is a development SDK. JVM tests exercise lifecycle/storage failures against injected adapters and a protocol double checked against both shared HTTP fixtures. They do not establish actual Go interoperability, Android Keystore behavior, Firebase authorization, or device notification display. A signed consumer, Compose helpers/sample, real-device delivery, and Maven Central publication remain pending.

Verified on 2026-11-03: all 28 JVM tests passed, release lint reported zero errors with dependency-update notices, debug/release AARs built, and local Maven publication succeeded. A separate temporary Android project compiled against the local artifact and its public APIs using the published dependency metadata. No Android device or Firebase project was used for these checks.

## Build and consume locally

From this directory, with JDK 21 and Android SDK platform 36 installed:

```sh
./gradlew testDebugUnitTest lintRelease assembleRelease
./gradlew publishReleasePublicationToMavenLocal
```

The wrapper pins Gradle 9.4.1. The library uses AGP 9.2.1 with its built-in Kotlin 2.2.10, Java 17 bytecode, coroutines 1.10.2, Firebase Messaging 25.0.1, Activity 1.11.0, and compile SDK 36. This is the initial tested build combination, not a compatibility matrix. The host must provide Firebase configuration and a compile SDK compatible with these AndroidX dependencies. SDK 24 is the declared minimum; execution on that minimum remains a device release check.

For a local consumer, include `mavenLocal()` in its dependency repositories, then:

```kotlin
implementation("com.klaraalabs:ting-android:0.1.0-SNAPSHOT")
```

This coordinate is local only; no public artifact has been published. Use Maven consumption so the AAR's dependencies resolve. Copying only the AAR omits its dependency metadata. A composite build can also substitute this standalone Gradle project. The host's Firebase BoM/version constraints must be compatible with the declared Messaging dependency.

## Firebase and notification channel setup

Configure Firebase for your host application, including its own `google-services.json` and Google Services plugin or equivalent explicit `FirebaseOptions`. Configure matching FCM provider credentials in the Ting App. Never ship Ting's server API token or identity-signing secret in the application.

Create a host-owned notification channel before permission or login integration. For example, on API 26+:

```kotlin
val channelId = "messages"
getSystemService(NotificationManager::class.java).createNotificationChannel(
    NotificationChannel(channelId, "Messages", NotificationManager.IMPORTANCE_DEFAULT)
)
```

Use the same channel ID in `TingConfiguration` and the host manifest. Supply a monochrome notification icon resource appropriate to your app:

```xml
<application ...>
    <meta-data android:name="com.google.firebase.messaging.default_notification_channel_id"
        android:value="messages" />
    <meta-data android:name="com.google.firebase.messaging.default_notification_icon"
        android:resource="@drawable/ic_notification" />
    <service android:name=".AppMessagingService" android:exported="false">
        <intent-filter>
            <action android:name="com.google.firebase.MESSAGING_EVENT" />
        </intent-filter>
    </service>
</application>
```

The SDK manifest adds INTERNET and POST_NOTIFICATIONS permissions. It declares no messaging service and does not initialize a Firebase project, create channels, or display notifications. Integrate the forwards below into an existing service if your app already has one. Do not register competing services for `MESSAGING_EVENT`. Use target SDK 33+ so your app controls permission timing.

## Create one application-scoped client

```kotlin
import com.klaraalabs.ting.TingConfiguration
import com.klaraalabs.ting.installations.IdentityTokenProvider
import com.klaraalabs.ting.installations.TingClient
import com.klaraalabs.ting.push.create

val ting = TingClient.create(
    context = applicationContext,
    configuration = TingConfiguration(
        endpoint = "https://ting.example.com",
        appId = "app_9f4d2c81e0ab76359c102ade847fb621",
        notificationChannelId = "messages",
    ),
    identityTokenProvider = IdentityTokenProvider { userId ->
        // Your authenticated backend derives JWT sub from its session.
        api.createTingIdentityToken(userId)
    },
)
```

`create` is suspending and performs storage work off Main. Retain its result once in an application-owned container and share it across Activities, services, and a future Compose adapter. Initialize it early enough for cold-start notification forwarding; an application-scoped `Deferred<TingClient>` can let those entry points await the same initialization. Report initialization failures through host error handling. Before the first unlock after boot, storage returns `STORAGE_UNAVAILABLE`; retry construction after unlock. Direct-boot contexts and independent clients in other Android processes are unsupported.

The identity callback must be thread-safe, use a bounded timeout, and avoid calling Ting recursively while the callback is pending. Ting never persists JWTs. After restoring the host's authenticated session:

```kotlin
val registration = ting.login(userId = currentUser.id)
// On app sign-out, before discarding the host session where possible:
val logout = ting.logout()
```

These calls do not prompt. `REGISTERED` confirms Ting's association, not notification display. Network errors return `PENDING_REGISTRATION` or `PENDING_REVOCATION`; terminal operational errors return `FAILED` with a typed `TingFailure`. Invalid input, closed/duplicate clients, and failed login/logout intent writes throw `TingException`. Always inspect returned status, and observe `ting.status` as a `StateFlow` for later transitions. Failed intent persistence clears the live identity and blocks old callbacks until an explicit login/logout successfully saves new intent.

Persisted user IDs never restore host authentication. Call `login` explicitly after session restoration. Pending logout can be reconciled without a host session. A fresh Firebase token is fetched when registration needs one, and each activation drops the in-memory token before refreshing. A confirmed same-user/token registration younger than 24 hours is reused; an older one gets a new generation and credential, allowing provider-invalidated associations to reactivate.

## Explicit permission requests and lifecycle

Register an Activity Result launcher before the Activity is started. Connect your chosen button/action to the helper on Main:

```kotlin
private val notificationPermission = registerForActivityResult(
    ActivityResultContracts.RequestPermission()
) {
    lifecycleScope.launch { ting.reconcile() }
}

// In a user action:
if (Build.VERSION.SDK_INT >= 33) {
    TingPermissions.requestPermission(notificationPermission)
} else {
    lifecycleScope.launch { ting.reconcile() }
}
```

The host owns rationale, denial recovery, and Settings navigation. `TingPermissions.status(context, channelId)` distinguishes runtime permission required, app notifications disabled, missing channel, blocked channel, and allowed. Android does not expose a reliable universal "never requested" state; `RUNTIME_PERMISSION_REQUIRED` does not mean another system prompt is guaranteed. Login and reconciliation never launch permission UI.

Forward app-level foreground/background transitions to `ting.applicationDidBecomeActive()` and `ting.applicationDidEnterBackground()`. Use the host's process-lifecycle observer or equivalent application-level policy; forwarding every Activity transition independently can stop retries while another Activity remains visible. No lifecycle observer or WorkManager job is installed by Ting. After connectivity recovery, call `ting.reconcile()`.

## Forward FCM tokens and received messages

In your existing `FirebaseMessagingService`, forward `onNewToken` and `onMessageReceived`. This sketch assumes your application container provides a shared `Deferred<TingClient>` named `ting`, an application coroutine scope, and an error reporter:

```kotlin
override fun onNewToken(token: String) {
    app.scope.launch {
        try { app.ting.await().onNewToken(token) }
        catch (error: TingException) { app.reportTingFailure(error.failure) }
    }
}

override fun onMessageReceived(message: RemoteMessage) {
    val notification = FirebaseNotifications.received(message) ?: return
    app.scope.launch { app.ting.await().notificationReceived(notification) }
    // The host chooses whether/how to show a foreground notification.
}
```

Catch/report client initialization failures in these host coroutine jobs as well. The token belongs to the same Firebase app passed to `TingClient.create`; the default is `FirebaseMessaging.getInstance()`. The host supplies any alternate instance and remains responsible for matching the configured Ting App's Firebase project.

FCM notification-plus-data messages in the background can be displayed by Firebase without `onMessageReceived`. On a tap, their data arrives in the launch Activity intent. Forward both initial intent in `onCreate` and replacement intent in `onNewIntent`:

```kotlin
private fun forwardOpen(intent: Intent) {
    val notification = FirebaseNotifications.opened(intent) ?: return
    lifecycleScope.launch { ting.notificationOpened(notification) }
}

override fun onNewIntent(intent: Intent) {
    super.onNewIntent(intent)
    setIntent(intent)
    forwardOpen(intent)
}
// Also call forwardOpen(intent) from onCreate after the shared client is available.
```

The intent helper requires a Firebase message ID and copies custom string extras, excluding reserved transport keys. Background launch intents do not guarantee the visible title/body, so those fields are empty for opens. For host-created notifications, forward your own `TingNotification` when their PendingIntent is opened. Intent payloads are untrusted; navigation must validate them against the current authenticated user.

Collect `ting.events` for `StatusChanged`, `NotificationReceived`, and `NotificationOpened`. Each collector gets current status first. Up to 32 initial opens are retained for the first collector, and the last 256 open identifiers are deduplicated within the current client lifetime. Each active collector has a 64-event oldest-drop buffer; cancel collection when its owner is disposed. Received callbacks, cross-restart exactly-once delivery, and delivery while a process is stopped are not promised. Do not log notification contents, JWTs, native tokens, or journal snapshots.

## Durability, HTTP, and shutdown

`KeystoreInstallationStorage` encrypts one versioned journal per canonical endpoint/App using AES-256-GCM with an Android Keystore key and namespace-bound authenticated data. Ciphertext lives in credential-protected `noBackupFilesDir`. A same-directory synced temporary file is atomically renamed over the old snapshot. Logout keeps the UUID, generation counter, and revocation credentials; it does not delete keys or journals. Existing unreadable records or missing decryption keys fail without generating a replacement identity. Reinstall/key loss and deliberate recovery require host handling and cannot revoke an inaccessible old server association by UUID alone.

Prepared bodies, proposed credentials, mutation generations, failure streaks, and pending revocation persist before HTTP. Exact retries reuse the saved request. New account/token intent gets a later generation and retains prior ownership candidates. Server acknowledgments are checked and saved before unresolved ownership evidence is discarded. A superseded UUID remains a surfaced conflict; it is never automatically replaced.

The default OkHttp client uses a 15-second call timeout, no cookies or authentication callbacks, no redirects, and a 64 KiB response limit. Production endpoints require HTTPS. Literal localhost, 127.0.0.1, and ::1 endpoints may use HTTP for development; the host must opt into Android cleartext policy if needed. Network I/O errors, 429 and 5xx retain the operation and persist exponential backoff with jitter. Client delay grows from about one second to five minutes before jitter. Retry-After seconds/dates are honored with a one-hour ceiling. Foreground reconciliation schedules at most two automatic retries per user/foreground cycle. Backgrounding cancels timers, not in-flight durable work. Explicit reconciliation respects persisted deadlines.

Cancelling a caller's coroutine does not cancel an already-started journal operation. `close()` drains its worker and releases namespace ownership before replacement. Pending work remains durable. The host callback's timeout bounds close if an identity lookup is in flight. Offline logout can leave server-side pushes active until revocation reaches Ting; already-submitted provider messages cannot be recalled.

The injectable `InstallationStorage`, `PushRegistrationProvider`, `IdentityTokenProvider`, and `TingHttpTransport` support tests and future adapters. Storage calls are synchronous atomic operations; custom implementations must preserve old data on failed saves. One coordinator must own the namespace in one process.

## Before release

Run a signed API 24 and current Android consumer through Firebase setup, API 33+ permission denial/recovery, missing/blocked channel states, foreground/background display and taps, process-death opens, token rotation, account switching, offline logout, locked storage, backup exclusion, and key-loss recovery. Exercise the SDK against the actual Go server and verify Maven consumption from a separate project. Add Compose helpers and a runnable sample under their planned directories when implementing them. Public release and phase-4 device acceptance remain open.

Platform references: [FCM setup](https://firebase.google.com/docs/cloud-messaging/android/get-started), [FCM receipt and background intents](https://firebase.google.com/docs/cloud-messaging/android/receive-messages), [notification permission](https://developer.android.com/develop/ui/compose/notifications/notification-permission), [Android Keystore](https://developer.android.com/privacy-and-security/keystore).
