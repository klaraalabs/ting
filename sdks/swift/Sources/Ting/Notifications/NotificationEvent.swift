import Foundation
#if canImport(UserNotifications)
import UserNotifications
#endif

/// Copy native notification values into this Sendable value before crossing actor boundaries.
/// The host owns presentation and navigation, including notification delegate completion handlers.
public struct TingNotification: Sendable, Equatable {
    public let identifier: String
    public let title: String
    public let body: String
    /// Custom payload keys, excluding `aps`. Strings are kept as-is, numbers and booleans use their
    /// textual form, and arrays/objects are JSON-encoded with sorted keys.
    public let data: [String: String]

    public init(identifier: String, title: String, body: String, data: [String: String]) {
        self.identifier = identifier
        self.title = title
        self.body = body
        self.data = data
    }

    static func payload(_ userInfo: [AnyHashable: Any]) -> [String: String] {
        var data: [String: String] = [:]
        for (key, value) in userInfo {
            guard let key = key as? String, key != "aps" else { continue }
            if let value = value as? String {
                data[key] = value
            } else if let number = value as? NSNumber {
                data[key] = CFGetTypeID(number) == CFBooleanGetTypeID() ? (number.boolValue ? "true" : "false") : number.stringValue
            } else if JSONSerialization.isValidJSONObject(value),
                      let json = try? JSONSerialization.data(withJSONObject: value, options: [.sortedKeys]) {
                data[key] = String(decoding: json, as: UTF8.self)
            }
        }
        return data
    }
}

#if canImport(UserNotifications)
extension TingNotification {
    /// Call synchronously inside the notification delegate, before any `await`.
    public init(_ notification: UNNotification) {
        let content = notification.request.content
        self.init(identifier: notification.request.identifier, title: content.title, body: content.body,
                  data: Self.payload(content.userInfo))
    }
}
#endif

public enum TingEvent: Sendable, Equatable {
    case statusChanged(InstallationStatus)
    case notificationReceived(TingNotification)
    case notificationOpened(TingNotification)
}
