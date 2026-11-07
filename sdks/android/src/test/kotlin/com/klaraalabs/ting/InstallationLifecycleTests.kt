@file:OptIn(kotlinx.coroutines.ExperimentalCoroutinesApi::class)
package com.klaraalabs.ting

import com.klaraalabs.ting.http.*
import com.klaraalabs.ting.installations.*
import com.klaraalabs.ting.notifications.*
import kotlinx.coroutines.*
import kotlinx.coroutines.flow.*
import kotlinx.coroutines.test.*
import org.junit.Assert.*
import org.junit.Test
import java.util.UUID

class InstallationLifecycleTests {
    private class Rig(val scheduler: TestCoroutineScheduler) {
        val config = TingConfiguration("https://ting.test/", UUID.randomUUID().toString(), "messages")
        val storage = MemoryStorage()
        val server = ProtocolServer()
        val push = TestPush()
        val clock = { 1_000_000L + scheduler.currentTime }
        var proofCalls = 0
        fun client(identity: IdentityTokenProvider = IdentityTokenProvider { proofCalls++; it }, transport: TingHttpTransport = server) =
            TingClient(config, identity, storage, push, transport, clock, StandardTestDispatcher(scheduler)).also { it.applicationDidEnterBackground() }
        fun journal() = storage.journal(config)
    }

    private suspend fun expectFailure(kind: FailureKind, block: suspend () -> Unit) {
        try { block(); fail("expected $kind") } catch (error: TingException) { assertEquals(kind, error.failure.kind) }
    }

    @Test fun permissionTokenSwitchAndLogout() = runTest {
        val rig = Rig(testScheduler); val client = rig.client()
        rig.push.authorization = PushPermission.RUNTIME_PERMISSION_REQUIRED
        assertEquals(RegistrationState.PERMISSION_REQUIRED, client.login("alice").state)
        assertTrue(rig.server.store.requests.isEmpty())
        rig.push.authorization = PushPermission.ALLOWED
        assertEquals(RegistrationState.REGISTERED, client.reconcile().state)
        val first = rig.journal()
        client.onNewToken("fcm-token-b")
        assertTrue(rig.journal().generation > first.generation)
        assertNotEquals(first.credential, rig.journal().credential)
        assertEquals(RegistrationState.REGISTERED, client.login("bob").state)
        assertEquals("bob", rig.server.store.records.values.single().user)
        assertEquals(RegistrationState.SIGNED_OUT, client.logout().state)
        assertFalse(rig.server.store.records.values.single().active)
        assertEquals(first.installationId, rig.journal().installationId)
        client.close()
    }

    @Test fun lostCreationReplaysSameRequestAfterRestart() = runTest {
        val rig = Rig(testScheduler); var client = rig.client()
        rig.server.dropNextPut = true
        assertEquals(RegistrationState.PENDING_REGISTRATION, client.login("alice").state)
        val prepared = rig.journal().prepared!!
        val id = client.status.value.installationId
        client.close(); advanceTimeBy(400_000)
        client = rig.client()
        assertEquals(RegistrationState.SIGNED_OUT, client.reconcile().state)
        assertEquals(1, rig.server.store.requests.size)
        assertEquals(RegistrationState.REGISTERED, client.login("alice").state)
        assertEquals(id, client.status.value.installationId)
        assertEquals(prepared.body, rig.journal().confirmed!!.body)
        client.close()
    }

    @Test fun delayedIdentityCannotRestoreEarlierUser() = runTest {
        val rig = Rig(testScheduler); val gate = Gate()
        val client = rig.client(IdentityTokenProvider { if (it == "alice") gate.pause(); it })
        val alice = async { client.login("alice") }
        gate.entered.await()
        val bob = async { client.login("bob") }
        runCurrent()
        assertEquals("bob", rig.journal().desiredUserId)
        assertEquals("bob", client.status.value.userId)
        gate.release.complete(Unit)
        alice.await(); bob.await()
        assertEquals(listOf("Bearer bob"), rig.server.store.requests.map { it.headers["Authorization"] })
        assertEquals("bob", client.status.value.userId)
        client.close()
    }

    @Test fun logoutDuringInFlightCreationUsesProposedCredential() = runTest {
        val rig = Rig(testScheduler); val gate = Gate(); val client = rig.client()
        rig.server.putGate = gate
        val login = async { client.login("alice") }; gate.entered.await()
        val logout = async { client.logout() }; runCurrent()
        assertNull(client.status.value.userId)
        assertTrue(rig.journal().revocation!!.generation > rig.journal().prepared!!.body.generation)
        gate.release.complete(Unit); login.await(); logout.await()
        assertEquals(RegistrationState.SIGNED_OUT, client.status.value.state)
        assertFalse(rig.server.store.records.values.single().active)
        client.close()
    }

    @Test fun failedNewIntentBlocksEarlierProofUntilExplicitRetry() = runTest {
        val rig = Rig(testScheduler); val gate = Gate()
        val client = rig.client(IdentityTokenProvider { if (it == "alice") gate.pause(); it })
        val old = async { client.login("alice") }; gate.entered.await()
        rig.storage.failNext()
        expectFailure(FailureKind.STORAGE_UNAVAILABLE) { client.login("bob") }
        gate.release.complete(Unit); old.await()
        assertNull(client.status.value.userId)
        assertEquals(RegistrationState.FAILED, client.reconcile().state)
        assertTrue(rig.server.store.requests.isEmpty())
        assertEquals(RegistrationState.REGISTERED, client.login("bob").state)
        client.close()
    }

    @Test fun preparedWriteFailureSendsNoHttp() = runTest {
        val rig = Rig(testScheduler); val client = rig.client()
        rig.storage.failWrite = rig.storage.writes + 2
        val result = client.login("alice")
        assertEquals(FailureKind.STORAGE_UNAVAILABLE, result.failure?.kind)
        assertTrue(rig.server.store.requests.isEmpty())
        assertNull(rig.journal().prepared)
        assertEquals(0L, rig.journal().generation)
        client.close()
    }

    @Test fun failedRegistrationAcknowledgmentReplaysDurablePreparedBody() = runTest {
        val rig = Rig(testScheduler); val client = rig.client()
        rig.server.afterPut = { rig.storage.failNext() }
        assertEquals(RegistrationState.FAILED, client.login("alice").state)
        val prepared = rig.journal().prepared!!
        assertEquals(RegistrationState.REGISTERED, client.reconcile().state)
        assertEquals(prepared.body, rig.journal().confirmed!!.body)
        client.close()
    }

    @Test fun failedLogoutIntentClearsLiveUserAndRetainsCredentials() = runTest {
        val rig = Rig(testScheduler); val client = rig.client()
        client.login("alice"); val credential = rig.journal().credential
        rig.storage.failNext()
        expectFailure(FailureKind.STORAGE_UNAVAILABLE) { client.logout() }
        assertNull(client.status.value.userId)
        assertEquals(credential, rig.journal().credential)
        assertEquals(RegistrationState.FAILED, client.reconcile().state)
        assertEquals(RegistrationState.SIGNED_OUT, client.logout().state)
        client.close()
    }

    @Test fun failedDeleteAcknowledgmentSurvivesRestart() = runTest {
        val rig = Rig(testScheduler); var client = rig.client()
        client.login("alice")
        rig.storage.failWrite = rig.storage.writes + 2
        assertEquals(RegistrationState.FAILED, client.logout().state)
        assertNotNull(rig.journal().revocation)
        client.close(); client = rig.client(IdentityTokenProvider { error("logout must not obtain identity") })
        assertEquals(RegistrationState.SIGNED_OUT, client.reconcile().state)
        assertNull(rig.journal().revocation)
        client.close()
    }

    @Test fun lostRotationRevokesWithoutHostIdentityAfterRestart() = runTest {
        val rig = Rig(testScheduler); var client = rig.client()
        client.login("alice"); rig.server.dropNextPut = true
        client.login("bob"); val id = rig.journal().installationId
        client.close(); advanceTimeBy(400_000)
        client = rig.client(IdentityTokenProvider { error("no session") })
        assertEquals(RegistrationState.SIGNED_OUT, client.logout().state)
        assertEquals(id, rig.journal().installationId)
        assertFalse(rig.server.store.records.values.single().active)
        client.close()
    }

    @Test fun unknownCreationRetainsLogoutThenNewLoginSupersedesIt() = runTest {
        val rig = Rig(testScheduler); val gate = Gate()
        var client = rig.client(IdentityTokenProvider { gate.pause(); it })
        val old = async { client.login("alice") }; gate.entered.await()
        val logout = async { client.logout() }; runCurrent()
        gate.release.complete(Unit); old.await()
        assertEquals(RegistrationState.PENDING_REVOCATION, logout.await().state)
        val id = rig.journal().installationId
        client.close(); client = rig.client()
        assertEquals(RegistrationState.PENDING_REVOCATION, client.reconcile().state)
        assertEquals(RegistrationState.REGISTERED, client.login("bob").state)
        assertEquals(id, rig.journal().installationId)
        assertNull(rig.journal().revocation)
        assertEquals("bob", rig.server.store.records.values.single().user)
        client.close()
    }

    @Test fun tokenCallbackWinsOverOlderTokenFetch() = runTest {
        val rig = Rig(testScheduler); val client = rig.client(); val gate = Gate()
        rig.push.tokenGate = gate
        val login = async { client.login("alice") }; gate.entered.await()
        val callback = async { client.onNewToken("fcm-newer-token") }; runCurrent()
        gate.release.complete(Unit); login.await(); callback.await()
        assertEquals("fcm-newer-token", rig.journal().confirmed!!.body.pushToken)
        assertEquals(1, rig.server.store.requests.size)
        client.close()
    }

    @Test fun rateLimitAndFailureStreakPersistAcrossRestartAndLogout() = runTest {
        val rig = Rig(testScheduler); var client = rig.client()
        rig.server.nextResponse = TingHttpResponse(429, mapOf("Retry-After" to "31536000"))
        assertEquals(RegistrationState.PENDING_REGISTRATION, client.login("alice").state)
        assertEquals(rig.clock() + 3_600_000, rig.journal().retryAt)
        assertEquals(RegistrationState.PENDING_REVOCATION, client.logout().state)
        assertEquals(1, rig.server.store.requests.size)
        client.close(); client = rig.client()
        assertEquals(RegistrationState.PENDING_REVOCATION, client.reconcile().state)
        assertEquals(1, rig.journal().failures)
        advanceTimeBy(3_600_001)
        rig.server.nextResponse = TingHttpResponse(503)
        client.reconcile()
        assertEquals(2, rig.journal().failures)
        client.close()
    }

    @Test fun unauthorizedRefreshesProofOnlyOnce() = runTest {
        val rig = Rig(testScheduler)
        var sends = 0
        val client = rig.client(transport = TingHttpTransport { request ->
            sends++
            if (sends <= 2) rig.server.error(401, "unauthorized") else rig.server.send(request)
        })
        val result = client.login("alice")
        assertEquals(RegistrationState.FAILED, result.state)
        assertEquals("unauthorized", result.failure?.serverCode)
        assertEquals(2, sends); assertEquals(2, rig.proofCalls)
        client.close()
    }

    @Test fun supersededClientKeepsUuidAndJournalAcrossRestart() = runTest {
        val rig = Rig(testScheduler); var client = rig.client()
        client.login("alice"); val old = rig.journal()
        val api = InstallationApi(rig.config); val replacementId = UUID.randomUUID().toString()
        rig.server.send(api.register(replacementId, RegistrationBody(pushToken = rig.push.currentToken,
            installationToken = newCredential(), generation = 1), "bob", null))
        advanceTimeBy(86_400_001)
        val conflict = client.reconcile()
        assertEquals("installation_superseded", conflict.failure?.serverCode)
        assertEquals(old.installationId, client.status.value.installationId)
        assertNotNull(rig.journal().prepared)
        client.close(); client = rig.client()
        assertEquals("installation_superseded", client.login("alice").failure?.serverCode)
        assertEquals(old.installationId, rig.journal().installationId)
        client.logout()
        assertTrue(rig.server.store.records.getValue(replacementId).active)
        assertEquals("bob", rig.server.store.records.getValue(replacementId).user)
        client.close()
    }

    @Test fun namespaceOwnershipCorruptionAndCloseAreExplicit() = runTest {
        val rig = Rig(testScheduler); val client = rig.client()
        expectFailure(FailureKind.CLIENT_ALREADY_EXISTS) { rig.client() }
        client.close()
        expectFailure(FailureKind.CLIENT_CLOSED) { client.login("alice") }
        rig.storage.save(rig.config.namespace, "broken".toByteArray())
        expectFailure(FailureKind.CORRUPT_JOURNAL) { rig.client() }
        assertEquals("broken", rig.storage.load(rig.config.namespace)!!.decodeToString())
    }

    @Test fun cancelledCallerDoesNotCancelDurableRegistration() = runTest {
        val rig = Rig(testScheduler); val gate = Gate()
        val client = rig.client(IdentityTokenProvider { gate.pause(); it })
        val task = launch { client.login("alice") }; gate.entered.await()
        task.cancelAndJoin(); gate.release.complete(Unit)
        assertEquals(RegistrationState.REGISTERED, client.reconcile().state)
        client.close()
    }

    @Test fun initialOpensAreBufferedDeduplicatedAndSubscribersDispose() = runTest {
        val rig = Rig(testScheduler); val client = rig.client()
        val notification = TingNotification("push-1", "Title", "Body", mapOf("orderId" to "42"))
        client.notificationOpened(notification); client.notificationOpened(notification)
        val first = async { client.events.take(2).toList() }; runCurrent()
        val values = first.await()
        assertTrue(values.first() is TingEvent.StatusChanged)
        assertSame(notification, (values[1] as TingEvent.NotificationOpened).notification)
        client.notificationOpened(TingNotification("push-2", "", "", emptyMap()))
        val second = client.events.take(2).toList()
        assertEquals("push-2", (second[1] as TingEvent.NotificationOpened).notification.identifier)
        client.close()
    }

    @Test fun foregroundSuppressesUnchangedRegistrationButRefreshesOldAssociation() = runTest {
        val rig = Rig(testScheduler); val client = rig.client()
        client.login("alice"); val before = rig.journal()
        client.applicationDidBecomeActive()
        assertEquals(1, rig.server.store.requests.size)
        assertEquals(before.credential, rig.journal().credential)
        advanceTimeBy(86_400_001)
        client.applicationDidBecomeActive()
        assertEquals(2, rig.server.store.requests.size)
        assertNotEquals(before.credential, rig.journal().credential)
        client.close()
    }

    @Test fun configurationCredentialsAndGenerationLimits() = runTest {
        val canonical = TingConfiguration("https://TING.test:443/path/", "app", "messages")
        assertEquals("https://ting.test/path", canonical.endpoint)
        assertEquals(canonical.namespace, TingConfiguration("https://ting.test/path", "app", "messages").namespace)
        expectFailure(FailureKind.INVALID_CONFIGURATION) { TingConfiguration("http://ting.test", "app", "messages") }
        expectFailure(FailureKind.INVALID_CONFIGURATION) { TingConfiguration("https://user:pass@ting.test", "app", "messages") }
        expectFailure(FailureKind.INVALID_CONFIGURATION) { TingConfiguration("https://ting.test?token=x", "app", "messages") }
        expectFailure(FailureKind.INVALID_CONFIGURATION) { TingConfiguration("https://ting.test:99999", "app", "messages") }
        expectFailure(FailureKind.INVALID_CONFIGURATION) { TingConfiguration("https://ting.test", "app\r\n", "messages") }
        assertEquals("http://localhost:8080", TingConfiguration("http://localhost:8080/", "app", "messages").endpoint)
        repeat(100) { assertTrue(validCredential(newCredential())) }
        val journal = InstallationJournal(namespace = canonical.namespace, generation = InstallationJournal.MAX_GENERATION)
        expectFailure(FailureKind.GENERATION_EXHAUSTED) { journal.nextGeneration() }
    }

    @Test fun automaticRetriesAreBoundedAndBackgroundStopsTimers() = runTest {
        val rig = Rig(testScheduler)
        var sends = 0
        val client = rig.client(transport = TingHttpTransport { sends++; TingHttpResponse(503) })
        client.applicationDidBecomeActive()
        client.login("alice")
        advanceTimeBy(60_000); runCurrent()
        assertEquals("one initial request and at most two automatic retries", 3, sends)
        client.applicationDidBecomeActive()
        client.applicationDidEnterBackground()
        val before = sends
        advanceTimeBy(60_000); runCurrent()
        assertEquals(before, sends)
        client.close()
    }

    @Test fun lostDeleteResponseRetriesSameGenerationAcrossRestart() = runTest {
        val rig = Rig(testScheduler); var client = rig.client()
        client.login("alice"); rig.server.dropNextDelete = true
        assertEquals(RegistrationState.PENDING_REVOCATION, client.logout().state)
        val pending = rig.journal().revocation!!
        client.close(); advanceTimeBy(400_000); client = rig.client()
        assertEquals(RegistrationState.SIGNED_OUT, client.reconcile().state)
        val deletes = rig.server.store.requests.filter { it.method == "DELETE" }
        assertEquals(2, deletes.size)
        assertTrue(deletes.all { it.headers["X-Ting-Installation-Generation"] == pending.generation.toString() })
        client.close()
    }
}
