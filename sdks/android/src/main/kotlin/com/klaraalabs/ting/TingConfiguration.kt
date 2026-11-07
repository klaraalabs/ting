package com.klaraalabs.ting

import java.net.URI

class TingConfiguration(endpoint: String, val appId: String, val notificationChannelId: String) {
    val endpoint: String
    internal val namespace: String

    init {
        val uri = try { URI(endpoint).normalize() } catch (_: Exception) { fail(FailureKind.INVALID_CONFIGURATION) }
        val scheme = uri.scheme?.lowercase()
        val host = uri.host?.lowercase()
        if (host.isNullOrEmpty() || uri.port !in -1..65535 || uri.userInfo != null || uri.rawQuery != null || uri.rawFragment != null ||
            (scheme != "https" && !(scheme == "http" && host in setOf("localhost", "127.0.0.1", "::1", "[::1]"))) ||
            appId.isEmpty() || appId.toByteArray().size > 256 || appId.any { it.code !in 33..126 } ||
            notificationChannelId.isBlank()) fail(FailureKind.INVALID_CONFIGURATION)
        val port = if ((scheme == "https" && uri.port == 443) || (scheme == "http" && uri.port == 80)) -1 else uri.port
        // Preserve encoded paths. Decoding and re-encoding can merge distinct server namespaces.
        val authority = host + if (port == -1) "" else ":$port"
        this.endpoint = "$scheme://$authority${uri.rawPath.orEmpty().trimEnd('/')}"
        namespace = "${this.endpoint}\n$appId"
    }
}

enum class FailureKind {
    INVALID_CONFIGURATION, INVALID_USER_ID, INVALID_PUSH_TOKEN, CLIENT_ALREADY_EXISTS, CLIENT_CLOSED,
    STORAGE_UNAVAILABLE, CORRUPT_JOURNAL, GENERATION_EXHAUSTED, IDENTITY_UNAVAILABLE,
    PUSH_UNAVAILABLE, INVALID_SERVER_RESPONSE, SERVER,
}

data class TingFailure(val kind: FailureKind, val httpStatus: Int? = null, val serverCode: String? = null)

/** Contains only a safe classification, never the underlying journal, identity proof, or native token. */
class TingException(val failure: TingFailure) : Exception("Ting: ${failure.kind}")

internal fun fail(kind: FailureKind): Nothing = throw TingException(TingFailure(kind))
internal fun validUserId(value: String): Boolean =
    value.isNotBlank() && value.toByteArray().size <= 256 && '\u0000' !in value
