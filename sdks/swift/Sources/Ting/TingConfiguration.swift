import Foundation

public enum APNsEnvironment: String, Codable, Sendable {
    case sandbox, production
}

public struct TingConfiguration: Sendable {
    public let endpoint: URL
    public let appID: String
    public let apnsEnvironment: APNsEnvironment

    public init(endpoint: URL, appID: String, apnsEnvironment: APNsEnvironment) throws {
        guard var parts = URLComponents(url: endpoint, resolvingAgainstBaseURL: false),
              let host = parts.host?.lowercased(), !host.isEmpty,
              parts.user == nil, parts.password == nil, parts.query == nil, parts.fragment == nil,
              let scheme = parts.scheme?.lowercased(),
              scheme == "https" || (scheme == "http" && ["localhost", "127.0.0.1", "::1", "[::1]"].contains(host)),
              !appID.isEmpty, appID.utf8.count <= 256,
              !appID.contains(where: { $0.isWhitespace || $0.isNewline })
        else { throw TingFailure.invalidConfiguration }
        parts.host = host
        parts.scheme = scheme
        if (scheme == "https" && parts.port == 443) || (scheme == "http" && parts.port == 80) { parts.port = nil }
        while parts.path.hasSuffix("/") { parts.path.removeLast() }
        guard let normalized = parts.url?.standardized else { throw TingFailure.invalidConfiguration }
        self.endpoint = normalized
        self.appID = appID
        self.apnsEnvironment = apnsEnvironment
    }

    var namespace: String { endpoint.absoluteString + "\n" + appID }
}

public enum TingFailure: Error, Equatable, Sendable {
    case invalidConfiguration
    case invalidUserID
    case invalidAPNsToken
    case clientAlreadyExists
    case clientClosed
    case storageUnavailable
    case corruptJournal
    case generationExhausted
    case randomnessUnavailable
    case identityUnavailable
    case permissionUnavailable
    case invalidServerResponse
    case server(status: Int, code: String)
    case unsupportedPlatform
}

extension TingFailure: LocalizedError {
    public var errorDescription: String? {
        switch self {
        case .invalidConfiguration: "The Ting endpoint or App ID is invalid."
        case .invalidUserID: "The user ID is empty, too long, or contains a NUL character."
        case .invalidAPNsToken: "The APNs device token is empty or too long."
        case .clientAlreadyExists: "Another Ting client is already open for this endpoint and App."
        case .clientClosed: "The Ting client has been closed."
        case .storageUnavailable: "Ting's secure storage is unavailable, for example before the device's first unlock."
        case .corruptJournal: "Ting's stored installation record is unreadable and was preserved for diagnosis."
        case .generationExhausted: "The installation's generation counter is exhausted."
        case .randomnessUnavailable: "The system could not generate an installation credential."
        case .identityUnavailable: "The identity token provider failed or returned an invalid token."
        case .permissionUnavailable: "Notification permission could not be read or requested."
        case .invalidServerResponse: "The Ting server returned an unexpected response."
        case let .server(status, code): "The Ting server rejected the request (HTTP \(status), \(code))."
        case .unsupportedPlatform: "Push registration is not supported on this platform."
        }
    }
}
