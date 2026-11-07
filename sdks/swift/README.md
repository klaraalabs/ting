# Ting for Swift

The initial native Swift client supports iOS/iPadOS 16.4+ and uses Swift 6 language mode, with a Swift tools minimum of 6.0. The root [Package.swift](../../Package.swift) publishes the `Ting` library from this directory. It has no third-party dependencies and does not require Go. The macOS deployment setting exists for host-side core tests; this increment does not provide a macOS push SDK.

This is a development SDK. Lifecycle and protocol-double tests run on the host, and the iOS-specific code compiles for iOS 16.4. Real-device APNs sandbox/production delivery, Keychain behavior in a signed consumer app, older compiler compatibility, and an Expo adapter are still release gates. No package version/tag has been published yet.

## Add the package

For local development, link directly to this checkout. No Git remote, package registry, or published version is required:

1. Open the consuming app's project or workspace in Xcode.
2. Choose **File > Add Package Dependencies**, then **Add Local**.
3. Select the Ting repository root, which contains `Package.swift`. Do not select `sdks/swift`.
4. Add the `Ting` library product to the app target, then use `import Ting`.

Xcode builds the SDK from those local files, so SDK edits are available on the next app build. Other machines need a checkout at the referenced path; use a relative path where possible or switch to a remote dependency for shared builds. See [Apple's local-package guide](https://developer.apple.com/documentation/xcode/editing-a-package-dependency-as-a-local-package).

A consumer Swift package can use `.package(path: "../ting")` and `.product(name: "Ting", package: "ting")`. Adjust the relative checkout path for that consumer.

GitHub can host the package as an ordinary Git repository. Once the repository is pushed, Xcode or Swift Package Manager can fetch `https://github.com/klaraalabs/ting.git`; a branch requirement works during development, and a SemVer tag such as `0.1.0` or `v0.1.0` enables version-based dependencies. The root manifest already exposes the Swift sources in this monorepo. There is no separate Swift package upload to GHCR or GitHub Packages. See [Apple's package-dependency guide](https://developer.apple.com/documentation/xcode/adding-package-dependencies-to-your-app).

No remote version has been published yet. After publishing a compatible `v0.1.0` tag, a consumer package can declare:

```swift
.package(url: "https://github.com/klaraalabs/ting.git", from: "0.1.0")
```

## Create one application-scoped client

```swift
import Foundation
import Ting

let configuration = try TingConfiguration(
    endpoint: URL(string: "https://ting.example.com")!,
    appID: "app_9f4d2c81e0ab76359c102ade847fb621",
    apnsEnvironment: .sandbox
)

let ting = try TingClient(
    configuration: configuration,
    identityTokenProvider: { userID in
        // Call your already-authenticated backend. It derives JWT sub from its session.
        try await api.createTingIdentityToken(userID: userID)
    }
)
```

Keep this client at application scope and inject the same instance into views, delegates, and native adapters. A second live client for the same canonical endpoint/App throws `clientAlreadyExists` to prevent competing journals. Different Apps or endpoints have independent storage. To deliberately replace a client, `await ting.close()` first; this drains its in-flight operation and keeps pending work for a replacement client.

The identity provider is `@Sendable` and asynchronous. Use an actor-safe authentication client. It must return the current user's short-lived identity JWT, enforce an appropriate timeout for its own backend request, and avoid calling back into Ting while waiting for an identity result. The SDK does not persist identity JWTs or accept the backend's signing secret or sender API token.

## Permissions, authentication, and foreground recovery

```swift
// Explicit user action only. Construction and login never show the system prompt.
let permission = try await ting.requestPermission() // [.alert, .sound, .badge]
// Or choose options, e.g. quiet provisional delivery without a prompt:
// try await ting.requestPermission(options: [.alert, .sound, .badge, .provisional])

// After the host has authenticated/restored the user:
let registration = try await ting.login(userID: currentUser.id)

// On sign-out, before discarding your application's session where possible:
let logout = try await ting.logout()
```

`login` can return `permissionRequired`, `awaitingAPNsToken`, or `pendingRegistration`. `registered` confirms Ting's installation association, not device display of a notification. Authorized, provisional, and ephemeral permission allow registration. `requestPermission(options:)` accepts `.alert`, `.sound`, `.badge`, `.provisional`, `.providesAppNotificationSettings`, and `.criticalAlert` (which needs Apple's entitlement); the default is `.standard`. The system's permission status remains available as the `requestPermission()` result; UI explanation and Settings navigation belong to the host.

Forward application activation to `await ting.applicationDidBecomeActive()` and backgrounding to `await ting.applicationDidEnterBackground()`. A SwiftUI app can forward the corresponding application notifications from its composition root. The client requests fresh APNs registration on each activation; the host must forward APNs success/failure callbacks below. While a confirmed registration waits for that fresh token, its status stays `registered`. If the token, user, and APNs environment match the last acknowledged registration from the past 24 hours, no request is sent. Otherwise the client registers again, which also reactivates an association the provider invalidated server-side. Coalesce app-level forwarding rather than installing a new client or listener for each window.

On restart, the SDK can drain persisted logout without host identity proof. It never resumes a persisted user's registration until the host calls `login` after restoring its authenticated session. Forward a fresh APNs token as well; a token in the journal is only the snapshot needed to replay an unresolved request.

`logout` clears the active in-memory identity immediately. `pendingRevocation` means durable cleanup remains, including an uncertain initial registration that might not yet exist on the server. Offline logout cannot recall provider-submitted pushes or promise immediate server-side revocation. Do not delete the SDK's Keychain entry on logout.

If saving a new login/logout intent fails, the live identity is cleared and registration work is blocked until the host explicitly retries login or logout and saves that intent. This prevents a delayed callback for an older user from proceeding after an account change whose durable write failed.

## Forward APNs and notification events

Enable the app target's Push Notifications capability. Set `apnsEnvironment` to match the signed build's APNs entitlement: `.sandbox` for development and `.production` for distribution. Treat this as explicit build configuration; do not derive it only from bundle ID or `DEBUG`. Configure matching environment credentials in Ting. This increment does not require a notification service extension or an App Group.

The SDK never replaces app delegates, swizzles methods, sets the notification-center delegate, or chooses foreground presentation. Integrate these forwards into your existing delegate. SwiftUI applications can host their delegate using `@UIApplicationDelegateAdaptor`.

```swift
import UIKit
import UserNotifications
import Ting

@MainActor
final class PushDelegate: NSObject, UIApplicationDelegate, UNUserNotificationCenterDelegate {
    // Assign the app's single client before starting permission/login integration.
    var ting: TingClient?
    var onForwardingFailure: ((TingFailure) -> Void)?

    func application(_ application: UIApplication,
                     didRegisterForRemoteNotificationsWithDeviceToken deviceToken: Data) {
        guard let ting else { return }
        Task { [weak self] in
            do { _ = try await ting.didRegisterForRemoteNotifications(deviceToken: deviceToken) }
            catch { self?.onForwardingFailure?((error as? TingFailure) ?? .storageUnavailable) }
        }
    }

    func application(_ application: UIApplication,
                     didFailToRegisterForRemoteNotificationsWithError error: Error) {
        guard let ting else { return }
        Task { await ting.didFailToRegisterForRemoteNotifications() }
    }

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter,
                                           didReceive response: UNNotificationResponse) async {
        // Copy native values before crossing an actor boundary.
        let notification = TingNotification(response.notification)
        let client = await ting
        await client?.notificationOpened(notification)
    }

    nonisolated func userNotificationCenter(_ center: UNUserNotificationCenter,
                                           willPresent notification: UNNotification) async
        -> UNNotificationPresentationOptions {
        let event = TingNotification(notification)
        let client = await ting
        await client?.notificationReceived(event)
        return [.banner, .sound] // The host chooses this presentation policy.
    }
}
```

The host assigns `UNUserNotificationCenter.current().delegate` to its own delegate early in launch, or forwards from the delegate it already uses. Wire `onForwardingFailure` to the host's error handling and observe registration status for operational failures. `TingNotification(_:)` copies the identifier, title, body, and custom payload keys other than `aps`. Strings are kept as-is, numbers and booleans become their textual form, and arrays/objects become JSON strings with sorted keys. Retain the client and forward notification responses before your navigation is ready; the client buffers up to 32 initial opens and delivers them only to the first `events()` subscriber, so subscribe from the router that handles them. Open-event duplicate suppression covers the last 256 notification identifiers in the current process. It is not an exactly-once guarantee across restarts. Background notification receipt is not guaranteed to produce a received callback.

Consume events from the UI or native adapter:

```swift
let stream = await ting.events()
for await event in stream {
    // In a SwiftUI .task this runs on the view's actor; otherwise hop to MainActor for UI work.
    // Update presentation state or pass notificationOpened to the host's authenticated router.
}
```

Each subscription gets the current status first. A status event is emitted whenever any field changes, including the user ID and retry deadline. Subscriptions are independent, have a 64-event newest-value buffer, and are removed when their stream terminates. View-task cancellation ends observation; it does not cancel the client's durable registration/logout operation. Sensitive data is not emitted in status snapshots. The host must avoid logging notification contents and must validate navigation against the current authenticated user.

## Storage and network behavior

The default storage atomically replaces a single versioned Keychain record per endpoint/App, with `kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly` and no synchronization, so prewarmed and background launches after the first unlock can construct a client. Records written by earlier builds migrate to this class on their next save. Before the first unlock since boot, construction throws `storageUnavailable` instead of generating a new installation; construct the client again once protected data becomes available. Malformed or incompatible journals throw `corruptJournal` and are preserved for diagnosis. Normal logout retains the installation UUID and mutation counter.

Prepared requests, credential candidates, pending revocation, and generation allocation are saved before HTTP. Each new registration generation proposes a fresh credential so it can also reactivate a server association invalidated by the provider; exact retries retain their original credential. A native-token change supersedes unresolved work with a higher generation and retains its credential candidates. An acknowledgment is saved before ownership evidence is discarded. A storage failure returns `failed(.storageUnavailable)` or throws while preserving the previous durable snapshot. Missing/unusable secrets and reinstall recovery require deliberate host handling; never erase the journal just because an HTTP request failed. The device-only record does not migrate to another device; do not assume app removal clears every Keychain entry.

The default URLSession uses an ephemeral configuration, a 15-second request/resource timeout, no cookies or credential storage, and refuses redirects. Production endpoints require HTTPS. Plain HTTP is permitted only for literal localhost, 127.0.0.1, or ::1 development endpoints.

Network errors, `429`, and server `5xx` retain the exact prepared operation. Retry deadlines and the consecutive-failure count are persisted. The delay doubles with each failure, from 1 second up to 5 minutes with ±20% jitter, across cycles and restarts, and resets after a successful request. Numeric and HTTP-date `Retry-After` are respected but capped at one hour, so a misbehaving proxy cannot block registration and logout indefinitely. While active, the coordinator schedules at most two automatic retries per user/foreground reconciliation cycle. Backgrounding stops scheduled retry; activation, connectivity recovery through `reconcile()`, or a new user action can resume pending work without violating the deadline. No background task or silent push capability is installed. Each registration attempt fetches one identity proof and reuses it across ownership candidates; an `unauthorized` proof gets at most one fresh proof retry; configuration and ownership errors are surfaced without UUID replacement.

`InstallationStorage`, `TingHTTPTransport`, and `PushRegistrationProvider` can be injected for tests or a future native adapter. Custom storage must provide atomic replacement and preserve existing state on a failed save. All adapters must be `Sendable` and avoid maintaining a second installation coordinator.

## Checks

From the repository root:

```sh
swift test
xcodebuild -scheme Ting -destination 'generic/platform=iOS' \
  -derivedDataPath /tmp/ting-ios-build CODE_SIGNING_ALLOWED=NO build
```

The tests exercise lifecycle faults against a protocol double, validate that double against the shared [HTTP scenarios](../../tests/contracts/installation-lifecycle.json), and verify URLSession request headers/response handling. They do not claim end-to-end Go interoperability or live APNs delivery. Before publishing, run a signed iOS/iPadOS consumer through permission denial/recovery, sandbox and production push, token rotation, account switching, locked storage, cold-start opens, and offline logout on real devices.
