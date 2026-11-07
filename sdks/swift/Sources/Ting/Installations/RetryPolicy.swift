import Foundation

enum RetryPolicy {
    /// Automatic retries per user action or foreground reconciliation cycle.
    static let automaticAttempts = 2
    /// Client backoff ceiling. The failure streak is persisted, so the delay keeps growing across cycles.
    static let maximumDelay: TimeInterval = 300
    /// A misbehaving proxy must not block registration and logout for longer than this.
    static let maximumServerDelay: TimeInterval = 3600
    /// A confirmed registration for the same user, token, and environment is re-sent after this interval,
    /// so that an association the provider invalidated server-side is reactivated.
    static let registrationRefresh: TimeInterval = 86_400

    static func deadline(failures: Int, serverDeadline: Date?, now: Date) -> Date {
        let exponent = Double(min(max(failures - 1, 0), 16))
        let delay = min(maximumDelay, pow(2, exponent)) * Double.random(in: 0.8...1.2)
        let server = serverDeadline.map { min($0, now.addingTimeInterval(maximumServerDelay)) } ?? .distantPast
        return max(server, now.addingTimeInterval(delay))
    }
}
