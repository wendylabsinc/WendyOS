package dev.wendy.android

import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test
import java.io.File
import java.nio.file.Files

class SitesTest {
    private fun fixture(name: String) = javaClass.classLoader!!.getResourceAsStream("sites/$name")!!.readBytes()
    private fun temporary(): File = Files.createTempDirectory("sites").toFile().also { it.deleteOnExit() }

    @Test fun siteJsonMatchesSwiftCodableAndValidates() {
        val camera = SiteCamera(gatewayID = "cloud-v2/a/b", sourceID = "usb-1", name = "Door", position = SitePoint(1.0, 2.0, 3.0))
        val site = Site(name = "Warehouse", cameras = listOf(camera))
        val json = site.json()
        assertEquals(setOf("version", "id", "name", "modelRevision", "metresPerModelUnit", "cameras"), json.keys().asSequence().toSet())
        // Swift omits nil optionals instead of writing null.
        assertFalse(json.getJSONArray("cameras").getJSONObject(0).has("manualAlignment"))
        assertEquals(site, Site.parse(JSONObject(json.toString())))
        site.validate()
        assertThrows(SiteException::class.java) { site.copy(name = " ").validate() }
        assertThrows(SiteException::class.java) { site.copy(cameras = listOf(camera, camera.copy(id = "E621E1F8-C36C-495A-93FC-0C247A3E6E5F"))).validate() }
        assertThrows(SiteException::class.java) { site.copy(version = 2).validate() }
        // A manual alignment belongs to one model revision and needs a position.
        val manual = SiteManualAlignment(site.modelRevision, 180.0, 0.0, 180.0, 70.0, 1600, 1200)
        site.copy(cameras = listOf(camera.copy(manualAlignment = manual))).validate()
        assertThrows(SiteException::class.java) { site.copy(cameras = listOf(camera.copy(manualAlignment = manual.copy(modelRevision = Site(name = "x").modelRevision)))).validate() }
        assertThrows(SiteException::class.java) { site.copy(cameras = listOf(camera.copy(position = null, manualAlignment = manual))).validate() }
    }

    @Test fun archivesRoundTripInBothVersions() {
        val site = Site(name = "Lab")
        val model = fixture("room.usdz")
        val v1 = SiteArchive.parse(SiteArchive(site, model).data())
        assertEquals(site, v1.site); assertArrayEquals(model, v1.model); assertNull(v1.visualMap)
        val bytes = SiteArchive(site, model, "{}".toByteArray()).data()
        assertEquals("WENDYST2", String(bytes, 0, 8))
        val v2 = SiteArchive.parse(bytes)
        assertArrayEquals("{}".toByteArray(), v2.visualMap)
        assertThrows(SiteException::class.java) { SiteArchive.parse(bytes.copyOf(bytes.size - 1)) }
        assertThrows(SiteException::class.java) { SiteArchive(site, "not a zip".toByteArray()) }
        assertEquals("a-b-c.wendysite", SiteModelStore.shareFilename("a/b:c"))
        assertEquals("Digital Twin.wendysite", SiteModelStore.shareFilename(" .. "))
    }

    @Test fun storeLinksModelsAndKeepsPlacementsWithTheirRevision() {
        val base = temporary()
        val store = SitesStore(base, "acct", "org")
        val id = store.create("Warehouse", listOf(SiteAsset("76/42", "Pi", false), SiteAsset("unitree:B42", "Go2", true)))
        assertEquals("1 devices · 1 robots", store.current?.assetSummary)
        val saved = store.addModel("Floor 1", fixture("scene.usdz"), id)
        assertEquals(saved.id, store.current?.defaultModel)
        store.renameModel(saved.id, id, "Ground floor")
        assertEquals("Ground floor", store.models.load(saved.id).site.name)
        val placed = store.models.load(saved.id).site.let { it.copy(cameras = listOf(SiteCamera(gatewayID = "g", sourceID = "s", name = "Cam", position = SitePoint(0.0, 1.0, 0.0)))) }
        store.saveCameraModel(placed, id, "Cam placed")
        assertEquals(1, store.models.load(saved.id).site.cameras.size)
        // A placement for another revision is refused.
        assertThrows(SiteException::class.java) { store.saveCameraModel(placed.copy(modelRevision = Site(name = "x").modelRevision), id, "moved") }
        // An exported archive imports as a new model with its placements.
        val copy = store.addModel("", store.models.archive(saved.id).data(), id)
        assertNotEquals(saved.id, copy.id)
        assertEquals(1, copy.site.cameras.size)
        // Reopening reads the same document; another organization is separate.
        val reopened = SitesStore(base, "acct", "org")
        assertEquals(listOf("Warehouse"), reopened.sites.map { it.name })
        assertEquals(listOf("Ground floor", "Ground floor"), reopened.sites[0].models.map { it.name })
        assertTrue(reopened.events.any { it.text == "Cam placed" })
        assertTrue(SitesStore(base, "acct", "other").sites.isEmpty())
        // Damaged geometry is never saved.
        assertThrows(Exception::class.java) { store.addModel("bad", byteArrayOf(0x50, 0x4b, 3, 4, 0, 0), id) }
    }

    @Test fun gatewayIdentitiesComeFromCloudEnrollmentOrSerial() {
        val tenant = "11111111-2222-3333-4444-555555555555"; val asset = "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"
        assertEquals("cloud-v2/$tenant/${asset.lowercase()}", SiteGatewayIdentity.cloud("cloud://api.wendy.dev:443/tenant/$tenant/asset/$asset"))
        assertNull(SiteGatewayIdentity.cloud("pi.local:50051"))
        val enrolled = SiteGatewayIdentity.resolve(null, true, tenant, asset, "SN1")
        assertEquals("cloud-v2/$tenant/${asset.lowercase()}", enrolled.id)
        assertTrue(enrolled.matches("hardware-v1/$tenant/SN1"))
        assertEquals("hardware-v1//SN1", SiteGatewayIdentity.resolve(null, false, "", "", "SN1").id)
        assertThrows(SiteException::class.java) { SiteGatewayIdentity.resolve("cloud-v2/x/y", true, tenant, asset, null) }
        assertThrows(SiteException::class.java) { SiteGatewayIdentity.resolve(null, false, "", "", "") }
    }

    @Test fun camerasLookAlongMinusZByDefaultAndRaysHitTheModel() {
        val manual = SiteManualAlignment(Site(name = "x").modelRevision, 180.0, 0.0, 180.0, 70.0, 1600, 1200)
        val forward = SiteMath.apply3(manual.rotation(), doubleArrayOf(0.0, 0.0, 1.0))
        val down = SiteMath.apply3(manual.rotation(), doubleArrayOf(0.0, 1.0, 0.0))
        assertArrayEquals(doubleArrayOf(0.0, 0.0, -1.0), forward, 1e-9)
        assertArrayEquals(doubleArrayOf(0.0, -1.0, 0.0), down, 1e-9)
        val marker = siteMarker(SiteCamera(gatewayID = "g", sourceID = "s", name = "c", position = SitePoint(0.0, 1.0, 0.0), manualAlignment = manual), false, false)!!
        assertEquals(15, marker.frustum!!.size)
        // Straight down onto the SceneKit fixture's floor-level wall bottom.
        val geometry = UsdReader.read(fixture("scene.usdz"))
        val hit = SiteRay.nearest(geometry, floatArrayOf(0f, 10f, -2f), floatArrayOf(0f, -1f, 0f))!!
        assertEquals(2.5f, hit[1], 1e-3f)
        assertNull(SiteRay.nearest(geometry, floatArrayOf(50f, 10f, 50f), floatArrayOf(0f, -1f, 0f)))
    }
}
