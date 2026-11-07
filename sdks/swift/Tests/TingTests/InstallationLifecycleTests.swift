import Foundation
import XCTest
@testable import Ting

@MainActor
final class InstallationLifecycleTests: XCTestCase {
    func configuration(_ appID: String = UUID().uuidString) throws -> TingConfiguration {
        try TingConfiguration(endpoint: URL(string: "https://ting.test/push/")!, appID: appID, apnsEnvironment: .sandbox)
    }

    func client(_ config: TingConfiguration, _ storage: MemoryStorage, _ server: ProtocolServer, _ push: TestPush,
                clock: TestClock = TestClock(),
                identity: @escaping @Sendable (String) async throws -> String = { $0 }) throws -> TingClient {
        try TingClient(configuration: config, identityTokenProvider: identity, storage: storage,
                       transport: server, pushRegistration: push, now: { clock.now() })
    }

    func journal(_ storage: MemoryStorage, _ config: TingConfiguration) throws -> InstallationJournal {
        try JSONDecoder().decode(InstallationJournal.self, from: XCTUnwrap(storage.load(namespace: config.namespace)))
    }

    func token(_ client: TingClient, _ value: UInt8 = 0xAB) async throws {
        _ = try await client.didRegisterForRemoteNotifications(deviceToken: Data(repeating: value, count: 32))
    }

    func waitForUser(_ user: String?, _ client: TingClient) async throws {
        for _ in 0..<10000 {
            if await client.status().userID == user { return }
            await Task.yield()
        }
        XCTFail("identity intent was not applied")
    }

    func testLoginPermissionTokenRotationAndLogout() async throws {
        let config = try configuration(), storage = MemoryStorage(), server = ProtocolServer(), push = TestPush()
        let client = try client(config, storage, server, push)
        let initialRequests = await server.requestCount
        XCTAssertEqual(initialRequests, 0)
        await push.set(.denied)
        let denied = try await client.login(userID: "alice")
        XCTAssertEqual(denied.state, .permissionRequired(.denied))
        let prompts = await push.prompts
        XCTAssertEqual(prompts, 0)
        await push.set(.provisional)
        let waiting = await client.reconcile()
        XCTAssertEqual(waiting.state, .awaitingAPNsToken)
        try await token(client)
        let registered = await client.status()
        XCTAssertEqual(registered.state, .registered)
        let first = try journal(storage, config)
        XCTAssertEqual(first.confirmed?.body.apnsEnvironment, .sandbox)
        XCTAssertEqual(first.confirmed?.body.pushToken, String(repeating: "ab", count: 32))
        try await token(client, 0xCD)
        let refreshed = try journal(storage, config)
        XCTAssertNotEqual(refreshed.confirmed?.body.installationToken, first.confirmed?.body.installationToken)
        XCTAssertGreaterThan(refreshed.generation, first.generation)
        let switched = try await client.login(userID: "bob")
        XCTAssertEqual(switched.state, .registered)
        let second = try journal(storage, config)
        XCTAssertNotEqual(second.confirmed?.body.installationToken, first.confirmed?.body.installationToken)
        let loggedOut = try await client.logout()
        XCTAssertNil(loggedOut.userID)
        XCTAssertEqual(loggedOut.state, .signedOut)
        let record = await server.records.values.first
        XCTAssertEqual(record?.active, false)
        await client.close()
    }

    func testLostResponseSurvivesRestartWithoutNewInstallation() async throws {
        let config = try configuration(), storage = MemoryStorage(), server = ProtocolServer(), push = TestPush()
        let first = try client(config, storage, server, push)
        await first.applicationDidEnterBackground()
        try await token(first)
        await server.loseNextPUTResponse()
        let result = try await first.login(userID: "alice")
        XCTAssertEqual(result.state, .pendingRegistration)
        let prepared = try journal(storage, config)
        XCTAssertNotNil(prepared.prepared)
        XCTAssertTrue(validInstallationCredential(prepared.prepared!.body.installationToken))
        await first.close()
        // Simulate the backoff deadline elapsing without sleeping in a crash/recovery test.
        var elapsed = prepared
        elapsed.retryAt = nil
        try storage.save(JSONEncoder().encode(elapsed), namespace: config.namespace)
        let restarted = try self.client(config, storage, server, push)
        _ = await restarted.applicationDidBecomeActive()
        let count = await server.requestCount
        XCTAssertEqual(count, 1, "persisted identity must not automatically register after restart")
        try await token(restarted, 0xCD)
        let recovered = try await restarted.login(userID: "alice")
        XCTAssertEqual(recovered.state, .registered)
        XCTAssertEqual(recovered.installationID, result.installationID)
        let final = try journal(storage, config)
        XCTAssertNotEqual(final.confirmed?.body.installationToken, prepared.prepared?.body.installationToken)
        XCTAssertEqual(final.confirmed?.body.pushToken, String(repeating: "cd", count: 32))
        let records = await server.records.count
        XCTAssertEqual(records, 1)
        await restarted.close()
    }

    func testDelayedIdentityCallbackCannotRestoreOldUser() async throws {
        let config = try configuration(), storage = MemoryStorage(), server = ProtocolServer(), push = TestPush(), gate = Gate()
        let client = try client(config, storage, server, push) { user in
            if user == "alice" { await gate.pause() }
            return user
        }
        try await token(client)
        let alice = Task { try await client.login(userID: "alice") }
        await gate.waitUntilEntered()
        let bob = Task { try await client.login(userID: "bob") }
        try await waitForUser("bob", client)
        await gate.release()
        _ = try await alice.value
        let result = try await bob.value
        XCTAssertEqual(result.userID, "bob")
        XCTAssertEqual(result.state, .registered)
        let users = await server.putUsers
        XCTAssertEqual(users, ["bob"])
        await client.close()
    }

    func testLogoutDuringInFlightCreationUsesPreparedCredential() async throws {
        let config = try configuration(), storage = MemoryStorage(), server = ProtocolServer(), push = TestPush(), gate = Gate()
        let client = try client(config, storage, server, push)
        try await token(client)
        await server.pauseNextPUT(gate)
        let login = Task { try await client.login(userID: "alice") }
        await gate.waitUntilEntered()
        let logout = Task { try await client.logout() }
        try await waitForUser(nil, client)
        let saved = try journal(storage, config)
        XCTAssertNil(saved.desiredUserID)
        XCTAssertGreaterThan(saved.revocation!.generation, saved.prepared!.body.generation)
        await gate.release()
        _ = try await login.value
        let result = try await logout.value
        XCTAssertEqual(result.state, .signedOut)
        let record = await server.records.values.first
        XCTAssertEqual(record?.active, false)
        await client.close()
    }

    func testStorageFailureBeforeSendAndAfterAcknowledgmentRetainsRecovery() async throws {
        let config = try configuration(), storage = MemoryStorage(), server = ProtocolServer(), push = TestPush()
        let client = try client(config, storage, server, push)
        try await token(client)
        storage.failNextSave()
        do { _ = try await client.login(userID: "alice"); XCTFail("expected storage failure") }
        catch { XCTAssertEqual(error as? TingFailure, .storageUnavailable) }
        let requests = await server.requestCount
        XCTAssertEqual(requests, 0)
        await server.afterNextPUT { storage.failNextSave() }
        let failed = try await client.login(userID: "alice")
        XCTAssertEqual(failed.state, .failed(.storageUnavailable))
        let saved = try journal(storage, config)
        XCTAssertNotNil(saved.prepared)
        let recovered = await client.reconcile()
        XCTAssertEqual(recovered.state, .registered)
        XCTAssertEqual(try journal(storage, config).confirmed?.body.installationToken, saved.prepared?.body.installationToken)
        storage.failNextSave()
        do { _ = try await client.logout(); XCTFail("expected storage failure") }
        catch { XCTAssertEqual(error as? TingFailure, .storageUnavailable) }
        let status = await client.status()
        XCTAssertNil(status.userID)
        XCTAssertNotNil(try journal(storage, config).confirmed)
        _ = try await client.logout()
        await client.close()
    }

    func testUnknownCreationRemainsPendingAcrossRestart() async throws {
        let config = try configuration(), storage = MemoryStorage(), server = ProtocolServer(), push = TestPush(), gate = Gate()
        let client = try client(config, storage, server, push) { user in await gate.pause(); return user }
        try await token(client)
        let login = Task { try await client.login(userID: "alice") }
        await gate.waitUntilEntered()
        let logout = Task { try await client.logout() }
        try await waitForUser(nil, client)
        await gate.release()
        _ = try await login.value
        let pending = try await logout.value
        XCTAssertEqual(pending.state, .pendingRevocation)
        await client.close()
        let restarted = try self.client(config, storage, server, push)
        let status = await restarted.applicationDidBecomeActive()
        XCTAssertEqual(status.state, .pendingRevocation)
        XCTAssertNil(status.userID)
        try await token(restarted)
        let loggedIn = try await restarted.login(userID: "bob")
        XCTAssertEqual(loggedIn.state, .registered)
        XCTAssertNil(try journal(storage, config).revocation)
        let users = await server.putUsers
        XCTAssertEqual(users, ["bob"])
        await restarted.close()
    }

    func testRateLimitDeadlineIsDurableAndHonoredOnLogout() async throws {
        let config = try configuration(), storage = MemoryStorage(), server = ProtocolServer(), push = TestPush()
        let client = try client(config, storage, server, push)
        await client.applicationDidEnterBackground()
        try await token(client)
        await server.respondNext(TingHTTPResponse(status: 429, headers: ["Retry-After": "3600"]))
        let result = try await client.login(userID: "alice")
        XCTAssertEqual(result.state, .pendingRegistration)
        XCTAssertGreaterThan(result.retryAt!.timeIntervalSinceNow, 3590)
        _ = await client.reconcile()
        let logout = try await client.logout()
        XCTAssertEqual(logout.state, .pendingRevocation)
        let count = await server.requestCount
        XCTAssertEqual(count, 1)
        XCTAssertNotNil(try journal(storage, config).retryAt)
        await client.close()
    }

    func testNamespaceOwnershipAndCorruptStorageAreNotSilentlyReset() async throws {
        let config = try configuration(), storage = MemoryStorage(), server = ProtocolServer(), push = TestPush()
        let client = try client(config, storage, server, push)
        XCTAssertThrowsError(try self.client(config, storage, server, push)) { error in
            XCTAssertEqual(error as? TingFailure, .clientAlreadyExists)
        }
        await client.close()
        try storage.save(Data("invalid-json".utf8), namespace: config.namespace)
        XCTAssertThrowsError(try self.client(config, storage, server, push)) { error in
            XCTAssertEqual(error as? TingFailure, .corruptJournal)
        }
        XCTAssertEqual(try storage.load(namespace: config.namespace), Data("invalid-json".utf8))
        let another = try self.client(configuration(), storage, server, push)
        await another.close()
    }

    func testColdStartOpensAreBufferedAndDeduplicated() async throws {
        let client = try client(configuration(), MemoryStorage(), ProtocolServer(), TestPush())
        let notification = TingNotification(identifier: "push-1", title: "Title", body: "Body", data: ["orderID": "42"])
        await client.notificationOpened(notification)
        await client.notificationOpened(notification)
        var events = await client.events().makeAsyncIterator()
        let initial = await events.next()
        guard case .statusChanged = initial else { return XCTFail("missing initial status") }
        let opened = await events.next()
        XCTAssertEqual(opened, .notificationOpened(notification))
        await client.close()
        let end = await events.next()
        XCTAssertNil(end, "duplicate open was emitted")
    }

    func testConfigurationRequiresTLSAndDoesNotMixNamespaces() throws {
        XCTAssertThrowsError(try TingConfiguration(endpoint: URL(string: "http://ting.example.com")!, appID: "app", apnsEnvironment: .production))
        XCTAssertThrowsError(try TingConfiguration(endpoint: URL(string: "https://user:secret@ting.example.com")!, appID: "app", apnsEnvironment: .production))
        XCTAssertThrowsError(try TingConfiguration(endpoint: URL(string: "https://ting.example.com?token=secret")!, appID: "app", apnsEnvironment: .production))
        let local = try TingConfiguration(endpoint: URL(string: "http://localhost:8080/")!, appID: "app", apnsEnvironment: .production)
        XCTAssertEqual(local.endpoint.absoluteString, "http://localhost:8080")
        XCTAssertNoThrow(try TingConfiguration(endpoint: URL(string: "http://[::1]:8080")!, appID: "a", apnsEnvironment: .sandbox))
        XCTAssertNotEqual(try configuration("a").namespace, try configuration("b").namespace)
        let normalized = try TingConfiguration(endpoint: URL(string: "https://TING.test:443/push/")!, appID: "a", apnsEnvironment: .sandbox)
        XCTAssertEqual(normalized.namespace, try configuration("a").namespace)
        XCTAssertTrue(validInstallationCredential(try installationCredential()))
    }

    func testLostSwitchAndReactivationCanLogoutWithoutIdentityProof() async throws {
        let config = try configuration(), storage = MemoryStorage(), server = ProtocolServer(), push = TestPush()
        var current = try client(config, storage, server, push)
        try await token(current)
        _ = try await current.login(userID: "alice")
        for user in ["bob", "bob"] {
            await current.applicationDidEnterBackground()
            await server.loseNextPUTResponse()
            let lost = try await current.login(userID: user)
            XCTAssertEqual(lost.state, .pendingRegistration)
            var saved = try journal(storage, config)
            XCTAssertNotNil(saved.prepared)
            await current.close()
            saved.retryAt = nil
            try storage.save(JSONEncoder().encode(saved), namespace: config.namespace)
            current = try client(config, storage, server, push) { _ in
                throw TingFailure.identityUnavailable
            }
            // No callback and no APNs token are needed to revoke the adopted candidate.
            let revoked = try await current.logout()
            XCTAssertEqual(revoked.state, .signedOut)
            XCTAssertEqual(revoked.installationID, lost.installationID)
            await current.close()
            current = try client(config, storage, server, push)
            try await token(current)
        }
        await current.close()
    }

    func testLogoutAfterNewLoginSupersedesOlderPendingRevocation() async throws {
        let config = try configuration(), storage = MemoryStorage(), server = ProtocolServer(), push = TestPush(), gate = Gate()
        let client = try client(config, storage, server, push)
        try await token(client)
        _ = try await client.login(userID: "alice")
        // Persist cleanup with an inconclusive HTTP result.
        await client.applicationDidEnterBackground()
        await server.respondNext(TingHTTPResponse(status: 401, body: Data("{\"error\":{\"code\":\"installation_credential_invalid\"}}".utf8)))
        _ = try await client.logout()
        let oldRevocation = try journal(storage, config).revocation!
        await server.pauseNextPUT(gate)
        let login = Task { try await client.login(userID: "bob") }
        await gate.waitUntilEntered()
        let logout = Task { try await client.logout() }
        try await waitForUser(nil, client)
        let next = try journal(storage, config)
        XCTAssertGreaterThan(next.revocation!.generation, next.prepared!.body.generation)
        XCTAssertGreaterThan(next.revocation!.generation, oldRevocation.generation)
        XCTAssertTrue(next.revocation!.credentials.contains(next.prepared!.body.installationToken))
        await gate.release()
        _ = try await login.value
        let result = try await logout.value
        XCTAssertEqual(result.state, .signedOut)
        let record = await server.records.values.first
        XCTAssertEqual(record?.active, false)
        await client.close()
    }

    func testPreparedCredentialWriteMustCommitBeforeHTTP() async throws {
        let config = try configuration(), storage = MemoryStorage(), server = ProtocolServer(), push = TestPush()
        let client = try client(config, storage, server, push)
        try await token(client)
        storage.failSave(number: storage.saves + 2) // Intent saves; prepared credential fails.
        let result = try await client.login(userID: "alice")
        XCTAssertEqual(result.state, .failed(.storageUnavailable))
        let requests = await server.requestCount
        XCTAssertEqual(requests, 0)
        let durable = try journal(storage, config)
        XCTAssertNil(durable.prepared)
        XCTAssertEqual(durable.generation, 0)
        await client.close()
    }

    func testRemoteLogoutAcknowledgmentStorageFailureIsReplayable() async throws {
        let config = try configuration(), storage = MemoryStorage(), server = ProtocolServer(), push = TestPush()
        let client = try client(config, storage, server, push)
        try await token(client)
        _ = try await client.login(userID: "alice")
        storage.failSave(number: storage.saves + 2) // Signed-out intent saves; DELETE acknowledgment fails.
        let failed = try await client.logout()
        XCTAssertNil(failed.userID)
        XCTAssertEqual(failed.state, .failed(.storageUnavailable))
        XCTAssertNotNil(try journal(storage, config).revocation)
        let recovered = await client.reconcile()
        XCTAssertEqual(recovered.state, .signedOut)
        XCTAssertNil(try journal(storage, config).revocation)
        await client.close()
    }

    func testViewTaskCancellationDoesNotDiscardDurableWork() async throws {
        let config = try configuration(), storage = MemoryStorage(), server = ProtocolServer(), push = TestPush(), gate = Gate()
        let client = try client(config, storage, server, push) { user in await gate.pause(); return user }
        try await token(client)
        let task = Task { try await client.login(userID: "alice") }
        await gate.waitUntilEntered()
        task.cancel()
        await gate.release()
        _ = try await task.value
        let result = await client.status()
        XCTAssertEqual(result.state, .registered)
        XCTAssertNotNil(try journal(storage, config).confirmed)
        await client.close()
    }

    func testFreshAPNsRegistrationCanReactivateProviderInvalidation() async throws {
        let config = try configuration(), storage = MemoryStorage(), server = ProtocolServer(), push = TestPush(), clock = TestClock()
        let client = try client(config, storage, server, push, clock: clock)
        try await token(client)
        let first = try await client.login(userID: "alice")
        let old = try journal(storage, config)
        await server.invalidateCurrentRegistration()
        clock.advance(RetryPolicy.registrationRefresh + 1)
        _ = await client.applicationDidBecomeActive()
        try await token(client)
        let status = await client.status()
        XCTAssertEqual(status.state, .registered)
        XCTAssertEqual(status.installationID, first.installationID)
        XCTAssertNotEqual(try journal(storage, config).confirmed?.body.installationToken, old.confirmed?.body.installationToken)
        let record = await server.records.values.first
        XCTAssertEqual(record?.active, true)
        await client.close()
    }

    func testFailedNewIdentityIntentCannotResumeOlderCallback() async throws {
        let config = try configuration(), storage = MemoryStorage(), server = ProtocolServer(), push = TestPush(), gate = Gate()
        let client = try client(config, storage, server, push) { user in
            if user == "alice" { await gate.pause() }
            return user
        }
        try await token(client)
        let old = Task { try await client.login(userID: "alice") }
        await gate.waitUntilEntered()
        storage.failNextSave()
        do { _ = try await client.login(userID: "bob"); XCTFail("expected storage error") }
        catch { XCTAssertEqual(error as? TingFailure, .storageUnavailable) }
        await gate.release()
        _ = try await old.value
        let blocked = await client.reconcile()
        XCTAssertNil(blocked.userID)
        XCTAssertEqual(blocked.state, .failed(.storageUnavailable))
        let count = await server.requestCount
        XCTAssertEqual(count, 0)
        let recovered = try await client.login(userID: "bob")
        XCTAssertEqual(recovered.state, .registered)
        let users = await server.putUsers
        XCTAssertEqual(users, ["bob"])
        await client.close()
    }

    func testForegroundWithUnchangedTokenNeitherRegistersNorFlapsStatus() async throws {
        let config = try configuration(), storage = MemoryStorage(), server = ProtocolServer(), push = TestPush()
        let identityCalls = Counter()
        let client = try client(config, storage, server, push) { user in await identityCalls.increment(); return user }
        try await token(client)
        _ = try await client.login(userID: "alice")
        let before = try journal(storage, config)
        var events = await client.events().makeAsyncIterator()
        _ = await events.next()
        let active = await client.applicationDidBecomeActive()
        XCTAssertEqual(active.state, .registered)
        try await token(client)
        _ = try await client.login(userID: "alice")
        let puts = await server.putCount, calls = await identityCalls.value
        XCTAssertEqual(puts, 1)
        XCTAssertEqual(calls, 1)
        XCTAssertEqual(try journal(storage, config).confirmed?.body.installationToken, before.confirmed?.body.installationToken)
        await client.close()
        let next = await events.next()
        XCTAssertNil(next, "an unchanged registration emitted a status change")
    }

    func testStatusEventsReportUserChangesWithinTheSameState() async throws {
        let config = try configuration(), push = TestPush()
        let client = try client(config, MemoryStorage(), ProtocolServer(), push)
        await push.set(.denied)
        var events = await client.events().makeAsyncIterator()
        _ = await events.next()
        _ = try await client.login(userID: "alice")
        _ = try await client.login(userID: "bob")
        let alice = await events.next(), bob = await events.next()
        guard case let .statusChanged(first) = alice, case let .statusChanged(second) = bob else { return XCTFail("missing status") }
        XCTAssertEqual(first.userID, "alice")
        XCTAssertEqual(second.userID, "bob")
        XCTAssertEqual(second.state, .permissionRequired(.denied))
        await client.close()
    }

    func testServerRetryDeadlineIsCappedAndBackoffStreakPersists() async throws {
        let config = try configuration(), storage = MemoryStorage(), server = ProtocolServer(), push = TestPush(), clock = TestClock()
        let client = try client(config, storage, server, push, clock: clock)
        await client.applicationDidEnterBackground()
        try await token(client)
        await server.respondNext(TingHTTPResponse(status: 503, headers: ["Retry-After": "31536000"]))
        let capped = try await client.login(userID: "alice")
        XCTAssertLessThanOrEqual(capped.retryAt!.timeIntervalSince(clock.now()), RetryPolicy.maximumServerDelay + 1)
        clock.advance(RetryPolicy.maximumServerDelay + 1)
        await server.respondNext(TingHTTPResponse(status: 503))
        _ = await client.reconcile()
        XCTAssertEqual(try journal(storage, config).failures, 2)
        clock.advance(RetryPolicy.maximumDelay * 2)
        let recovered = await client.reconcile()
        XCTAssertEqual(recovered.state, .registered)
        XCTAssertNil(try journal(storage, config).failures)
        await client.close()
    }

    func testOneIdentityProofServesEveryOwnershipCandidate() async throws {
        let config = try configuration(), storage = MemoryStorage(), server = ProtocolServer(), push = TestPush(), clock = TestClock()
        let identityCalls = Counter()
        let client = try client(config, storage, server, push, clock: clock) { user in await identityCalls.increment(); return user }
        try await token(client)
        _ = try await client.login(userID: "alice")
        let first = await server.requests.first
        XCTAssertNil(first?.headers["X-Ting-Installation-Token"], "an unacknowledged installation sent an ownership header")
        // Lose a rotation so that the next registration carries two ownership candidates.
        await client.applicationDidEnterBackground()
        await server.loseNextPUTResponse()
        try await token(client, 0xCD)
        clock.advance(RetryPolicy.maximumDelay * 2)
        // Candidates: the lost rotation's credential (forced stale here), the confirmed one, then the first again.
        await server.respondNext(TingHTTPResponse(status: 401, body: Data("{\"error\":{\"code\":\"installation_credential_invalid\"}}".utf8)))
        try await token(client, 0xEF)
        let status = await client.status()
        XCTAssertEqual(status.state, .registered)
        let calls = await identityCalls.value, puts = await server.putCount
        XCTAssertEqual(puts, 5)
        XCTAssertEqual(calls, 3, "each registration should fetch one identity proof")
        await client.close()
    }

    func testRequestPermissionForwardsOptions() async throws {
        let push = TestPush()
        let client = try client(configuration(), MemoryStorage(), ProtocolServer(), push)
        _ = try await client.requestPermission()
        _ = try await client.requestPermission(options: [.alert, .provisional])
        let options = await push.promptOptions
        XCTAssertEqual(options, [.standard, [.alert, .provisional]])
        await client.close()
    }

    func testNotificationPayloadKeepsNonStringValues() {
        let payload = TingNotification.payload([
            "aps": ["alert": "hidden"], "orderID": "42", "count": 3, "urgent": true,
            "items": [["id": 1]], AnyHashable(7): "ignored",
        ])
        XCTAssertEqual(payload, ["orderID": "42", "count": "3", "urgent": "true", "items": "[{\"id\":1}]"])
    }
}

actor Counter {
    private(set) var value = 0
    func increment() { value += 1 }
}
