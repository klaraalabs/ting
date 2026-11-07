import Foundation

public enum PushPermission: String, Sendable {
    case notDetermined, denied, authorized, provisional, ephemeral

    public var allowsRegistration: Bool {
        self == .authorized || self == .provisional || self == .ephemeral
    }
}

public enum RegistrationState: Equatable, Sendable {
    case signedOut
    case permissionRequired(PushPermission)
    case awaitingAPNsToken
    case registering
    case registered
    case pendingRegistration
    case pendingRevocation
    case apnsRegistrationFailed
    case failed(TingFailure)
}

public struct InstallationStatus: Equatable, Sendable {
    public let installationID: UUID
    public let userID: String?
    public let state: RegistrationState
    public let retryAt: Date?
}

/// Mirrors the UserNotifications authorization options the host may request.
public struct PushPermissionOptions: OptionSet, Sendable {
    public let rawValue: Int
    public init(rawValue: Int) { self.rawValue = rawValue }

    public static let alert = PushPermissionOptions(rawValue: 1 << 0)
    public static let sound = PushPermissionOptions(rawValue: 1 << 1)
    public static let badge = PushPermissionOptions(rawValue: 1 << 2)
    /// Quiet delivery without a prompt. The resulting permission is `.provisional`.
    public static let provisional = PushPermissionOptions(rawValue: 1 << 3)
    public static let providesAppNotificationSettings = PushPermissionOptions(rawValue: 1 << 4)
    /// Requires Apple's critical-alerts entitlement.
    public static let criticalAlert = PushPermissionOptions(rawValue: 1 << 5)

    public static let standard: PushPermissionOptions = [.alert, .sound, .badge]
}

public protocol PushRegistrationProvider: Sendable {
    func permission() async throws -> PushPermission
    func requestPermission(options: PushPermissionOptions) async throws -> PushPermission
    func registerForRemoteNotifications() async
}

struct RegistrationBody: Codable, Sendable, Equatable {
    let platform: String
    let pushToken: String
    let apnsEnvironment: APNsEnvironment
    let installationToken: String
    let generation: Int64
}

struct PreparedRegistration: Codable, Sendable {
    let userID: String
    let body: RegistrationBody
    let previousCredentials: [String]
}

struct ConfirmedRegistration: Codable, Sendable {
    let userID: String
    let body: RegistrationBody
    var active: Bool
    // Optional so that journals written before this field decode; nil is treated as due for refresh.
    var confirmedAt: Date?
}

struct PendingRevocation: Codable, Sendable {
    let generation: Int64
    let credentials: [String]
}

struct InstallationJournal: Codable, Sendable {
    var version = 1
    let namespace: String
    let installationID: UUID
    var generation: Int64 = 0
    var desiredUserID: String?
    var intent = UUID()
    // Ownership proved by the latest successful PUT or DELETE, even if a PUT acknowledgment was lost.
    var credential: String?
    var confirmed: ConfirmedRegistration?
    var prepared: PreparedRegistration?
    var revocation: PendingRevocation?
    var retryAt: Date?
    // Consecutive retryable failures. Optional so that older journals decode.
    var failures: Int?

    static let maxGeneration: Int64 = 9_007_199_254_740_991

    mutating func allocateGeneration() throws -> Int64 {
        guard generation < Self.maxGeneration else { throw TingFailure.generationExhausted }
        generation += 1
        return generation
    }

    var credentials: [String] {
        let values = [prepared?.body.installationToken, credential, confirmed?.body.installationToken].compactMap { $0 }
            + (prepared?.previousCredentials ?? []) + (revocation?.credentials ?? [])
        var seen = Set<String>()
        return values.filter { seen.insert($0).inserted }
    }
}
