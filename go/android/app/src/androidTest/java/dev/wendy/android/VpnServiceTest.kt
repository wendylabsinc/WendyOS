package dev.wendy.android

import android.content.Intent
import android.net.VpnService
import androidx.core.content.ContextCompat
import androidx.test.core.app.ApplicationProvider
import androidx.test.ext.junit.runners.AndroidJUnit4
import kotlinx.coroutines.*
import kotlinx.coroutines.flow.first
import org.junit.Assert.*
import org.junit.Assume.assumeTrue
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class VpnServiceTest {
    @Test fun idleTunnelCanDisconnectAndReconnect() = runBlocking {
        val app = ApplicationProvider.getApplicationContext<WendyApplication>()
        // The emulator test setup grants consent explicitly. Never alter a real
        // device's VPN authorization from instrumentation.
        assumeTrue("Grant VPN consent on the test emulator first", VpnService.prepare(app) == null)
        repeat(2) {
            try {
                ContextCompat.startForegroundService(app, Intent(app, WendyVpnService::class.java).setAction(WendyVpnService.START))
                withTimeout(15000) { WendyVpnService.phase.first { it == VpnPhase.Connected } }
                // An empty directory must still support turning mesh on.
                assertFalse(WendyVpnService.internetBackup.value)
                assertTrue(WendyVpnService.engaged)
                delay(if (it == 0) 6000 else 300) // Include an automatic refresh, then an idle restart.
                assertEquals(VpnPhase.Connected, WendyVpnService.phase.value)
            } finally {
                app.startService(Intent(app, WendyVpnService::class.java).setAction(WendyVpnService.STOP))
                withTimeout(5000) { WendyVpnService.phase.first { it == VpnPhase.Disconnected } }
            }
            assertTrue(WendyVpnService.activeRoutes.value.isEmpty())
            assertNull(WendyVpnService.failure.value)
            delay(300) // Wait for Android to finish destroying this Service.
        }
    }
}
