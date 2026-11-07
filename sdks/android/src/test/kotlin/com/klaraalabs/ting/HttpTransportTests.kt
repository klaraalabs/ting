package com.klaraalabs.ting

import com.klaraalabs.ting.http.*
import com.klaraalabs.ting.installations.*
import kotlinx.coroutines.test.runTest
import okhttp3.mockwebserver.MockResponse
import okhttp3.mockwebserver.MockWebServer
import org.junit.Assert.*
import org.junit.Test
import java.util.UUID
import java.util.concurrent.TimeUnit

class HttpTransportTests {
    @Test fun realTransportSendsAndroidBodyAndOwnershipHeaders() = runTest {
        MockWebServer().use { server ->
            server.start()
            val config = TingConfiguration(server.url("/push/").toString(), "app_test", "messages")
            val api = InstallationApi(config)
            val body = RegistrationBody(pushToken = "fcm-token", installationToken = newCredential(), generation = 42)
            val id = UUID.randomUUID().toString()
            val previous = newCredential()
            server.enqueue(MockResponse().setResponseCode(201).setBody("""{"installationId":"$id","installationToken":"${body.installationToken}","generation":42,"status":"registered"}"""))
            val transport = OkHttpTingTransport()
            val response = transport.send(api.register(id, body, "identity-jwt", previous))
            assertTrue(api.acknowledges(response, id, body))
            val request = server.takeRequest(1, TimeUnit.SECONDS)!!
            assertEquals("PUT", request.method)
            assertEquals("/push/v1/installations/$id", request.path)
            assertEquals("Bearer identity-jwt", request.getHeader("Authorization"))
            assertEquals("app_test", request.getHeader("X-Ting-App-Id"))
            assertEquals(previous, request.getHeader("X-Ting-Installation-Token"))
            val json = request.body.readUtf8()
            assertTrue(json.contains("\"platform\":\"android\""))
            assertFalse(json.contains("apnsEnvironment"))
            server.enqueue(MockResponse().setResponseCode(429).setHeader("Retry-After", "120")
                .setBody("""{"error":{"code":"rate_limited"}}"""))
            val limited = transport.send(api.revoke(id, 43, body.installationToken))
            assertEquals("rate_limited", limited.errorCode)
            assertEquals(121_000L, limited.retryAfter(1000))
            val delete = server.takeRequest(1, TimeUnit.SECONDS)!!
            assertEquals("DELETE", delete.method)
            assertEquals("43", delete.getHeader("X-Ting-Installation-Generation"))
            assertEquals("Bearer ${body.installationToken}", delete.getHeader("Authorization"))
        }
    }

    @Test fun refusesRedirectsAndDoesNotRetainCookies() = runTest {
        MockWebServer().use { source -> MockWebServer().use { target ->
            source.start(); target.start()
            source.enqueue(MockResponse().setResponseCode(302).setHeader("Location", target.url("/capture"))
                .setHeader("Set-Cookie", "session=secret"))
            val transport = OkHttpTingTransport()
            val response = transport.send(TingHttpRequest("GET", source.url("/first").toString(), mapOf("Authorization" to "Bearer private")))
            assertEquals(302, response.status)
            assertEquals(0, target.requestCount)
            source.takeRequest()
            source.enqueue(MockResponse().setResponseCode(204))
            transport.send(TingHttpRequest("GET", source.url("/second").toString(), emptyMap()))
            assertNull(source.takeRequest().getHeader("Cookie"))
        } }
    }

    @Test fun rejectsWrongAcknowledgmentAndOversizedResponse() = runTest {
        val api = InstallationApi(TingConfiguration("https://ting.test", "app", "messages"))
        val id = UUID.randomUUID().toString()
        val body = RegistrationBody(pushToken = "fcm", installationToken = newCredential(), generation = 7)
        assertFalse(api.acknowledges(TingHttpResponse(200, body = "{}"), id, body))
        assertFalse(api.acknowledges(TingHttpResponse(200, body = """{"installationId":"$id","installationToken":"${body.installationToken}","generation":6,"status":"registered"}"""), id, body))
        MockWebServer().use { server ->
            server.start(); server.enqueue(MockResponse().setBody("a".repeat(65_537)))
            try {
                OkHttpTingTransport().send(TingHttpRequest("GET", server.url("/").toString(), emptyMap()))
                fail("expected response limit")
            } catch (error: TingException) { assertEquals(FailureKind.INVALID_SERVER_RESPONSE, error.failure.kind) }
        }
    }

    @Test fun retryAfterDateAndSafeErrorFallback() {
        val date = TingHttpResponse(503, mapOf("retry-after" to "Thu, 01 Jan 1970 00:30:00 GMT"))
        assertEquals(1_800_000L, date.retryAfter(0))
        assertNull(TingHttpResponse(429, mapOf("retry-after" to "-1")).retryAfter(0))
        assertEquals("http_error", TingHttpResponse(401, body = """{"error":{"code":"private identity value"}}""").errorCode)
    }
}
