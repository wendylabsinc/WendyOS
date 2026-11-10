package dev.wendy.android

import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test

class NearbySetupTest {
    private fun obs(s: SetupSnapshot, selection: Boolean = true, wifi: Boolean = false, status: Double = 0.0, scan: Double = 0.0) = s.observation(selection, wifi, status, scan)

    @Test fun attemptedEnrollmentIsNeverRetriedAutomatically() {
        val attempted = SetupSnapshot(session = 1, connected = true, attempted = true)
        // Without a pending Cloud check the loop only keeps the device status current.
        assertEquals(SetupSnapshot.Observation.Status, obs(attempted, status = 30.0))
        assertEquals(SetupSnapshot.Observation.Idle, obs(attempted, status = 1.0))
        assertEquals(SetupSnapshot.Observation.CheckEnrollment, obs(attempted.copy(pending = true), status = 1.0))
        assertEquals(SetupSnapshot.Observation.Reconnect, obs(attempted.copy(connected = false, canResume = true), status = 30.0))
        assertEquals(SetupSnapshot.Observation.Idle, obs(attempted.copy(enrolled = true)))
    }

    @Test fun freshSetupDiscoversThenConnects() {
        assertEquals(SetupSnapshot.Observation.Discover, obs(SetupSnapshot(), selection = false))
        assertEquals(SetupSnapshot.Observation.Reconnect, obs(SetupSnapshot()))
        assertEquals(SetupSnapshot.Observation.ScanWiFi, obs(SetupSnapshot(connected = true), wifi = true, scan = 21.0))
        assertEquals(SetupSnapshot.Observation.Idle, obs(SetupSnapshot(completed = true, pending = true)))
    }

    @Test fun failedStepKeepsItsState() {
        val snapshot = SetupSnapshot.parse(JSONObject("""{"State":{"Session":9,"Attempted":true,"Pending":true,"Networks":["Lab","Office"]},"Error":"device pki enrollment failed: i/o timeout"}"""))
        assertEquals(9L, snapshot.session)
        assertTrue(snapshot.attempted && snapshot.pending)
        assertEquals(listOf("Lab", "Office"), snapshot.networks)
        assertEquals("device pki enrollment failed: i/o timeout", snapshot.error)
        assertNull(SetupSnapshot.parse(JSONObject("""{"State":{"Session":1}}""")).error)
    }

    @Test fun promptInvitesEachStrongAdvertisementOnce() {
        val history = SetupPromptHistory()
        val devices = listOf(SetupDevice("a", "wendyos-a", -70), SetupDevice("b", "wendyos-b", -50), SetupDevice("c", "far", -95), SetupDevice("d", "unknown", 0))
        assertEquals("b", history.next(devices)?.address)
        assertEquals("a", history.next(devices)?.address)
        assertNull(history.next(devices))
        assertEquals("B", SetupDevice("b", "wendyos-b", -50).title)
        assertEquals("Wendy device", SetupDevice("x", "", -50).title)
    }

    @Test fun setupMovesThroughFourStepsLikeIOS() {
        val flow = SetupFlow()
        assertEquals(SetupFlow.Step.Device, flow.step)
        flow.continueFromNetwork(connected = true)
        assertEquals("Continuing needs a chosen device first", SetupFlow.Step.Device, flow.step)
        flow.choseDevice(); assertEquals(SetupFlow.Step.Network, flow.step)
        flow.continueFromNetwork(connected = false); assertEquals(SetupFlow.Step.Network, flow.step)
        flow.continueFromNetwork(connected = false, useEthernet = true); assertEquals(SetupFlow.Step.Organization, flow.step)
        flow.backToNetwork(); assertEquals(SetupFlow.Step.Network, flow.step)
        flow.reset(); assertEquals(SetupFlow.Step.Device, flow.step)
        assertEquals(listOf("Device", "Connection", "Organization", "Enrollment"), SetupFlow.Step.entries.map { it.title })
    }

    @Test fun wifiLimitsAreProtocolBytes() {
        assertTrue(SetupFlow.validWiFi("Office", ""))
        assertFalse(SetupFlow.validWiFi("   ", "secret"))
        assertFalse(SetupFlow.validWiFi("é".repeat(17), ""))
        assertTrue(SetupFlow.validWiFi("é".repeat(16), "p".repeat(64)))
        assertFalse(SetupFlow.validWiFi("Office", "p".repeat(65)))
    }
}
