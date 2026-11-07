import Foundation
@testable import Ting

final class MemoryStorage: InstallationStorage, @unchecked Sendable {
    private let lock = NSLock()
    private var records: [String: Data] = [:]
    private var fail = false
    private var writeCount = 0
    private var failAt: Int?

    func load(namespace: String) throws -> Data? { lock.withLock { records[namespace] } }
    func save(_ data: Data, namespace: String) throws {
        try lock.withLock {
            writeCount += 1
            if fail || writeCount == failAt { fail = false; failAt = nil; throw TingFailure.storageUnavailable }
            records[namespace] = data
        }
    }
    func failNextSave() { lock.withLock { fail = true } }
    func failSave(number: Int) { lock.withLock { failAt = number } }
    var saves: Int { lock.withLock { writeCount } }
}

actor TestPush: PushRegistrationProvider {
    var authorization: PushPermission = .authorized
    var prompts = 0
    var promptOptions: [PushPermissionOptions] = []
    var registrations = 0
    func permission() async throws -> PushPermission { authorization }
    func requestPermission(options: PushPermissionOptions) async throws -> PushPermission {
        prompts += 1
        promptOptions.append(options)
        return authorization
    }
    func registerForRemoteNotifications() async { registrations += 1 }
    func set(_ value: PushPermission) { authorization = value }
}

/// Real time plus a manually advanced offset, so timers still work while staleness can be simulated.
final class TestClock: @unchecked Sendable {
    private let lock = NSLock()
    private var offset: TimeInterval = 0
    func now() -> Date { lock.withLock { Date().addingTimeInterval(offset) } }
    func advance(_ interval: TimeInterval) { lock.withLock { offset += interval } }
}

actor Gate {
    private var entered = false
    private var waiters: [CheckedContinuation<Void, Never>] = []
    private var blocked: CheckedContinuation<Void, Never>?
    func pause() async {
        entered = true
        for waiter in waiters { waiter.resume() }
        waiters.removeAll()
        await withCheckedContinuation { blocked = $0 }
    }
    func waitUntilEntered() async {
        if entered { return }
        await withCheckedContinuation { waiters.append($0) }
    }
    func release() { blocked?.resume(); blocked = nil }
}

/// This server double is checked against the same protocol fixture exercised by the real Go API.
/// Network loss happens after committing, independently of whether the client received an acknowledgment.
actor ProtocolServer: TingHTTPTransport {
    struct Record {
        var user: String
        var body: RegistrationBody
        var generation: Int64
        var active: Bool
    }
    private(set) var records: [String: Record] = [:]
    private(set) var requests: [TingHTTPRequest] = []
    private var dropPUT = false
    private var putGate: Gate?
    private var afterPUT: (@Sendable () -> Void)?
    private var nextResponse: TingHTTPResponse?

    func loseNextPUTResponse() { dropPUT = true }
    func pauseNextPUT(_ gate: Gate) { putGate = gate }
    func afterNextPUT(_ callback: @escaping @Sendable () -> Void) { afterPUT = callback }
    func respondNext(_ response: TingHTTPResponse) { nextResponse = response }
    func invalidateCurrentRegistration() {
        for key in records.keys { records[key]?.active = false }
    }
    var requestCount: Int { requests.count }
    var putCount: Int { requests.filter { $0.method == "PUT" }.count }
    var putUsers: [String] {
        requests.filter { $0.method == "PUT" }.map { String(($0.headers["Authorization"] ?? "").dropFirst(7)) }
    }

    func send(_ request: TingHTTPRequest) async throws -> TingHTTPResponse {
        requests.append(request)
        if let next = nextResponse { nextResponse = nil; return next }
        if request.method == "PUT", let gate = putGate { putGate = nil; await gate.pause() }
        let key = (request.headers["X-Ting-App-Id"] ?? "") + request.url.lastPathComponent
        let bearer = String((request.headers["Authorization"] ?? "").dropFirst(7))
        if request.method == "DELETE" {
            guard var record = records[key], bearer == record.body.installationToken else { return failure(401, "installation_credential_invalid") }
            guard let generation = Int64(request.headers["X-Ting-Installation-Generation"] ?? ""), generation > 0 else { return failure(400, "invalid_request") }
            if generation < record.generation { return failure(409, "installation_superseded") }
            if generation == record.generation && record.active { return failure(409, "installation_generation_conflict") }
            record.active = false
            record.generation = generation
            records[key] = record
            return TingHTTPResponse(status: 204)
        }
        let body = try JSONDecoder().decode(RegistrationBody.self, from: request.body!)
        guard body.generation > 0, validInstallationCredential(body.installationToken) else { return failure(400, "invalid_request") }
        let old = records[key]
        if let old {
            let currentProof = request.headers["X-Ting-Installation-Token"] == old.body.installationToken
            let proposedProof = body.installationToken == old.body.installationToken
            if !currentProof && !proposedProof { return failure(401, "installation_credential_invalid") }
            if body.generation < old.generation { return failure(409, "installation_superseded") }
            if body.generation == old.generation {
                if !old.active { return failure(409, "installation_superseded") }
                if !proposedProof || bearer != old.user || body != old.body { return failure(409, "installation_generation_conflict") }
                return acknowledgment(request.url.lastPathComponent, body, status: 200)
            }
            if !currentProof { return failure(401, "installation_credential_invalid") }
            if (old.user != bearer || !old.active) && proposedProof { return failure(400, "invalid_request") }
        }
        records[key] = Record(user: bearer, body: body, generation: body.generation, active: true)
        afterPUT?()
        afterPUT = nil
        if dropPUT { dropPUT = false; throw URLError(.networkConnectionLost) }
        return acknowledgment(request.url.lastPathComponent, body, status: old == nil ? 201 : 200)
    }

    private func acknowledgment(_ id: String, _ body: RegistrationBody, status: Int) -> TingHTTPResponse {
        let data = try! JSONSerialization.data(withJSONObject: ["installationId": id, "installationToken": body.installationToken,
            "generation": body.generation, "status": "registered"])
        return TingHTTPResponse(status: status, body: data)
    }
    private func failure(_ status: Int, _ code: String) -> TingHTTPResponse {
        TingHTTPResponse(status: status, body: try! JSONSerialization.data(withJSONObject: ["error": ["code": code]]))
    }
}
