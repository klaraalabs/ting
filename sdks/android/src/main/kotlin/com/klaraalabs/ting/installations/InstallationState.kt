package com.klaraalabs.ting.installations

import com.klaraalabs.ting.*
import kotlinx.serialization.Serializable
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json
import java.security.SecureRandom
import java.util.UUID

enum class PushPermission { ALLOWED, RUNTIME_PERMISSION_REQUIRED, NOTIFICATIONS_DISABLED, CHANNEL_MISSING, CHANNEL_DISABLED }
enum class RegistrationState {
    SIGNED_OUT, PERMISSION_REQUIRED, REGISTERING, REGISTERED, PENDING_REGISTRATION, PENDING_REVOCATION, FAILED,
}

data class InstallationStatus(
    val installationId: String,
    val userId: String?,
    val state: RegistrationState,
    val retryAtMillis: Long? = null,
    val permission: PushPermission? = null,
    val failure: TingFailure? = null,
)

fun interface IdentityTokenProvider { suspend fun token(userId: String): String }

interface PushRegistrationProvider {
    suspend fun permission(): PushPermission
    /** Fetch a current Firebase token. A journal token is never authentication or permanent token authority. */
    suspend fun token(): String
}

/** Atomic replacement is required. Failed writes must leave the previous snapshot readable. */
interface InstallationStorage {
    /** Only genuinely missing state returns null. Locked, unreadable, or lost-key state must throw. */
    fun load(namespace: String): ByteArray?
    fun save(namespace: String, value: ByteArray)
}

@Serializable
internal data class RegistrationBody(
    val platform: String = "android",
    val pushToken: String,
    val installationToken: String,
    val generation: Long,
)
@Serializable
internal data class PreparedRegistration(val userId: String, val body: RegistrationBody, val previousCredentials: List<String>)
@Serializable
internal data class ConfirmedRegistration(val userId: String, val body: RegistrationBody, val active: Boolean, val confirmedAt: Long)
@Serializable
internal data class PendingRevocation(val generation: Long, val credentials: List<String>)
@Serializable
internal data class InstallationJournal(
    val version: Int = 1,
    val namespace: String,
    val installationId: String = UUID.randomUUID().toString(),
    val generation: Long = 0,
    val intent: String = UUID.randomUUID().toString(),
    val desiredUserId: String? = null,
    val credential: String? = null,
    val confirmed: ConfirmedRegistration? = null,
    val prepared: PreparedRegistration? = null,
    val revocation: PendingRevocation? = null,
    val retryAt: Long? = null,
    val failures: Int = 0,
) {
    fun nextGeneration(): Long {
        if (generation >= MAX_GENERATION) fail(FailureKind.GENERATION_EXHAUSTED)
        return generation + 1
    }
    val credentials: List<String> get() = (listOfNotNull(prepared?.body?.installationToken, credential,
        confirmed?.body?.installationToken) + prepared?.previousCredentials.orEmpty() + revocation?.credentials.orEmpty()).distinct()

    fun validate(expectedNamespace: String) {
        val uuid = runCatching { UUID.fromString(installationId).toString() }.getOrNull()
        val bodies = listOfNotNull(prepared?.body, confirmed?.body)
        if (version != 1 || namespace != expectedNamespace || uuid != installationId ||
            generation !in 0..MAX_GENERATION || failures !in 0..1000 || retryAt?.let { it < 0 } == true ||
            credentials.any { !validCredential(it) } || desiredUserId?.let { !validUserId(it) } == true ||
            prepared?.let { !validUserId(it.userId) } == true || confirmed?.let { !validUserId(it.userId) } == true ||
            bodies.any { it.platform != "android" || it.generation !in 1..generation || !validPushToken(it.pushToken) } ||
            revocation?.let { it.generation !in 1..generation || it.credentials.isEmpty() } == true) fail(FailureKind.CORRUPT_JOURNAL)
    }

    companion object { const val MAX_GENERATION = 9_007_199_254_740_991L }
}

internal object JournalCodec {
    private val json = Json { encodeDefaults = true }
    fun encode(journal: InstallationJournal): ByteArray = json.encodeToString(journal).toByteArray(Charsets.UTF_8)
    fun decode(value: ByteArray, namespace: String): InstallationJournal = try {
        json.decodeFromString<InstallationJournal>(value.toString(Charsets.UTF_8)).also { it.validate(namespace) }
    } catch (_: Exception) { fail(FailureKind.CORRUPT_JOURNAL) }
}

// Native Android API 24 support without requiring java.util.Base64 or consumer desugaring.
private const val BASE64URL = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
internal fun newCredential(): String {
    val bytes = ByteArray(32).also { SecureRandom().nextBytes(it) }
    var bits = 0
    var value = 0
    return "ting_ik_" + buildString {
        for (byte in bytes) {
            value = (value shl 8) or (byte.toInt() and 255)
            bits += 8
            while (bits >= 6) { bits -= 6; append(BASE64URL[(value ushr bits) and 63]) }
        }
        if (bits > 0) append(BASE64URL[(value shl (6 - bits)) and 63])
    }
}
internal fun validCredential(value: String): Boolean = value.length == 51 && value.startsWith("ting_ik_") &&
    value.drop(8).all { it in BASE64URL } && BASE64URL.indexOf(value.last()) % 4 == 0
internal fun validPushToken(token: String): Boolean = token.isNotBlank() && token.toByteArray().size <= 4096 &&
    token.none { it.isWhitespace() || it.isISOControl() }
