package dev.wendy.android

import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test
import java.nio.ByteBuffer
import java.nio.ByteOrder

class SiteCalibrationTest {
    private val revision = Site(name = "Lab").modelRevision
    private val now = 1_800_000_000_000L
    private val gateway = SiteGatewayIdentity("cloud-v2/a/b", setOf("hardware-v1/a/SN1"))

    /** pb.CameraMetadata as the Go backend's encoding/json writes it. */
    private fun record(source: String = "usb-1", provenance: Int = 1, position: List<Double> = listOf(1.0, 2.0, 3.0), rotation: List<Double> = listOf(0.0, 0.0, 0.0, 1.0),
                       modelRevision: String = revision.lowercase(), validUntil: Long = now + 3_600_000) = JSONObject()
        .put("stable_id", source).put("revision", 7).put("capture_mode", "H.264 1280x960").put("distortion_model", "brown_conrady")
        .put("distortion", JSONArray(listOf(-0.1, 0.02, 0.0, 0.0, 0.0)))
        .put("intrinsics", JSONObject().put("width", 1280).put("height", 960).put("fx", 900.0).put("fy", 900.0).put("cx", 640.0).put("cy", 480.0).put("provenance", provenance).put("note", "Measured"))
        .put("site_poses", JSONArray(listOf(JSONObject().put("model_revision", modelRevision).put("position", JSONArray(position)).put("rotation", JSONArray(rotation))
            .put("method", 2).put("provenance", "Matched").put("observed_at_unix_ms", now - 1000).put("valid_until_unix_ms", validUntil))))

    @Test fun readsGatewayRecordsWithTheBridgeRules() {
        val parsed = SiteCameraMetadata.parse(record())
        assertEquals("usb-1", parsed.sourceID); assertEquals(7L, parsed.revision)
        val lens = parsed.lens!!
        assertTrue(lens.valid && lens.measured); assertEquals(listOf(-0.1, 0.02, 0.0, 0.0, 0.0), lens.distortion); assertEquals("H.264 1280x960", lens.captureMode)
        // Model revisions compare as UUIDs, whatever their case.
        assertNotNull(parsed.pose(Site(name = "Lab", modelRevision = revision), now))
        assertFalse(SiteCameraMetadata.parse(record(provenance = 2)).lens!!.measured)
        assertNull(SiteCameraMetadata.parse(record(provenance = 0)).lens)
        // Distortion that does not match its model hides the lens; malformed pose arrays never become a pose.
        assertNull(SiteCameraMetadata.parse(record().put("distortion", JSONArray(listOf(0.1)))).lens)
        assertNull(SiteCameraMetadata.parse(record(position = listOf(1.0, 2.0))).pose(Site(name = "Lab", modelRevision = revision), now))
        assertNull(SiteCameraMetadata.parse(record(rotation = listOf(0.0, 0.0, 0.5, 0.5))).pose(Site(name = "Lab", modelRevision = revision), now))
        assertNull(SiteCameraMetadata.parse(record(validUntil = now - 1)).pose(Site(name = "Lab", modelRevision = revision), now))
    }

    @Test fun coverageNeverBorrowsCalibration() {
        val site = Site(name = "Lab", modelRevision = revision)
        val camera = SiteCamera(gatewayID = "hardware-v1/a/SN1", sourceID = "usb-1", name = "Door")
        val records = listOf(SiteCameraMetadata.parse(record()))
        val geometry = SiteCoverage.geometry(camera, site, records, gateway, now)!!
        assertEquals(SitePoint(1.0, 2.0, 3.0), geometry.pose.position)
        // Another gateway, an ambiguous source, a different saved placement or another revision gives nothing.
        assertNull(SiteCoverage.geometry(camera.copy(gatewayID = "cloud-v2/x/y"), site, records, gateway, now))
        assertNull(SiteCoverage.geometry(camera, site, records + records, gateway, now))
        assertNull(SiteCoverage.geometry(camera.copy(position = SitePoint(1.5, 2.0, 3.0)), site, records, gateway, now))
        assertNotNull(SiteCoverage.geometry(camera.copy(position = SitePoint(1.0, 2.0, 3.0)), site, records, gateway, now))
        assertNull(SiteCoverage.geometry(camera, Site(name = "Lab"), records, gateway, now))
        // A manual visualization wins over the gateway's record.
        val manual = SiteManualAlignment(revision, 180.0, 0.0, 180.0, 70.0, 1600, 1200)
        val shown = SiteCoverage.geometry(camera.copy(position = SitePoint(0.0, 1.0, 0.0), manualAlignment = manual), site, records, gateway, now)!!
        assertEquals("Manual visualization", shown.pose.provenance); assertFalse(shown.lens!!.measured)
        assertEquals(70.0, shown.lens.horizontalFOV!!, 1e-6)
        assertArrayEquals(manual.rotation(), shown.pose.matrix(), 1e-9)
    }

    @Test fun quaternionsRoundTripMatrices() {
        for ((yaw, pitch, roll) in listOf(Triple(0.0, 0.0, 0.0), Triple(180.0, 0.0, 180.0), Triple(37.0, -20.0, 95.0), Triple(-170.0, 80.0, -10.0))) {
            val m = SiteManualAlignment(revision, yaw, pitch, roll, 60.0, 640, 480).rotation()
            val q = SiteQuaternion.from(m)
            assertEquals(1.0, q.sumOf { it * it }, 1e-9)
            assertArrayEquals(m, SiteQuaternion.matrix(q), 1e-9)
            assertEquals(0.0, SiteQuaternion.angle(q, q.map { -it }), 1e-6)
        }
    }

    @Test fun lensRaysInvertDistortion() {
        val lens = SiteCameraLens(1280, 960, 900.0, 900.0, 640.0, 480.0, "brown_conrady", listOf(-0.2, 0.05, 0.001, -0.001, 0.0), true, "Measured", "H.264 1280x960")
        assertArrayEquals(doubleArrayOf(0.0, 0.0, 1.0), lens.ray(640.0, 480.0)!!, 1e-12)
        assertEquals(32, lens.boundaryRays!!.size)
        // A ray re-distorts to its pixel.
        val r = lens.ray(100.0, 80.0)!!; val u = r[0] / r[2]; val v = r[1] / r[2]
        val (k1, k2, p1, p2, k3) = lens.distortion; val r2 = u * u + v * v; val radial = 1 + r2 * (k1 + r2 * (k2 + r2 * k3))
        assertEquals(100.0, (u * radial + 2 * p1 * u * v + p2 * (r2 + 2 * u * u)) * 900 + 640, 0.1)
        assertEquals(80.0, (v * radial + p1 * (r2 + 2 * v * v) + 2 * p2 * u * v) * 900 + 480, 0.1)
        assertFalse(lens.copy(distortion = listOf(0.1)).valid); assertFalse(lens.copy(note = "").valid); assertFalse(lens.copy(cx = 2000.0).valid)
    }

    private fun result(inliers: Int = 40, residual: Double = 1.0, measured: Boolean = true, position: SitePoint = SitePoint(1.0, 2.0, 3.0), observed: Long = System.currentTimeMillis()) = SiteLocalizationResult(
        SiteCameraLens(1280, 960, 900.0, 900.0, 640.0, 480.0, "brown_conrady", listOf(0.0, 0.0, 0.0, 0.0, 0.0), measured, "Joint fit", "H.264 1280x960"),
        SiteCameraPose(revision, position, listOf(0.0, 0.0, 0.0, 1.0), 2, "Matched", observed, observed + 86_400_000), inliers, residual, true)

    @Test fun savedRecordsMatchTheIOSBridge() {
        val saved = SiteCalibrationRecords.alignment("usb-1", result())
        assertEquals(setOf("stable_id", "capture_mode", "intrinsics", "distortion_model", "distortion", "calibrated_at_unix_ms", "site_poses"), saved.keys().asSequence().toSet())
        assertEquals(1, saved.getJSONObject("intrinsics").getInt("provenance"))
        val pose = saved.getJSONArray("site_poses").getJSONObject(0)
        assertEquals(2, pose.getInt("method")); assertEquals(4, pose.getJSONArray("rotation").length()); assertEquals(40, pose.getInt("inlier_count"))
        // The saved record reads back as the same geometry.
        val back = SiteCameraMetadata.parse(saved.put("revision", 1))
        assertEquals(SitePoint(1.0, 2.0, 3.0), back.pose(Site(name = "Lab", modelRevision = revision), System.currentTimeMillis())!!.position)
        assertEquals(2, SiteCalibrationRecords.alignment("usb-1", result(measured = false, inliers = 9, residual = 4.0)).getJSONObject("intrinsics").getInt("provenance"))
        // Weak solutions are never saved.
        assertThrows(SiteLocalizationException::class.java) { SiteCalibrationRecords.alignment("usb-1", result(inliers = 10)) }
        assertThrows(SiteLocalizationException::class.java) { SiteCalibrationRecords.alignment("usb-1", result(residual = 6.0)) }
        assertThrows(SiteLocalizationException::class.java) { SiteCalibrationRecords.alignment("usb-1", result(observed = System.currentTimeMillis() - 2 * 86_400_000)) }
        assertFalse(SiteCalibrationRecords.lens("usb-1", result().lens).has("site_poses"))
        assertThrows(SiteLocalizationException::class.java) { SiteCalibrationRecords.lens("usb-1", result(measured = false).lens) }
    }

    @Test fun aSecondFrameMustAgree() {
        val first = CameraCalibrationFrame(ByteArray(4), 2, 2, 1000, 1, "H.264"); val second = CameraCalibrationFrame(ByteArray(4), 2, 2, 2000, 2, "H.264")
        val verified = SiteAlignmentValidation.verified(result(inliers = 50), result(inliers = 30, position = SitePoint(1.02, 2.0, 3.0)), first, second)
        assertEquals(30, verified.inliers); assertEquals(1.02, verified.pose.position.x, 1e-9)
        assertThrows(SiteException::class.java) { SiteAlignmentValidation.verified(result(), result(position = SitePoint(1.1, 2.0, 3.0)), first, second) }
        assertThrows(SiteLocalizationException::class.java) { SiteAlignmentValidation.verified(result(), result(), first, first) }
        // An estimated lens tolerates more movement.
        SiteAlignmentValidation.verified(result(measured = false), result(measured = false, position = SitePoint(1.2, 2.0, 3.0)), first, second)
    }

    @Test fun visualMapsReadSwiftCodableJSON() {
        val model = "model bytes".toByteArray()
        val landmarks = ByteBuffer.allocate(120 * 131 * 4).order(ByteOrder.LITTLE_ENDIAN).apply { repeat(120 * 131) { putFloat(if (it % 131 < 3) 1.5f else 10f) } }.array()
        // Swift's JSONEncoder writes Data as base64, UUID uppercase and Date as seconds since 2001.
        val json = JSONObject().put("version", 1).put("modelRevision", revision).put("modelSHA256", SiteVisualMap.digest(model)).put("capturedAt", 784_000_000.5)
            .put("frameCount", 12).put("landmarks", java.util.Base64.getEncoder().encodeToString(landmarks))
        val map = SiteVisualMap.parse(json.toString().toByteArray())
        assertEquals(120, map.count); assertEquals(1.5f, map.floats()[0]); map.validate(revision.lowercase(), model)
        assertThrows(SiteLocalizationException::class.java) { map.validate(Site(name = "x").modelRevision, model) }
        assertThrows(SiteLocalizationException::class.java) { map.validate(revision, "other".toByteArray()) }
        val damaged = landmarks.copyOf().also { ByteBuffer.wrap(it).order(ByteOrder.LITTLE_ENDIAN).putFloat(20, 600f) }
        assertThrows(SiteLocalizationException::class.java) { SiteVisualMap.parse(json.put("landmarks", java.util.Base64.getEncoder().encodeToString(damaged)).toString().toByteArray()).validate(revision) }
        assertThrows(SiteLocalizationException::class.java) { SiteVisualMap.parse("{}".toByteArray()) }
        assertEquals(json.getString("modelSHA256"), SiteVisualMap.parse(map.json().toString().toByteArray()).modelSHA256)
    }

    @Test fun screenLensGuidanceFollowsSamples() {
        assertTrue(ScreenLensProgress().instruction.startsWith("Face this screen"))
        assertTrue(ScreenLensProgress(count = 1, detection = 1).instruction.startsWith("Move the phone closer"))
        assertTrue(ScreenLensProgress(count = 3, detection = 4, minX = .5, maxX = .7, minY = .2, maxY = .8).instruction.contains("left"))
        val ready = ScreenLensProgress(count = 12, detection = 4, revision = 9, coverageReady = true)
        assertTrue(ready.shouldSolve(6)); assertFalse(ready.shouldSolve(7)); assertFalse(ready.copy(count = 11).shouldSolve(0))
    }
}
