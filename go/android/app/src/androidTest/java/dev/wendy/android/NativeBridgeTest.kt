package dev.wendy.android

import androidx.test.core.app.ApplicationProvider
import androidx.test.core.app.ActivityScenario
import androidx.test.ext.junit.runners.AndroidJUnit4
import kotlinx.coroutines.runBlocking
import org.json.JSONArray
import org.junit.Assert.*
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class NativeBridgeTest {
    @Test fun nativeBackendInitializesAndRejectsStaleSessions() = runBlocking {
        val app = ApplicationProvider.getApplicationContext<WendyApplication>()
        val backend = app.backend
        backend.initialize()
        assertTrue(backend.call("Accounts") is JSONArray)
        try { backend.call("DeviceInfo", 12345); fail("A stale session was accepted") }
        catch (e: BackendFailure) { assertTrue(e.message!!.isNotBlank()) }
        try { backend.call("Unknown 🤖 東京"); fail("Unknown method was accepted") }
        catch (e: BackendFailure) { assertTrue(e.message!!.contains("🤖 東京")) }
    }
    @Test fun androidDiscoveryReturnsThroughJNI() = runBlocking {
        val backend = ApplicationProvider.getApplicationContext<WendyApplication>().backend
        backend.initialize()
        assertTrue(backend.call("Nearby") is JSONArray)
    }
    @Test fun activitySurvivesRecreation() {
        ActivityScenario.launch(MainActivity::class.java).use { scenario ->
            scenario.recreate()
            scenario.onActivity { assertFalse(it.isFinishing) }
        }
    }
}
