import Foundation

/// Retain one client per endpoint/App at application scope. Its durable work outlives view tasks.
public actor TingClient {
    public nonisolated let configuration: TingConfiguration
    private let storage: any InstallationStorage
    private let transport: any TingHTTPTransport
    private let push: any PushRegistrationProvider
    private let identityTokenProvider: @Sendable (String) async throws -> String
    private let api: InstallationAPI
    private let now: @Sendable () -> Date
    private var journal: InstallationJournal
    private var currentUserID: String?
    private var nativeToken: String?
    private var currentState: RegistrationState
    private var publishedStatus: InstallationStatus?
    private var worker: Task<Void, Never>?
    private var retryTask: Task<Void, Never>?
    private var automaticAttempts = 0
    private var active = true
    private var closed = false
    private var storageBlocked = false
    private var ownsNamespace = true
    private var revision: UInt64 = 0
    private var listeners: [UUID: AsyncStream<TingEvent>.Continuation] = [:]
    private var initialOpens: [TingNotification] = []
    private var seenOpens: [String] = []

    public init(
        configuration: TingConfiguration,
        identityTokenProvider: @escaping @Sendable (String) async throws -> String,
        storage: any InstallationStorage = KeychainInstallationStorage(),
        transport: any TingHTTPTransport = URLSessionTingTransport(),
        pushRegistration: (any PushRegistrationProvider)? = nil
    ) throws {
        try self.init(configuration: configuration, identityTokenProvider: identityTokenProvider, storage: storage,
                      transport: transport, pushRegistration: pushRegistration, now: { Date() })
    }

    init(
        configuration: TingConfiguration,
        identityTokenProvider: @escaping @Sendable (String) async throws -> String,
        storage: any InstallationStorage,
        transport: any TingHTTPTransport,
        pushRegistration: (any PushRegistrationProvider)?,
        now: @escaping @Sendable () -> Date
    ) throws {
        try InstallationOwnership.shared.acquire(configuration.namespace)
        do {
            let journal: InstallationJournal
            if let saved = try storage.load(namespace: configuration.namespace) {
                do { journal = try JSONDecoder().decode(InstallationJournal.self, from: saved) }
                catch { throw TingFailure.corruptJournal }
                try Self.validate(journal, namespace: configuration.namespace)
            } else {
                journal = InstallationJournal(namespace: configuration.namespace, installationID: UUID())
                try storage.save(JSONEncoder().encode(journal), namespace: configuration.namespace)
            }
            self.configuration = configuration
            self.identityTokenProvider = identityTokenProvider
            self.storage = storage
            self.transport = transport
            self.push = pushRegistration ?? defaultPushRegistration()
            self.api = InstallationAPI(configuration: configuration)
            self.now = now
            self.journal = journal
            currentState = journal.revocation == nil ? .signedOut : .pendingRevocation
        } catch {
            InstallationOwnership.shared.release(configuration.namespace)
            throw (error as? TingFailure) ?? TingFailure.storageUnavailable
        }
    }

    deinit {
        retryTask?.cancel()
        if ownsNamespace { InstallationOwnership.shared.release(configuration.namespace) }
        for listener in listeners.values { listener.finish() }
    }

    public func status() -> InstallationStatus {
        InstallationStatus(installationID: journal.installationID, userID: currentUserID,
                           state: currentState, retryAt: journal.retryAt)
    }

    public func events() -> AsyncStream<TingEvent> {
        let id = UUID()
        let (stream, continuation) = AsyncStream<TingEvent>.makeStream(bufferingPolicy: .bufferingNewest(64))
        guard !closed else { continuation.finish(); return stream }
        listeners[id] = continuation
        continuation.yield(.statusChanged(status()))
        for notification in initialOpens { continuation.yield(.notificationOpened(notification)) }
        initialOpens.removeAll()
        continuation.onTermination = { [weak self] _ in Task { await self?.removeListener(id) } }
        return stream
    }

    @discardableResult
    public func login(userID: String) async throws -> InstallationStatus {
        try requireOpen()
        guard Self.validUserID(userID) else { throw TingFailure.invalidUserID }
        var next = journal
        next.intent = UUID()
        next.desiredUserID = userID
        do { try persist(next) }
        catch {
            // A failed new identity intent still supersedes older in-memory authentication.
            // Require an explicit durable login/logout before any more registration work.
            currentUserID = nil
            revision &+= 1
            storageBlocked = true
            publish(.failed(.storageUnavailable))
            throw error
        }
        storageBlocked = false
        currentUserID = userID
        revision &+= 1
        automaticAttempts = 0
        return await reconcile()
    }

    @discardableResult
    public func logout() async throws -> InstallationStatus {
        try requireOpen()
        // Clear the live identity even if secure storage is unavailable.
        currentUserID = nil
        revision &+= 1
        retryTask?.cancel()
        var next = journal
        next.intent = UUID()
        next.desiredUserID = nil
        do {
            if !next.credentials.isEmpty, next.revocation == nil || next.revocation!.generation < next.generation {
                let generation = try next.allocateGeneration()
                next.revocation = PendingRevocation(generation: generation, credentials: next.credentials)
            }
            try persist(next)
        } catch {
            storageBlocked = true
            publish(.failed((error as? TingFailure) ?? .storageUnavailable))
            throw error
        }
        storageBlocked = false
        automaticAttempts = 0
        publish(next.revocation == nil ? .signedOut : .pendingRevocation)
        return await reconcile()
    }

    /// Shows the system prompt. Call only from an explicit user action.
    @discardableResult
    public func requestPermission(options: PushPermissionOptions = .standard) async throws -> PushPermission {
        try requireOpen()
        let permission: PushPermission
        do { permission = try await push.requestPermission(options: options) }
        catch { throw (error as? TingFailure) ?? TingFailure.permissionUnavailable }
        revision &+= 1
        _ = await reconcile()
        return permission
    }

    /// Call on process activation. Startup never restores a user from a persisted user ID.
    @discardableResult
    public func applicationDidBecomeActive() async -> InstallationStatus {
        guard !closed else { return status() }
        active = true
        automaticAttempts = 0
        // A token in the journal is only an exact-replay snapshot, not a current APNs token.
        nativeToken = nil
        revision &+= 1
        return await reconcile()
    }

    public func applicationDidEnterBackground() {
        active = false
        retryTask?.cancel()
    }

    /// Forward UIApplicationDelegate's APNs token callback. No delegate replacement or swizzling.
    @discardableResult
    public func didRegisterForRemoteNotifications(deviceToken: Data) async throws -> InstallationStatus {
        try requireOpen()
        guard !deviceToken.isEmpty, deviceToken.count <= 256 else { throw TingFailure.invalidAPNsToken }
        let token = deviceToken.map { String(format: "%02x", $0) }.joined()
        if nativeToken == token, currentState == .registered { return status() }
        nativeToken = token
        revision &+= 1
        return await reconcile()
    }

    public func didFailToRegisterForRemoteNotifications() {
        guard !closed else { return }
        nativeToken = nil
        revision &+= 1
        if currentUserID != nil { publish(.apnsRegistrationFailed) }
    }

    public func notificationReceived(_ notification: TingNotification) {
        guard !closed else { return }
        for listener in listeners.values { listener.yield(.notificationReceived(notification)) }
    }

    /// Buffer up to 32 initial opens; suppress duplicate IDs within the last 256 opens in this process.
    public func notificationOpened(_ notification: TingNotification) {
        guard !closed, !seenOpens.contains(notification.identifier) else { return }
        seenOpens.append(notification.identifier)
        if seenOpens.count > 256 { seenOpens.removeFirst() }
        if listeners.isEmpty {
            initialOpens.append(notification)
            if initialOpens.count > 32 { initialOpens.removeFirst() }
        } else {
            for listener in listeners.values { listener.yield(.notificationOpened(notification)) }
        }
    }

    /// Explicit retry or connectivity recovery. Coalesces with work already in flight.
    @discardableResult
    public func reconcile() async -> InstallationStatus {
        guard !closed, !storageBlocked else { return status() }
        if worker == nil { worker = Task { await self.drain() } }
        let task = worker
        await task?.value
        return status()
    }

    /// Drain the current operation before releasing the namespace. Durable pending work is retained.
    public func close() async {
        guard !closed else { return }
        closed = true
        retryTask?.cancel()
        await worker?.value
        if ownsNamespace {
            InstallationOwnership.shared.release(configuration.namespace)
            ownsNamespace = false
        }
        for listener in listeners.values { listener.finish() }
        listeners.removeAll()
    }

    private func drain() async {
        defer { worker = nil }
        while !closed && !storageBlocked {
            let startedRevision = revision
            do {
                if let deadline = journal.retryAt, deadline > now() {
                    publish(currentUserID == nil ? (journal.revocation == nil ? .signedOut : .pendingRevocation) : .pendingRegistration)
                    scheduleRetry(at: deadline)
                    return
                }
                let more = try await reconcileOnce()
                if !more && startedRevision == revision { return }
            } catch let retry as RetryLater {
                do {
                    var next = journal
                    let failures = min((next.failures ?? 0) + 1, 1_000)
                    next.failures = failures
                    next.retryAt = RetryPolicy.deadline(failures: failures, serverDeadline: retry.date, now: now())
                    try persist(next)
                    publish(currentUserID == nil ? .pendingRevocation : .pendingRegistration)
                    scheduleRetry(at: next.retryAt!)
                } catch { publish(.failed(.storageUnavailable)) }
                return
            } catch {
                publish(.failed((error as? TingFailure) ?? .storageUnavailable))
                // A stale operation's error must not swallow a newer login/logout intent.
                if startedRevision == revision { return }
            }
        }
    }

    private func reconcileOnce() async throws -> Bool {
        guard let userID = currentUserID else {
            guard let revocation = journal.revocation else { publish(.signedOut); return false }
            return try await revoke(revocation)
        }
        let intent = journal.intent
        let permission: PushPermission
        do { permission = try await push.permission() }
        catch { throw (error as? TingFailure) ?? TingFailure.permissionUnavailable }
        guard isCurrent(userID, intent) else { return true }
        guard permission.allowsRegistration else { publish(.permissionRequired(permission)); return false }
        guard let token = nativeToken else {
            // Activation discards the token to request a fresh one. Don't flap a confirmed status meanwhile.
            if currentState != .registered || !hasActiveRegistration(userID) { publish(.awaitingAPNsToken) }
            await push.registerForRemoteNotifications()
            return false
        }

        let prepared: PreparedRegistration
        if let existing = journal.prepared, existing.userID == userID, journal.revocation == nil,
           existing.body.pushToken == token, existing.body.apnsEnvironment == configuration.apnsEnvironment {
            prepared = existing
        } else if journal.prepared == nil, hasActiveRegistration(userID), let confirmed = journal.confirmed,
                  confirmed.body.pushToken == token,
                  let confirmedAt = confirmed.confirmedAt,
                  (0..<RetryPolicy.registrationRefresh).contains(now().timeIntervalSince(confirmedAt)) {
            // Unchanged and recently acknowledged. A clock moved backwards also falls through to re-register.
            publish(.registered)
            return false
        } else {
            var next = journal
            let previous = next.credentials
            // A fresh credential also permits authenticated reactivation if the provider retired
            // the server association since our last acknowledgment. Exact retries retain theirs.
            let credential = try installationCredential()
            let generation = try next.allocateGeneration()
            prepared = PreparedRegistration(userID: userID, body: RegistrationBody(platform: "ios", pushToken: token,
                apnsEnvironment: configuration.apnsEnvironment, installationToken: credential, generation: generation),
                previousCredentials: previous)
            next.prepared = prepared
            try persist(next)
        }

        publish(.registering)
        // One identity proof serves every ownership candidate; only an `unauthorized` proof is refreshed, once.
        var cachedProof: String?
        var proofRefreshed = false
        var candidateIndex = 0
        var candidates: [String?] = prepared.previousCredentials.isEmpty ? [nil] : prepared.previousCredentials
        if candidates.count > 1 { candidates.append(candidates[0]) }
        while candidateIndex < candidates.count {
            if cachedProof == nil {
                do { cachedProof = try await identityTokenProvider(userID) }
                catch {
                    guard isCurrent(userID, intent) else { return true }
                    throw TingFailure.identityUnavailable
                }
            }
            guard isCurrent(userID, intent), !closed else { return true }
            guard let proof = cachedProof, !proof.isEmpty, proof.utf8.count <= 8192, !proof.contains(where: { $0.isWhitespace }) else {
                throw TingFailure.identityUnavailable
            }
            let response = try await send(api.register(journal.installationID, body: prepared.body, proof: proof,
                                                       ownership: candidates[candidateIndex]))
            if response.status == 401, response.errorCode == "unauthorized", !proofRefreshed {
                proofRefreshed = true
                cachedProof = nil
                continue
            }
            if response.status == 401, response.errorCode == "installation_credential_invalid" {
                candidateIndex += 1
                continue
            }
            guard response.status == 200 || response.status == 201 else {
                throw TingFailure.server(status: response.status, code: response.errorCode)
            }
            guard api.acknowledges(response, journal.installationID, prepared.body) else { throw TingFailure.invalidServerResponse }
            var next = journal
            next.credential = prepared.body.installationToken
            next.confirmed = ConfirmedRegistration(userID: prepared.userID, body: prepared.body, active: true, confirmedAt: now())
            next.prepared = nil
            next.retryAt = nil
            next.failures = nil
            // Only a newer acknowledged registration can supersede older pending cleanup.
            if let revocation = next.revocation, prepared.body.generation > revocation.generation { next.revocation = nil }
            try persist(next)
            if isCurrent(userID, intent) { publish(.registered) }
            return !isCurrent(userID, intent) || nativeToken != prepared.body.pushToken || next.revocation != nil
        }
        throw TingFailure.server(status: 401, code: "installation_credential_invalid")
    }

    private func revoke(_ revocation: PendingRevocation) async throws -> Bool {
        var candidates = revocation.credentials
        if candidates.count > 1 { candidates.append(candidates[0]) }
        for credential in candidates {
            let response = try await send(api.revoke(journal.installationID, credential: credential,
                                                     generation: revocation.generation))
            if response.status == 401, response.errorCode == "installation_credential_invalid" { continue }
            guard response.status == 204 else { throw TingFailure.server(status: response.status, code: response.errorCode) }
            var next = journal
            // DELETE may be the first acknowledgment proving that a rotation committed.
            // Retain the credential it authenticated before dropping the unresolved PUT.
            next.credential = credential
            next.confirmed?.active = false
            next.prepared = nil
            next.revocation = nil
            next.retryAt = nil
            next.failures = nil
            try persist(next)
            if currentUserID == nil { publish(.signedOut) }
            return currentUserID != nil
        }
        // Unknown initial creation is not proof of revocation. Retain all candidates.
        publish(.pendingRevocation)
        return currentUserID != nil
    }

    private struct RetryLater: Error { let date: Date? }

    private func send(_ request: TingHTTPRequest) async throws -> TingHTTPResponse {
        let response: TingHTTPResponse
        do { response = try await transport.send(request) }
        catch let failure as TingFailure { throw failure }
        catch { throw RetryLater(date: nil) }
        if response.status == 429 || (500...599).contains(response.status) { throw RetryLater(date: response.retryAfter) }
        return response
    }

    private func scheduleRetry(at date: Date) {
        retryTask?.cancel()
        guard active, !closed, automaticAttempts < RetryPolicy.automaticAttempts else { return }
        automaticAttempts += 1
        let nanoseconds = UInt64(max(0, date.timeIntervalSince(now())) * 1_000_000_000)
        retryTask = Task { [weak self] in
            do { try await Task.sleep(nanoseconds: nanoseconds) } catch { return }
            guard !Task.isCancelled else { return }
            _ = await self?.reconcile()
        }
    }

    /// The last acknowledged registration is this user's, in this environment, with nothing pending.
    private func hasActiveRegistration(_ userID: String) -> Bool {
        guard let confirmed = journal.confirmed else { return false }
        return confirmed.active && confirmed.userID == userID && journal.revocation == nil
            && confirmed.body.apnsEnvironment == configuration.apnsEnvironment
            && journal.credential == confirmed.body.installationToken
    }

    private func isCurrent(_ userID: String, _ intent: UUID) -> Bool {
        currentUserID == userID && journal.intent == intent
    }

    private func persist(_ next: InstallationJournal) throws {
        do { try storage.save(JSONEncoder().encode(next), namespace: configuration.namespace) }
        catch { throw TingFailure.storageUnavailable }
        journal = next
    }

    /// Emits whenever any part of the status changes, including the user or retry deadline.
    private func publish(_ state: RegistrationState) {
        currentState = state
        let next = status()
        guard next != publishedStatus else { return }
        publishedStatus = next
        for listener in listeners.values { listener.yield(.statusChanged(next)) }
    }

    private func requireOpen() throws { if closed { throw TingFailure.clientClosed } }
    private func removeListener(_ id: UUID) { listeners.removeValue(forKey: id) }

    private static func validUserID(_ id: String) -> Bool {
        !id.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty && id.utf8.count <= 256 && !id.contains("\0")
    }

    private static func validate(_ journal: InstallationJournal, namespace: String) throws {
        guard journal.version == 1, journal.namespace == namespace,
              journal.generation >= 0, journal.generation <= InstallationJournal.maxGeneration,
              (journal.failures ?? 0) >= 0,
              journal.credentials.allSatisfy(validInstallationCredential),
              journal.desiredUserID.map(validUserID) ?? true else { throw TingFailure.corruptJournal }
        for body in [journal.prepared?.body, journal.confirmed?.body].compactMap({ $0 }) {
            guard body.platform == "ios", body.generation > 0, body.generation <= journal.generation,
                  !body.pushToken.isEmpty, body.pushToken.utf8.count <= 512,
                  body.pushToken.utf8.count.isMultiple(of: 2),
                  body.pushToken.utf8.allSatisfy({ (48...57).contains($0) || (97...102).contains($0) })
            else { throw TingFailure.corruptJournal }
        }
        guard journal.prepared.map({ validUserID($0.userID) }) ?? true,
              journal.confirmed.map({ validUserID($0.userID) }) ?? true
        else { throw TingFailure.corruptJournal }
        if let revocation = journal.revocation {
            guard revocation.generation > 0, revocation.generation <= journal.generation, !revocation.credentials.isEmpty
            else { throw TingFailure.corruptJournal }
        }
    }
}
