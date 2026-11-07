package com.klaraalabs.ting.installations

import com.klaraalabs.ting.*
import com.klaraalabs.ting.http.*
import com.klaraalabs.ting.notifications.*
import kotlinx.coroutines.*
import kotlinx.coroutines.channels.BufferOverflow
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.flow.*
import java.io.IOException
import java.util.UUID
import kotlin.math.pow
import kotlin.random.Random

/** One application-scoped client per canonical endpoint/App. Call close before deliberately replacing it. */
class TingClient(
    val configuration: TingConfiguration,
    private val identityTokenProvider: IdentityTokenProvider,
    private val storage: InstallationStorage,
    private val pushRegistration: PushRegistrationProvider,
    private val transport: TingHttpTransport = OkHttpTingTransport(),
    private val clock: () -> Long = System::currentTimeMillis,
    private val dispatcher: CoroutineDispatcher = Dispatchers.IO,
) {
    private val lock = Any()
    private val scope = CoroutineScope(SupervisorJob() + dispatcher)
    private val api = InstallationApi(configuration)
    private var journal: InstallationJournal
    private var currentUser: String? = null
    private var nativeToken: String? = null
    private var revision = 0L
    private var worker: Deferred<Unit>? = null
    private var retry: Job? = null
    private var automaticAttempts = 0
    private var active = true
    private var closed = false
    private var storageBlocked = false
    private var ownsNamespace = true
    private val listeners = mutableSetOf<Channel<TingEvent>>()
    private val initialOpens = ArrayDeque<TingNotification>()
    private val seenOpens = LinkedHashSet<String>()
    private val mutableStatus: MutableStateFlow<InstallationStatus>
    val status: StateFlow<InstallationStatus> get() = mutableStatus.asStateFlow()

    init {
        synchronized(ownership) {
            if (!ownership.add(configuration.namespace)) fail(FailureKind.CLIENT_ALREADY_EXISTS)
        }
        try {
            journal = storage.load(configuration.namespace)?.let { JournalCodec.decode(it, configuration.namespace) }
                ?: InstallationJournal(namespace = configuration.namespace).also {
                    storage.save(configuration.namespace, JournalCodec.encode(it))
                }
            mutableStatus = MutableStateFlow(InstallationStatus(journal.installationId, null,
                if (journal.revocation == null) RegistrationState.SIGNED_OUT else RegistrationState.PENDING_REVOCATION,
                journal.retryAt))
        } catch (error: Exception) {
            synchronized(ownership) { ownership.remove(configuration.namespace) }
            scope.cancel()
            throw if (error is TingException) error else TingException(TingFailure(FailureKind.STORAGE_UNAVAILABLE))
        }
    }

    /** A cancelled observer never cancels journal work. Initial opens go to the first subscriber only. */
    val events: Flow<TingEvent> = flow {
        val channel = Channel<TingEvent>(64, BufferOverflow.DROP_OLDEST)
        synchronized(lock) {
            if (closed) channel.close() else {
                listeners.add(channel)
                channel.trySend(TingEvent.StatusChanged(mutableStatus.value))
                initialOpens.forEach { channel.trySend(TingEvent.NotificationOpened(it)) }
                initialOpens.clear()
            }
        }
        try { emitAll(channel.receiveAsFlow()) }
        finally { synchronized(lock) { listeners.remove(channel); channel.close() } }
    }

    suspend fun login(userId: String): InstallationStatus = withContext(dispatcher) {
        synchronized(lock) {
            requireOpen()
            if (!validUserId(userId)) fail(FailureKind.INVALID_USER_ID)
            try { persist(journal.copy(intent = UUID.randomUUID().toString(), desiredUserId = userId)) }
            catch (error: TingException) {
                currentUser = null; revision++; storageBlocked = true
                publish(RegistrationState.FAILED, failure = error.failure)
                throw error
            }
            currentUser = userId; revision++; storageBlocked = false; automaticAttempts = 0
            if (mutableStatus.value.userId != userId || mutableStatus.value.state == RegistrationState.FAILED)
                publish(RegistrationState.PENDING_REGISTRATION)
        }
        reconcile()
    }

    suspend fun logout(): InstallationStatus = withContext(dispatcher) {
        synchronized(lock) {
            requireOpen()
            currentUser = null; revision++; retry?.cancel()
            try {
                var next = journal.copy(intent = UUID.randomUUID().toString(), desiredUserId = null)
                if (next.credentials.isNotEmpty() && (next.revocation == null || next.revocation.generation < next.generation)) {
                    val generation = next.nextGeneration()
                    next = next.copy(generation = generation, revocation = PendingRevocation(generation, next.credentials))
                }
                persist(next)
            } catch (error: TingException) {
                storageBlocked = true
                publish(RegistrationState.FAILED, failure = error.failure)
                throw error
            }
            storageBlocked = false; automaticAttempts = 0
            publish(if (journal.revocation == null) RegistrationState.SIGNED_OUT else RegistrationState.PENDING_REVOCATION)
        }
        reconcile()
    }

    /** Call from onNewToken. FCM service ownership stays with the host app. */
    suspend fun onNewToken(token: String): InstallationStatus {
        synchronized(lock) {
            requireOpen()
            if (!validPushToken(token)) fail(FailureKind.INVALID_PUSH_TOKEN)
            nativeToken = token; revision++
        }
        return reconcile()
    }

    suspend fun applicationDidBecomeActive(): InstallationStatus {
        synchronized(lock) {
            if (closed) return status.value
            active = true; automaticAttempts = 0; nativeToken = null; revision++
        }
        return reconcile()
    }

    fun applicationDidEnterBackground() = synchronized(lock) { active = false; retry?.cancel(); Unit }

    /** Explicit connectivity recovery, permission-result reconciliation, or retry. Does not prompt. */
    suspend fun reconcile(): InstallationStatus {
        val task = synchronized(lock) {
            if (closed || storageBlocked) return status.value
            worker ?: scope.async(start = CoroutineStart.LAZY) { drain() }.also { worker = it }
        }
        task.start()
        task.await()
        return status.value
    }

    fun notificationReceived(notification: TingNotification) = synchronized(lock) {
        if (!closed) listeners.forEach { it.trySend(TingEvent.NotificationReceived(notification)) }
    }

    fun notificationOpened(notification: TingNotification) = synchronized(lock) {
        if (closed || !seenOpens.add(notification.identifier)) return@synchronized
        if (seenOpens.size > 256) seenOpens.remove(seenOpens.first())
        if (listeners.isEmpty()) {
            initialOpens.addLast(notification)
            if (initialOpens.size > 32) initialOpens.removeFirst()
        } else listeners.forEach { it.trySend(TingEvent.NotificationOpened(notification)) }
    }

    suspend fun close() = withContext(NonCancellable) {
        val task = synchronized(lock) { closed = true; retry?.cancel(); worker }
        task?.await()
        synchronized(lock) {
            if (ownsNamespace) {
                synchronized(ownership) { ownership.remove(configuration.namespace) }
                ownsNamespace = false
            }
            listeners.forEach { it.close() }; listeners.clear()
            scope.cancel()
        }
    }

    private suspend fun drain() {
        while (true) {
            val startedRevision = synchronized(lock) {
                if (closed || storageBlocked) { worker = null; return }
                val deadline = journal.retryAt
                if (deadline != null && deadline > clock()) {
                    publish(pendingState()); scheduleRetry(deadline); worker = null; return
                }
                revision
            }
            try {
                val more = reconcileOnce()
                synchronized(lock) {
                    if (!more && startedRevision == revision) { worker = null; return }
                }
            } catch (error: RetryLater) {
                synchronized(lock) {
                    try {
                        val failures = (journal.failures + 1).coerceAtMost(1000)
                        val delay = (1000.0 * 2.0.pow((failures - 1).coerceAtMost(16))).coerceAtMost(300_000.0) * Random.nextDouble(0.8, 1.2)
                        val deadline = maxOf(clock() + delay.toLong(), (error.deadline ?: 0).coerceAtMost(clock() + 3_600_000))
                        persist(journal.copy(failures = failures, retryAt = deadline))
                        publish(pendingState()); scheduleRetry(deadline)
                    } catch (storageError: TingException) { publish(RegistrationState.FAILED, failure = storageError.failure) }
                    worker = null
                }
                return
            } catch (error: Exception) {
                synchronized(lock) {
                    val failure = (error as? TingException)?.failure ?: TingFailure(FailureKind.PUSH_UNAVAILABLE)
                    publish(RegistrationState.FAILED, failure = failure)
                    if (startedRevision == revision) { worker = null; return }
                }
            }
        }
    }

    private suspend fun reconcileOnce(): Boolean {
        val snapshot = synchronized(lock) { Triple(currentUser, journal.intent, journal.revocation) }
        val user = snapshot.first
        if (user == null) {
            val revocation = snapshot.third
            if (revocation != null) return revoke(revocation)
            synchronized(lock) { publish(RegistrationState.SIGNED_OUT) }
            return false
        }
        val intent = snapshot.second
        val permission = try { pushRegistration.permission() } catch (_: Exception) { fail(FailureKind.PUSH_UNAVAILABLE) }
        synchronized(lock) {
            if (!isCurrent(user, intent)) return true
            if (permission != PushPermission.ALLOWED) {
                publish(RegistrationState.PERMISSION_REQUIRED, permission = permission); return false
            }
        }
        // Epoch-check every asynchronous result. A newer FCM callback wins over a token fetch in flight.
        val tokenRevision = synchronized(lock) { revision }
        val fetchedToken = synchronized(lock) { nativeToken } ?: try {
            pushRegistration.token()
        } catch (_: Exception) {
            synchronized(lock) { if (!isCurrent(user, intent)) return true }
            fail(FailureKind.PUSH_UNAVAILABLE)
        }
        val prepared = synchronized(lock) {
            if (!isCurrent(user, intent)) return true
            if (revision != tokenRevision) return true
            if (!validPushToken(fetchedToken)) fail(FailureKind.INVALID_PUSH_TOKEN)
            nativeToken = fetchedToken
            val existing = journal.prepared
            val confirmed = journal.confirmed
            if (existing != null && existing.userId == user && existing.body.pushToken == fetchedToken && journal.revocation == null) existing
            else if (existing == null && hasActiveRegistration(user) && confirmed != null && confirmed.body.pushToken == fetchedToken &&
                clock() - confirmed.confirmedAt in 0 until 86_400_000) {
                publish(RegistrationState.REGISTERED); return false
            } else {
                val generation = journal.nextGeneration()
                val value = PreparedRegistration(user, RegistrationBody(pushToken = fetchedToken,
                    installationToken = newCredential(), generation = generation), journal.credentials)
                persist(journal.copy(generation = generation, prepared = value))
                value
            }.also { publish(RegistrationState.REGISTERING) }
        }

        val candidates: List<String?> = prepared.previousCredentials.ifEmpty { listOf(null) }.let {
            if (it.size > 1) it + it.first() else it
        }
        var proof: String? = null
        var refreshed = false
        var candidateIndex = 0
        while (candidateIndex < candidates.size) {
            if (proof == null) proof = try { identityTokenProvider.token(user) } catch (_: Exception) {
                synchronized(lock) { if (!isCurrent(user, intent)) return true }
                fail(FailureKind.IDENTITY_UNAVAILABLE)
            }
            synchronized(lock) { if (!isCurrent(user, intent) || closed) return true }
            if (proof.isBlank() || proof.toByteArray().size > 8192 || proof.any { it.isWhitespace() || it.isISOControl() }) fail(FailureKind.IDENTITY_UNAVAILABLE)
            val id = synchronized(lock) { journal.installationId }
            val response = send(api.register(id, prepared.body, proof, candidates[candidateIndex]))
            if (response.status == 401 && response.errorCode == "unauthorized" && !refreshed) {
                refreshed = true; proof = null; continue
            }
            if (response.status == 401 && response.errorCode == "installation_credential_invalid") { candidateIndex++; continue }
            if (response.status != 200 && response.status != 201) serverFailure(response)
            if (!api.acknowledges(response, id, prepared.body)) fail(FailureKind.INVALID_SERVER_RESPONSE)
            return synchronized(lock) {
                persist(journal.copy(credential = prepared.body.installationToken,
                    confirmed = ConfirmedRegistration(user, prepared.body, true, clock()), prepared = null,
                    retryAt = null, failures = 0,
                    revocation = journal.revocation?.takeUnless { prepared.body.generation > it.generation }))
                if (isCurrent(user, intent)) publish(RegistrationState.REGISTERED)
                !isCurrent(user, intent) || nativeToken != prepared.body.pushToken || journal.revocation != null
            }
        }
        throw TingException(TingFailure(FailureKind.SERVER, 401, "installation_credential_invalid"))
    }

    private suspend fun revoke(revocation: PendingRevocation): Boolean {
        val candidates = revocation.credentials.let { if (it.size > 1) it + it.first() else it }
        for (credential in candidates) {
            val id = synchronized(lock) { journal.installationId }
            val response = send(api.revoke(id, revocation.generation, credential))
            if (response.status == 401 && response.errorCode == "installation_credential_invalid") continue
            if (response.status != 204) serverFailure(response)
            return synchronized(lock) {
                persist(journal.copy(credential = credential, confirmed = journal.confirmed?.copy(active = false),
                    prepared = null, revocation = null, retryAt = null, failures = 0))
                if (currentUser == null) publish(RegistrationState.SIGNED_OUT)
                currentUser != null
            }
        }
        return synchronized(lock) { publish(RegistrationState.PENDING_REVOCATION); currentUser != null }
    }

    private class RetryLater(val deadline: Long?) : Exception()
    private suspend fun send(request: TingHttpRequest): TingHttpResponse {
        val response = try { transport.send(request) } catch (_: IOException) { throw RetryLater(null) }
        if (response.status == 429 || response.status in 500..599) throw RetryLater(response.retryAfter(clock()))
        return response
    }
    private fun serverFailure(response: TingHttpResponse): Nothing =
        throw TingException(TingFailure(FailureKind.SERVER, response.status, response.errorCode))

    // All helpers below run under lock. No suspending host or network callback runs while holding it.
    private fun scheduleRetry(deadline: Long) {
        retry?.cancel()
        if (!active || closed || automaticAttempts >= 2) return
        automaticAttempts++
        retry = scope.launch { delay((deadline - clock()).coerceAtLeast(1)); reconcile() }
    }
    private fun persist(next: InstallationJournal) {
        try { storage.save(configuration.namespace, JournalCodec.encode(next)) }
        catch (_: Exception) { fail(FailureKind.STORAGE_UNAVAILABLE) }
        journal = next
    }
    private fun pendingState() = if (currentUser == null) {
        if (journal.revocation == null) RegistrationState.SIGNED_OUT else RegistrationState.PENDING_REVOCATION
    } else RegistrationState.PENDING_REGISTRATION
    private fun hasActiveRegistration(user: String): Boolean = journal.confirmed?.let {
        it.active && it.userId == user && journal.revocation == null && journal.credential == it.body.installationToken
    } == true
    private fun isCurrent(user: String, intent: String) = !storageBlocked && currentUser == user && journal.intent == intent
    private fun requireOpen() { if (closed) fail(FailureKind.CLIENT_CLOSED) }
    private fun publish(state: RegistrationState, permission: PushPermission? = null, failure: TingFailure? = null) {
        val next = InstallationStatus(journal.installationId, currentUser, state, journal.retryAt, permission, failure)
        if (mutableStatus.value == next) return
        mutableStatus.value = next
        listeners.forEach { it.trySend(TingEvent.StatusChanged(next)) }
    }

    companion object { private val ownership = mutableSetOf<String>() }
}
