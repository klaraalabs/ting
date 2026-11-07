package com.klaraalabs.ting.http

import com.klaraalabs.ting.*
import com.klaraalabs.ting.installations.RegistrationBody
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.*
import okhttp3.*
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.RequestBody.Companion.toRequestBody
import java.text.SimpleDateFormat
import java.util.Locale
import java.util.TimeZone
import java.util.concurrent.TimeUnit

// Avoid data-class toString implementations that would expose credentials and payloads in diagnostics.
class TingHttpRequest(val method: String, val url: String, val headers: Map<String, String>, val body: String? = null)
class TingHttpResponse(val status: Int, headers: Map<String, String> = emptyMap(), val body: String = "") {
    val headers = headers.mapKeys { it.key.lowercase(Locale.ROOT) }
    internal val errorCode: String get() = runCatching {
        Json.parseToJsonElement(body).jsonObject["error"]!!.jsonObject["code"]!!.jsonPrimitive.content
            .takeIf { it.matches(Regex("[a-z_]{1,64}")) }
    }.getOrNull() ?: "http_error"

    internal fun retryAfter(now: Long): Long? {
        val value = headers["retry-after"] ?: return null
        value.toLongOrNull()?.let { if (it >= 0) return now + it.coerceAtMost(3600) * 1000 }
        return runCatching {
            SimpleDateFormat("EEE, dd MMM yyyy HH:mm:ss z", Locale.US).apply {
                timeZone = TimeZone.getTimeZone("GMT"); isLenient = false
            }.parse(value)?.time
        }.getOrNull()?.coerceAtMost(now + 3_600_000)
    }
}

fun interface TingHttpTransport { suspend fun send(request: TingHttpRequest): TingHttpResponse }

class OkHttpTingTransport : TingHttpTransport {
    private val client = OkHttpClient.Builder()
        .callTimeout(15, TimeUnit.SECONDS).connectTimeout(15, TimeUnit.SECONDS).readTimeout(15, TimeUnit.SECONDS)
        .followRedirects(false).followSslRedirects(false).cookieJar(CookieJar.NO_COOKIES)
        .authenticator(Authenticator.NONE).proxyAuthenticator(Authenticator.NONE).build()

    override suspend fun send(request: TingHttpRequest): TingHttpResponse = withContext(Dispatchers.IO) {
        val body = request.body?.toRequestBody("application/json".toMediaType())
        val wire = Request.Builder().url(request.url).method(request.method, body).apply {
            request.headers.forEach { (key, value) -> header(key, value) }
        }.build()
        client.newCall(wire).execute().use { response ->
            val source = response.body?.source()
            // Installation responses are small. Bound data retained from an untrusted proxy/server.
            if (source?.request(65_537) == true) fail(FailureKind.INVALID_SERVER_RESPONSE)
            TingHttpResponse(response.code, response.headers.toMap(), source?.readUtf8().orEmpty())
        }
    }
}

internal class InstallationApi(private val configuration: TingConfiguration) {
    private val json = Json { encodeDefaults = true }
    private fun url(id: String) = "${configuration.endpoint}/v1/installations/$id"
    fun register(id: String, body: RegistrationBody, proof: String, credential: String?): TingHttpRequest = TingHttpRequest(
        "PUT", url(id), buildMap {
            put("Authorization", "Bearer $proof"); put("X-Ting-App-Id", configuration.appId)
            put("Content-Type", "application/json")
            credential?.let { put("X-Ting-Installation-Token", it) }
        }, json.encodeToString(body),
    )
    fun revoke(id: String, generation: Long, credential: String) = TingHttpRequest("DELETE", url(id), mapOf(
        "Authorization" to "Bearer $credential", "X-Ting-App-Id" to configuration.appId,
        "X-Ting-Installation-Generation" to generation.toString(),
    ))
    fun acknowledges(response: TingHttpResponse, id: String, body: RegistrationBody): Boolean = runCatching {
        val value = Json.parseToJsonElement(response.body).jsonObject
        value["installationId"]?.jsonPrimitive?.content == id &&
            value["installationToken"]?.jsonPrimitive?.content == body.installationToken &&
            value["generation"]?.jsonPrimitive?.long == body.generation &&
            value["status"]?.jsonPrimitive?.content == "registered"
    }.getOrDefault(false)
}
