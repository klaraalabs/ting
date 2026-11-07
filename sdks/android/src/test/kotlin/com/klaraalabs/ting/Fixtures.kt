package com.klaraalabs.ting

import com.klaraalabs.ting.http.*
import com.klaraalabs.ting.installations.*
import kotlinx.coroutines.CompletableDeferred
import kotlinx.serialization.json.*
import java.io.IOException

internal class MemoryStorage : InstallationStorage {
    private val records = mutableMapOf<String, ByteArray>()
    var writes = 0
    var failWrite: Int? = null
    @Synchronized override fun load(namespace: String) = records[namespace]?.copyOf()
    @Synchronized override fun save(namespace: String, value: ByteArray) {
        writes++
        if (failWrite == writes) { failWrite = null; throw IOException("injected write failure") }
        records[namespace] = value.copyOf()
    }
    fun failNext() { failWrite = writes + 1 }
    fun journal(config: TingConfiguration) = JournalCodec.decode(load(config.namespace)!!, config.namespace)
}

internal class TestPush : PushRegistrationProvider {
    var authorization = PushPermission.ALLOWED
    var currentToken = "fcm-token-a"
    var tokenGate: Gate? = null
    override suspend fun permission() = authorization
    override suspend fun token(): String {
        val result = currentToken
        tokenGate?.let { tokenGate = null; it.pause() }
        return result
    }
}
internal class Gate {
    val entered = CompletableDeferred<Unit>()
    val release = CompletableDeferred<Unit>()
    suspend fun pause() { entered.complete(Unit); release.await() }
}

internal data class Record(val user: String, val body: RegistrationBody, val generation: Long, val active: Boolean, val superseded: Boolean = false)
internal class ProtocolStore {
    val records = mutableMapOf<String, Record>()
    val requests = mutableListOf<TingHttpRequest>()
}

/** Checked against both canonical Go HTTP fixtures. It is not an actual Go server or identity verifier. */
internal class ProtocolServer(val store: ProtocolStore = ProtocolStore()) : TingHttpTransport {
    var dropNextPut = false
    var dropNextDelete = false
    var putGate: Gate? = null
    var afterPut: (() -> Unit)? = null
    var nextResponse: TingHttpResponse? = null
    override suspend fun send(request: TingHttpRequest): TingHttpResponse {
        store.requests.add(request)
        nextResponse?.let { nextResponse = null; return it }
        if (request.method == "PUT") putGate?.let { putGate = null; it.pause() }
        val id = request.url.substringAfterLast('/')
        val proof = request.headers["Authorization"]?.removePrefix("Bearer ")
        val old = store.records[id]
        if (request.method == "DELETE") {
            if (old == null || proof != old.body.installationToken) return error(401, "installation_credential_invalid")
            val generation = request.headers["X-Ting-Installation-Generation"]!!.toLong()
            if (generation < old.generation) return error(409, "installation_superseded")
            if (generation == old.generation && old.active) return error(409, "installation_generation_conflict")
            store.records[id] = old.copy(generation = generation, active = false)
            if (dropNextDelete) { dropNextDelete = false; throw IOException("lost delete response") }
            return TingHttpResponse(204)
        }
        val body = Json.decodeFromString<RegistrationBody>(request.body!!)
        if (!validCredential(body.installationToken) || body.generation <= 0) return error(400, "invalid_request")
        if (old != null) {
            val currentProof = request.headers["X-Ting-Installation-Token"] == old.body.installationToken
            val proposedProof = body.installationToken == old.body.installationToken
            if (!currentProof && !proposedProof) return error(401, "installation_credential_invalid")
            if (old.superseded || body.generation < old.generation) return error(409, "installation_superseded")
            if (body.generation == old.generation) {
                if (!old.active) return error(409, "installation_superseded")
                if (!proposedProof || proof != old.user || body != old.body) return error(409, "installation_generation_conflict")
                return acknowledgment(id, body, 200)
            }
            if (!currentProof) return error(401, "installation_credential_invalid")
            if ((old.user != proof || !old.active) && proposedProof) return error(400, "invalid_request")
        }
        store.records.entries.toList().forEach { (key, record) ->
            if (key != id && record.body.pushToken == body.pushToken) store.records[key] = record.copy(active = false, superseded = true)
        }
        store.records[id] = Record(proof!!, body, body.generation, true)
        afterPut?.let { afterPut = null; it() }
        if (dropNextPut) { dropNextPut = false; throw IOException("lost put response") }
        return acknowledgment(id, body, if (old == null) 201 else 200)
    }
    fun error(status: Int, code: String) = TingHttpResponse(status, body = """{"error":{"code":"$code"}}""")
    private fun acknowledgment(id: String, body: RegistrationBody, status: Int) = TingHttpResponse(status, body = buildJsonObject {
        put("installationId", id); put("installationToken", body.installationToken)
        put("generation", body.generation); put("status", "registered")
    }.toString())
}
