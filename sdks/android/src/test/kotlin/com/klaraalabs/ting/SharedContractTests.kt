package com.klaraalabs.ting

import com.klaraalabs.ting.http.InstallationApi
import com.klaraalabs.ting.installations.*
import kotlinx.coroutines.test.runTest
import kotlinx.serialization.json.*
import org.junit.Assert.*
import org.junit.Test
import java.io.File
import java.util.UUID

class SharedContractTests {
    @Test fun lifecycleFixtures() = runTest { runFixture("installation-lifecycle.json") }
    @Test fun transferFixtures() = runTest { runFixture("installation-transfer.json") }

    private suspend fun runFixture(name: String) {
        val fixture = Json.parseToJsonElement(File(System.getProperty("ting.contracts"), name).readText()).jsonObject
        assertEquals(1, fixture["version"]!!.jsonPrimitive.int)
        val config = TingConfiguration("https://ting.test", "app_contract", "messages")
        val api = InstallationApi(config)
        fixture["scenarios"]!!.jsonArray.forEach { raw ->
            val scenario = raw.jsonObject
            val store = ProtocolStore()
            var server = ProtocolServer(store)
            val ids = listOf("default", "old", "replacement").associateWith { UUID.randomUUID().toString() }
            val credentials = listOf("first", "second", "third").associateWith { newCredential() }
            for (entry in scenario["steps"]!!.jsonArray) {
                val step = entry.jsonObject
                fun string(key: String) = step[key]?.jsonPrimitive?.content
                if (step["restart"]?.jsonPrimitive?.boolean == true) server = ProtocolServer(store)
                val id = ids.getValue(string("installation") ?: "default")
                val generation = step["generation"]!!.jsonPrimitive.long
                val credential = string("credential")?.let { credentials.getValue(it) }
                val request = if (string("method") == "DELETE") api.revoke(id, generation, credential!!) else {
                    api.register(id, RegistrationBody(pushToken = "fcm-${string("push")}",
                        installationToken = credentials.getValue(string("proposed")!!), generation = generation), string("user")!!, credential)
                }
                val response = server.send(request)
                assertEquals("${scenario["name"]} $step", step["status"]!!.jsonPrimitive.int, response.status)
                string("error")?.let { assertEquals(it, response.errorCode) }
            }
            if (name == "installation-lifecycle.json") {
                val record = store.records.getValue(ids.getValue("default"))
                assertEquals(scenario["finalUser"]!!.jsonPrimitive.content, record.user)
                assertEquals(credentials[scenario["finalCredential"]!!.jsonPrimitive.content], record.body.installationToken)
                assertEquals(scenario["finalGeneration"]!!.jsonPrimitive.long, record.generation)
                assertEquals(scenario["finalActive"]!!.jsonPrimitive.boolean, record.active)
            } else {
                val old = store.records.getValue(ids.getValue("old"))
                assertTrue(old.superseded)
                assertFalse(old.active)
                assertEquals(scenario["oldGeneration"]!!.jsonPrimitive.long, old.generation)
                val replacement = store.records.getValue(ids.getValue("replacement"))
                assertEquals("bob", replacement.user)
                assertTrue(replacement.active)
                assertEquals(listOf("bob"), store.records.values.filter { it.active }.map { it.user })
            }
        }
    }
}
