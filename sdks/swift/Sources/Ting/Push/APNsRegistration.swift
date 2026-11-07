import Foundation

#if os(iOS)
import UIKit
import UserNotifications

public struct APNsRegistration: PushRegistrationProvider {
    public init() {}

    public func permission() async throws -> PushPermission {
        let settings = await UNUserNotificationCenter.current().notificationSettings()
        switch settings.authorizationStatus {
        case .notDetermined: return .notDetermined
        case .denied: return .denied
        case .authorized: return .authorized
        case .provisional: return .provisional
        case .ephemeral: return .ephemeral
        @unknown default: return .denied
        }
    }

    public func requestPermission(options: PushPermissionOptions) async throws -> PushPermission {
        var native: UNAuthorizationOptions = []
        if options.contains(.alert) { native.insert(.alert) }
        if options.contains(.sound) { native.insert(.sound) }
        if options.contains(.badge) { native.insert(.badge) }
        if options.contains(.provisional) { native.insert(.provisional) }
        if options.contains(.providesAppNotificationSettings) { native.insert(.providesAppNotificationSettings) }
        if options.contains(.criticalAlert) { native.insert(.criticalAlert) }
        _ = try await UNUserNotificationCenter.current().requestAuthorization(options: native)
        return try await permission()
    }

    public func registerForRemoteNotifications() async {
        await MainActor.run { UIApplication.shared.registerForRemoteNotifications() }
    }
}
#endif

struct UnsupportedPushRegistration: PushRegistrationProvider {
    func permission() async throws -> PushPermission { throw TingFailure.unsupportedPlatform }
    func requestPermission(options: PushPermissionOptions) async throws -> PushPermission { throw TingFailure.unsupportedPlatform }
    func registerForRemoteNotifications() async {}
}

func defaultPushRegistration() -> any PushRegistrationProvider {
    #if os(iOS)
    APNsRegistration()
    #else
    UnsupportedPushRegistration()
    #endif
}
