package dev.wendy.android

import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test

class VpnNetworkSettingsTest {
    @Test fun emptyMeshStillInstallsDnsWithoutTakingInternet() {
        val settings = VpnNetworkSettings.parse(JSONObject("""{"routes":[]}"""))
        assertEquals(listOf("100.64.0.53" to 32), settings.routes)
        assertFalse(settings.internetBackup)
        assertNull(settings.address6)
    }
    @Test fun everyDiscoveredDeviceGetsARoute() {
        val settings = VpnNetworkSettings.parse(JSONObject("""{"routes":[{"address":"10.99.1.1"},{"address":"10.99.2.2"}]}"""))
        assertTrue(settings.routes.containsAll(listOf("10.99.1.1" to 32, "10.99.2.2" to 32)))
        assertFalse(settings.routes.any { it.second == 0 })
    }
    @Test fun backupCapturesBothFamiliesAndRecoveryRemovesDefaultRoutes() {
        val config = JSONObject("""{"routes":[],"internet":{"address4":"10.88.1.1","mtu":1280}}""")
        val backup = VpnNetworkSettings.parse(config)
        assertTrue(backup.internetBackup)
        assertEquals("10.88.1.1", backup.address4)
        assertEquals(1280, backup.mtu)
        assertNotNull(backup.address6)
        assertTrue(backup.routes.containsAll(listOf("0.0.0.0" to 0, "::" to 0)))
        config.remove("internet")
        val recovered = VpnNetworkSettings.parse(config)
        assertFalse(recovered.internetBackup)
        assertFalse(recovered.routes.any { it.second == 0 })
        assertNull(recovered.address6)
    }
}
