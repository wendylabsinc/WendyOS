package dev.wendy.android

import org.json.JSONArray
import org.json.JSONObject
import java.nio.ByteBuffer
import java.nio.ByteOrder
import java.security.MessageDigest
import kotlin.math.abs
import kotlin.math.acos
import kotlin.math.hypot
import kotlin.math.max
import kotlin.math.min
import kotlin.math.sqrt
import kotlin.math.tan

/*
 * Camera calibration for Sites, as in iOS PR #2715: a camera's lens and its pose in a site model are saved on the
 * gateway (CalibratedFrameService), not in the model, so every phone sees the same alignment. Types mirror
 * SiteCameraGeometry.swift and SiteVisualMap.swift.
 */

/** A pinhole lens with optional Brown–Conrady distortion, in the capture's pixels (iOS SiteCameraLens). */
data class SiteCameraLens(val width: Int, val height: Int, val fx: Double, val fy: Double, val cx: Double, val cy: Double, val distortionModel: String,
                          val distortion: List<Double>, val measured: Boolean, val note: String, val captureMode: String) {
    val alignmentResidualLimit get() = (if (measured) 2.5 else 5.0) * max(width, height) / SiteLocalizationLimits.REFERENCE_SIZE

    val valid get() = width in 1..8192 && height in 1..8192 && listOf(fx, fy, cx, cy).all { it.isFinite() } && fx > 0 && fy > 0 && fx <= 1e7 && fy <= 1e7 &&
        cx in 0.0..width.toDouble() && cy in 0.0..height.toDouble() && captureMode.isNotEmpty() && note.isNotEmpty() &&
        (distortionModel == "none" && distortion.isEmpty() || distortionModel == "brown_conrady" && distortion.size == 5 && distortion.all { it.isFinite() && abs(it) <= 100 })

    /** fx, fy, cx, cy, k1, k2, p1, p2, k3 for the solver. */
    fun known(): DoubleArray = doubleArrayOf(fx, fy, cx, cy) + (if (distortionModel == "none") DoubleArray(5) else distortion.toDoubleArray())

    /**
     * The optical (right, down, forward) unit ray through a pixel. Brown–Conrady is inverted only when it converges
     * and re-distorts to the same pixel; no default field of view is substituted.
     */
    fun ray(x: Double, y: Double): DoubleArray? {
        if (!valid) return null
        val xd = (x - cx) / fx; val yd = (y - cy) / fy
        var u = xd; var v = yd
        if (distortionModel == "brown_conrady") {
            val (k1, k2, p1, p2, k3) = distortion
            repeat(40) {
                val r2 = u * u + v * v; val radial = 1 + r2 * (k1 + r2 * (k2 + r2 * k3))
                if (!radial.isFinite() || radial <= 0.01) return null
                val du = 2 * p1 * u * v + p2 * (r2 + 2 * u * u); val dv = p1 * (r2 + 2 * v * v) + 2 * p2 * u * v
                u = (xd - du) / radial; v = (yd - dv) / radial
            }
            val r2 = u * u + v * v; val radial = 1 + r2 * (k1 + r2 * (k2 + r2 * k3))
            val checkX = (u * radial + 2 * p1 * u * v + p2 * (r2 + 2 * u * u) - xd) * fx
            val checkY = (v * radial + p1 * (r2 + 2 * v * v) + 2 * p2 * u * v - yd) * fy
            if (!checkX.isFinite() || !checkY.isFinite() || hypot(checkX, checkY) >= 0.1) return null
        }
        if (!u.isFinite() || !v.isFinite()) return null
        val length = sqrt(u * u + v * v + 1)
        return doubleArrayOf(u / length, v / length, 1 / length)
    }

    private fun angle(a: DoubleArray?, b: DoubleArray?): Double? = if (a == null || b == null) null else Math.toDegrees(acos((a[0] * b[0] + a[1] * b[1] + a[2] * b[2]).coerceIn(-1.0, 1.0)))
    val horizontalFOV get() = angle(ray(0.0, cy), ray(width.toDouble(), cy))
    val verticalFOV get() = angle(ray(cx, 0.0), ray(cx, height.toDouble()))

    /** 32 rays around the image edge, or null when any cannot be inverted. */
    val boundaryRays: List<DoubleArray>? get() {
        val w = width.toDouble(); val h = height.toDouble(); val result = ArrayList<DoubleArray>()
        for (edge in 0 until 4) for (step in 0 until 8) {
            val t = step / 8.0
            val (x, y) = when (edge) { 0 -> t * w to 0.0; 1 -> w to t * h; 2 -> (1 - t) * w to h; else -> 0.0 to (1 - t) * h }
            result += ray(x, y) ?: return null
        }
        return result
    }
}

/** A camera's position and orientation in one model revision; [rotation] is x, y, z, w (iOS SiteCameraPose). */
data class SiteCameraPose(val modelRevision: String?, val position: SitePoint, val rotation: List<Double>, val method: Int, val provenance: String, val observedAt: Long, val validUntil: Long) {
    fun isCurrent(revision: String, now: Long): Boolean = modelRevision != null && modelRevision.equals(revision, ignoreCase = true) && position.valid && method in 1..2 &&
        provenance.isNotEmpty() && rotation.size == 4 && rotation.all { it.isFinite() } && abs(rotation.sumOf { it * it } - 1) < 0.001 &&
        observedAt <= now + 30_000 && validUntil > now && validUntil > observedAt && validUntil - observedAt <= 7L * 24 * 60 * 60 * 1000

    /** Optical camera axes in the model world, column-major 3×3 (the convention of [SiteMath]). */
    fun matrix(): DoubleArray = SiteQuaternion.matrix(rotation)
}

data class SiteCameraGeometry(val pose: SiteCameraPose, val lens: SiteCameraLens?)

/** One camera's saved calibration on its gateway (iOS SiteCameraMetadata). */
data class SiteCameraMetadata(val sourceID: String, val revision: Long, val lens: SiteCameraLens?, val poses: List<SiteCameraPose>) {
    fun pose(site: Site, now: Long): SiteCameraPose? = poses.filter { it.isCurrent(site.modelRevision, now) }.singleOrNull()

    companion object {
        /** Reads one pb.CameraMetadata as the shared Go backend encodes it, with the iOS bridge's validity rules. */
        fun parse(value: JSONObject): SiteCameraMetadata {
            val intrinsics = value.optJSONObject("intrinsics")
            val model = value.optString("distortion_model")
            val distortion = value.optJSONArray("distortion")?.let { a -> (0 until a.length()).map { a.optDouble(it) } }.orEmpty()
            val provenance = intrinsics?.optInt("provenance") ?: 0
            val consistent = !(model == "brown_conrady" && distortion.size != 5 || model == "none" && distortion.isNotEmpty())
            val lens = if (intrinsics != null && (provenance == 1 || provenance == 2) && consistent) SiteCameraLens(intrinsics.optInt("width"), intrinsics.optInt("height"),
                intrinsics.optDouble("fx", 0.0), intrinsics.optDouble("fy", 0.0), intrinsics.optDouble("cx", 0.0), intrinsics.optDouble("cy", 0.0), model,
                if (model == "brown_conrady") distortion else emptyList(), provenance == 1, intrinsics.optString("note"), value.optString("capture_mode")) else null
            val poses = value.list("site_poses").map { p ->
                val position = p.optJSONArray("position"); val rotation = p.optJSONArray("rotation")
                // Keep malformed remote arrays invalid rather than indexing outside them.
                val point = if (position?.length() == 3 && rotation?.length() == 4) SitePoint(position.optDouble(0), position.optDouble(1), position.optDouble(2)) else SitePoint(0.0, 0.0, 0.0)
                val quaternion = if (position?.length() == 3 && rotation?.length() == 4) (0 until 4).map { rotation.optDouble(it) } else listOf(0.0, 0.0, 0.0, 0.0)
                SiteCameraPose(p.optString("model_revision").ifEmpty { null }, point, quaternion, p.optInt("method"), p.optString("provenance"),
                    p.optLong("observed_at_unix_ms"), p.optLong("valid_until_unix_ms"))
            }
            return SiteCameraMetadata(value.optString("stable_id"), value.optLong("revision"), lens, poses)
        }
    }
}

object SiteQuaternion {
    /** x, y, z, w from a column-major rotation matrix (simd_quatd(matrix)), normalized. */
    fun from(m: DoubleArray): List<Double> {
        fun at(row: Int, col: Int) = m[col * 3 + row]
        val trace = at(0, 0) + at(1, 1) + at(2, 2)
        val q = when {
            trace > 0 -> { val s = sqrt(trace + 1) * 2; listOf((at(2, 1) - at(1, 2)) / s, (at(0, 2) - at(2, 0)) / s, (at(1, 0) - at(0, 1)) / s, s / 4) }
            at(0, 0) > at(1, 1) && at(0, 0) > at(2, 2) -> { val s = sqrt(1 + at(0, 0) - at(1, 1) - at(2, 2)) * 2; listOf(s / 4, (at(0, 1) + at(1, 0)) / s, (at(0, 2) + at(2, 0)) / s, (at(2, 1) - at(1, 2)) / s) }
            at(1, 1) > at(2, 2) -> { val s = sqrt(1 + at(1, 1) - at(0, 0) - at(2, 2)) * 2; listOf((at(0, 1) + at(1, 0)) / s, s / 4, (at(1, 2) + at(2, 1)) / s, (at(0, 2) - at(2, 0)) / s) }
            else -> { val s = sqrt(1 + at(2, 2) - at(0, 0) - at(1, 1)) * 2; listOf((at(0, 2) + at(2, 0)) / s, (at(1, 2) + at(2, 1)) / s, s / 4, (at(1, 0) - at(0, 1)) / s) }
        }
        val length = sqrt(q.sumOf { it * it })
        return q.map { it / length }
    }

    /** Column-major 3×3 from x, y, z, w. */
    fun matrix(q: List<Double>): DoubleArray {
        val (x, y, z, w) = q
        return doubleArrayOf(1 - 2 * (y * y + z * z), 2 * (x * y + z * w), 2 * (x * z - y * w),
            2 * (x * y - z * w), 1 - 2 * (x * x + z * z), 2 * (y * z + x * w),
            2 * (x * z + y * w), 2 * (y * z - x * w), 1 - 2 * (x * x + y * y))
    }

    /** The rotation angle between two orientations, in radians (2·acos|q₁·q₂|). */
    fun angle(a: List<Double>, b: List<Double>) = 2 * acos(min(1.0, abs(a.indices.sumOf { a[it] * b[it] })))
}

/** A manual visualization as camera geometry: an unmeasured centred pinhole lens and a one-day pose (iOS). */
fun SiteManualAlignment.geometry(position: SitePoint, revision: String, now: Long): SiteCameraGeometry? {
    if (!revision.equals(modelRevision, ignoreCase = true) || !position.valid || !valid) return null
    val focal = width / (2 * tan(Math.toRadians(horizontalFOV) / 2))
    val lens = SiteCameraLens(width, height, focal, focal, width / 2.0, height / 2.0, "none", emptyList(), false, "Manually aligned", "Manual ${width}x$height")
    return SiteCameraGeometry(SiteCameraPose(revision, position, SiteQuaternion.from(rotation()), 1, "Manual visualization", now, now + 86_400_000), lens)
}

/** Shared by viewing and placement: never borrow calibration from another gateway, revision or ambiguous source (iOS MobileSiteCoveragePolicy). */
object SiteCoverage {
    fun geometry(camera: SiteCamera, site: Site, records: List<SiteCameraMetadata>, gateway: SiteGatewayIdentity, now: Long): SiteCameraGeometry? {
        if (!gateway.matches(camera.gatewayID)) return null
        val manual = camera.manualAlignment; val position = camera.position
        if (manual != null && position != null) return manual.geometry(position, site.modelRevision, now)
        val record = records.filter { it.sourceID == camera.sourceID }.singleOrNull() ?: return null
        val pose = record.pose(site, now) ?: return null
        val lens = record.lens?.takeIf { it.valid && it.boundaryRays != null } ?: return null
        if (position != null && (abs(position.x - pose.position.x) >= 0.001 || abs(position.y - pose.position.y) >= 0.001 || abs(position.z - pose.position.z) >= 0.001)) return null
        return SiteCameraGeometry(pose, lens)
    }
}

/** A luma frame decoded for calibration only (iOS CameraCalibrationFrame). */
class CameraCalibrationFrame(val gray: ByteArray, val width: Int, val height: Int, val received: Long, val sequence: Long, val codec: String) {
    fun isFresh(now: Long) = now - received in 0..3000
}

data class SiteLocalizationResult(val lens: SiteCameraLens, val pose: SiteCameraPose, val inliers: Int, val residual: Double, val fittedLens: Boolean)

/** iOS SiteLocalizationError, with the same wording. */
class SiteLocalizationException(val kind: Kind, val reason: Int = 0, val matches: Int = 0, val inliers: Int = 0) : Exception() {
    enum class Kind { InvalidMap, InsufficientMap, InvalidImage, InvalidResult, NoSolution }
    override val message: String get() = when (kind) {
        Kind.InvalidMap -> "The room’s visual reference map is invalid or belongs to a different model."
        Kind.InsufficientMap -> "Not enough visual references. Scan textured objects at different depths in the area the camera sees."
        Kind.InvalidImage -> "A fresh camera image is needed to calibrate and align this camera."
        Kind.InvalidResult -> "The estimated camera geometry did not pass validation. No calibration was saved."
        Kind.NoSolution -> when (reason) {
            1 -> "Found $matches possible matches. Lens calibration needs at least ${SiteLocalizationLimits.MIN_LENS_INLIERS} consistent points; alignment with a measured lens needs ${SiteLocalizationLimits.MIN_POSE_INLIERS}, or ${SiteLocalizationLimits.MIN_ESTIMATED_INLIERS} for an approximate position."
            2 -> "Found $matches possible matches, but only $inliers agree on the camera geometry. Include more distinctive objects in the scan and camera view."
            3 -> "The matches are concentrated in a small part of the image. Try Estimated Alignment, or measure the lens with your phone screen."
            7 -> "These matches lie on a flat surface, so the lens cannot be measured from this view alone. Use Estimated Alignment or Phone Screen Calibration; the room does not need to change."
            8 -> "The viewing direction is plausible, but the camera’s distance is ambiguous. Different lens estimates fit this view at different positions. No position was saved."
            4, 5 -> "The $inliers geometric matches did not give a stable calibration across validation checks. Include clearer views of the camera’s scene and try again."
            else -> "Automatic calibration could not solve this view. No calibration was saved."
        }
    }
}

/** The scan's landmark map, saved beside a model (iOS SiteVisualMap, Swift Codable JSON). */
class SiteVisualMap(val version: Int, val modelRevision: String, val modelSHA256: String, val capturedAt: Double, val frameCount: Int, val landmarks: ByteArray) {
    val count get() = landmarks.size / (SiteLocalizationLimits.LANDMARK_FLOATS * 4)

    fun floats(): FloatArray = FloatArray(landmarks.size / 4).also { ByteBuffer.wrap(landmarks).order(ByteOrder.LITTLE_ENDIAN).asFloatBuffer().get(it) }

    fun validate(revision: String, model: ByteArray? = null) {
        val invalid = SiteLocalizationException(SiteLocalizationException.Kind.InvalidMap)
        if (version != 1 || !modelRevision.equals(revision, ignoreCase = true) || frameCount !in 1..200 || count !in 120..SiteLocalizationLimits.MAX_LANDMARKS ||
            landmarks.size != count * SiteLocalizationLimits.LANDMARK_FLOATS * 4 || modelSHA256.length != 64 || !modelSHA256.all { it.isDigit() || it.lowercaseChar() in 'a'..'f' } ||
            !capturedAt.isFinite()) throw invalid
        if (model != null && digest(model) != modelSHA256.lowercase()) throw invalid
        floats().forEachIndexed { i, v -> if (!v.isFinite() || (if (i % SiteLocalizationLimits.LANDMARK_FLOATS < 3) abs(v) > 1000 else v < 0 || v > 512)) throw invalid }
    }

    fun json(): JSONObject = JSONObject().put("version", version).put("modelRevision", modelRevision).put("modelSHA256", modelSHA256)
        .put("capturedAt", capturedAt).put("frameCount", frameCount).put("landmarks", java.util.Base64.getEncoder().encodeToString(landmarks))

    companion object {
        const val MAXIMUM_BYTES = 12 * 1024 * 1024
        fun digest(data: ByteArray) = MessageDigest.getInstance("SHA-256").digest(data).joinToString("") { "%02x".format(it) }
        fun parse(bytes: ByteArray): SiteVisualMap {
            if (bytes.size > MAXIMUM_BYTES) throw SiteLocalizationException(SiteLocalizationException.Kind.InvalidMap)
            return try {
                val v = JSONObject(String(bytes, Charsets.UTF_8))
                SiteVisualMap(v.getInt("version"), v.getString("modelRevision"), v.getString("modelSHA256"), v.getDouble("capturedAt"), v.getInt("frameCount"),
                    java.util.Base64.getDecoder().decode(v.getString("landmarks")))
            } catch (e: Exception) { throw SiteLocalizationException(SiteLocalizationException.Kind.InvalidMap) }
        }
    }
}

/** Serial access to one site's landmark map (iOS SiteLocalizationEngine and its storage). */
class SiteLocalizationEngine {
    private var map = SiteLandmarkMap()
    private var frames = 0

    @Synchronized fun load(visualMap: SiteVisualMap, revision: String, model: ByteArray) {
        visualMap.validate(revision, model)
        val next = SiteLandmarkMap()
        if (next.import(visualMap.floats(), visualMap.count) != visualMap.count) throw SiteLocalizationException(SiteLocalizationException.Kind.InvalidMap)
        map = next; frames = visualMap.frameCount
    }

    /** Loads a stored site's map; throws [SiteLocalizationException.Kind.InsufficientMap] when the model has none. */
    fun load(site: StoredSite) {
        val file = site.visualMap ?: throw SiteLocalizationException(SiteLocalizationException.Kind.InsufficientMap)
        if (!file.isFile || file.length() > SiteVisualMap.MAXIMUM_BYTES) throw SiteLocalizationException(SiteLocalizationException.Kind.InvalidMap)
        load(SiteVisualMap.parse(file.readBytes()), site.site.modelRevision, site.model.readBytes())
    }

    @Synchronized fun localize(gray: ByteArray, width: Int, height: Int, revision: String, lens: SiteCameraLens?, estimated: Boolean = false, now: Long = System.currentTimeMillis()): SiteLocalizationResult {
        if (width !in 32..8192 || height !in 32..8192 || width.toLong() * height > 32_000_000 || gray.size != width * height) throw SiteLocalizationException(SiteLocalizationException.Kind.InvalidImage)
        val existing = lens?.takeIf { it.valid && (it.measured || estimated) && it.width == width && it.height == height }
        val known = existing?.known()
        val output = when {
            known != null -> SiteSolver.localize(map, gray, width, height, known, estimated = existing.measured.not())
            else -> SiteSolver.localize(map, gray, width, height, null, estimated)
        }
        if (!output.success) throw SiteLocalizationException(SiteLocalizationException.Kind.NoSolution, output.reason, output.matches, output.inliers)
        val t = output.worldFromCamera
        val rotation = SiteQuaternion.from(doubleArrayOf(t[0], t[1], t[2], t[4], t[5], t[6], t[8], t[9], t[10]))
        val approximate = output.fittedLens == 2 || existing?.measured == false
        val rms = "%.2f".format(java.util.Locale.ROOT, output.rms)
        val pose = SiteCameraPose(revision, SitePoint(t[12], t[13], t[14]), rotation, 2,
            (if (approximate) "Approximate pose using an assumed pinhole lens. " else "") + "Matched camera image to iPhone LiDAR room references; held-out RMS $rms px",
            now, now + 7L * 24 * 60 * 60 * 1000)
        val fitted = SiteCameraLens(width, height, output.fx, output.fy, output.cx, output.cy, "brown_conrady", output.distortion.toList(), !approximate,
            if (approximate) "Estimated pinhole lens from scene matches; centred principal point and zero distortion assumed. FOV and camera position are approximate."
            else "Joint fit against iPhone LiDAR scene points; square pixels, radial distortion; held-out RMS $rms px",
            "JPEG ${width}x$height")
        val resultLens = existing ?: fitted
        if (!pose.isCurrent(revision, now) || !resultLens.valid || resultLens.boundaryRays == null) throw SiteLocalizationException(SiteLocalizationException.Kind.InvalidResult)
        return SiteLocalizationResult(resultLens, pose, output.inliers, output.rms, output.fittedLens != 0)
    }
}

/** A second fresh frame must agree with the first before an alignment is saved (iOS SiteAlignmentValidation). */
object SiteAlignmentValidation {
    fun verified(solution: SiteLocalizationResult, verification: SiteLocalizationResult, first: CameraCalibrationFrame, second: CameraCalibrationFrame): SiteLocalizationResult {
        if (second.received <= first.received || second.sequence == first.sequence || first.width != second.width || first.height != second.height || first.codec != second.codec)
            throw SiteLocalizationException(SiteLocalizationException.Kind.InvalidImage)
        val a = solution.pose.position; val b = verification.pose.position
        val distance = sqrt((a.x - b.x) * (a.x - b.x) + (a.y - b.y) * (a.y - b.y) + (a.z - b.z) * (a.z - b.z))
        val angle = SiteQuaternion.angle(solution.pose.rotation, verification.pose.rotation)
        if (distance > (if (solution.lens.measured) 0.08 else 0.30) || angle > Math.toRadians(if (solution.lens.measured) 3.0 else 10.0))
            throw SiteException("The camera position changed between frames. Waiting for a stable view.")
        return SiteLocalizationResult(solution.lens, verification.pose, min(solution.inliers, verification.inliers), max(solution.residual, verification.residual), solution.fittedLens)
    }
}

/** The pb.CameraMetadata the shared Go backend saves, built as the iOS bridge builds it (WDCameraAlignmentSave). */
object SiteCalibrationRecords {
    private fun distortion(lens: SiteCameraLens) = JSONArray(if (lens.distortionModel == "none") List(5) { 0.0 } else lens.distortion)
    private fun intrinsics(lens: SiteCameraLens) = JSONObject().put("width", lens.width).put("height", lens.height).put("fx", lens.fx).put("fy", lens.fy)
        .put("cx", lens.cx).put("cy", lens.cy).put("provenance", if (lens.measured) 1 else 2).put("note", lens.note)

    fun alignment(sourceID: String, result: SiteLocalizationResult): JSONObject {
        val pose = result.pose; val lens = result.lens
        val revision = pose.modelRevision ?: throw SiteLocalizationException(SiteLocalizationException.Kind.InvalidResult)
        val minimum = if (!lens.measured) SiteLocalizationLimits.MIN_ESTIMATED_INLIERS else if (result.fittedLens) SiteLocalizationLimits.MIN_LENS_INLIERS else SiteLocalizationLimits.MIN_POSE_INLIERS
        if (!lens.valid || !pose.isCurrent(revision, System.currentTimeMillis()) || result.inliers < minimum || !result.residual.isFinite() || result.residual > lens.alignmentResidualLimit)
            throw SiteLocalizationException(SiteLocalizationException.Kind.InvalidResult)
        val sitePose = JSONObject().put("model_revision", revision).put("position", JSONArray(listOf(pose.position.x, pose.position.y, pose.position.z)))
            .put("rotation", JSONArray(pose.rotation)).put("method", 2).put("provenance", pose.provenance).put("observed_at_unix_ms", pose.observedAt)
            .put("valid_until_unix_ms", pose.validUntil).put("reprojection_rms_pixels", result.residual).put("inlier_count", result.inliers)
        return JSONObject().put("stable_id", sourceID).put("capture_mode", lens.captureMode).put("intrinsics", intrinsics(lens))
            .put("distortion_model", "brown_conrady").put("distortion", distortion(lens)).put("calibrated_at_unix_ms", pose.observedAt).put("site_poses", JSONArray(listOf(sitePose)))
    }

    fun lens(sourceID: String, lens: SiteCameraLens): JSONObject {
        if (!lens.valid || !lens.measured || lens.boundaryRays == null) throw SiteLocalizationException(SiteLocalizationException.Kind.InvalidResult)
        return JSONObject().put("stable_id", sourceID).put("capture_mode", lens.captureMode).put("intrinsics", intrinsics(lens))
            .put("distortion_model", "brown_conrady").put("distortion", distortion(lens)).put("calibrated_at_unix_ms", System.currentTimeMillis())
    }
}

suspend fun WendyModel.cameraMetadata(session: Long): List<SiteCameraMetadata> = backend.call("CameraMetadata", session).items().map(SiteCameraMetadata::parse)
suspend fun WendyModel.saveCameraAlignment(session: Long, sourceID: String, expectedRevision: Long, result: SiteLocalizationResult) {
    backend.call("SaveCameraAlignment", session, SiteCalibrationRecords.alignment(sourceID, result), expectedRevision)
}
suspend fun WendyModel.saveCameraLens(session: Long, sourceID: String, expectedRevision: Long, lens: SiteCameraLens) {
    backend.call("SaveCameraLens", session, SiteCalibrationRecords.lens(sourceID, lens), expectedRevision)
}

/** The phone-screen lens measurement with the iOS wording (ScreenLensCalibration). */
object ScreenLensMeasurement {
    fun lens(fit: SiteLocalizationOutput, width: Int, height: Int, codec: String): SiteCameraLens {
        if (!fit.success) throw SiteException(failure(fit.reason))
        val lens = SiteCameraLens(width, height, fit.fx, fit.fy, fit.cx, fit.cy, "brown_conrady", fit.distortion.toList(), true,
            "Measured using a 6x9 phone-screen checkerboard across ${fit.matches} views; withheld validation RMS ${"%.2f".format(java.util.Locale.ROOT, fit.rms)} px", "$codec ${width}x$height")
        if (!lens.valid || lens.boundaryRays == null) throw SiteLocalizationException(SiteLocalizationException.Kind.InvalidResult)
        return lens
    }

    fun failure(reason: Int) = when (reason) {
        ScreenLensReason.VIEWS -> "More distinct views are needed. Move and tilt the checkerboard, pausing at each position."
        ScreenLensReason.COVERAGE -> "More of the camera image needs samples. Move the screen toward the edges, keeping its white border visible."
        ScreenLensReason.TILT -> "The captured views are too similar in angle. Bring the left edge closer, then the right edge; repeat with the top and bottom edges. Pause at each angle."
        ScreenLensReason.UNCERTAIN -> "The views do not constrain the lens accurately enough. Add tilted views at different distances, keeping the whole checkerboard visible."
        ScreenLensReason.DISAGREEMENT -> "Different groups of views give different lens measurements. Keep the pattern flat and unchanged; add sharp views at several angles and distances."
        ScreenLensReason.FIT -> "The lens model does not fit the captured corners accurately enough. Keep the pattern flat, avoid glare, and pause so each new view is sharp."
        ScreenLensReason.VALIDATION -> "The lens fit failed on separate validation views. Add sharp, tilted views near the image edges, keeping the whole checkerboard visible."
        else -> "The lens calculation failed. Cancel and restart calibration. No new calibration was saved."
    }
}

/** What to do next with the phone (iOS ScreenLensProgress.phoneMotion / nextStep). */
enum class ScreenLensSide(val title: String) { Left("Left"), Right("Right"), Top("Top"), Bottom("Bottom") }

fun ScreenLensProgress.hasSample(side: ScreenLensSide): Boolean = count > 0 && when (side) {
    ScreenLensSide.Left -> minX <= 0.35; ScreenLensSide.Right -> maxX >= 0.65; ScreenLensSide.Top -> minY <= 0.35; ScreenLensSide.Bottom -> maxY >= 0.65
}
fun ScreenLensProgress.shouldSolve(after: Long) = count >= 12 && coverageReady && revision >= after && revision - after >= 3
val ScreenLensProgress.instruction: String get() = when {
    detection == 0 -> "Face this screen toward the camera. Keep the whole checkerboard visible."
    detection == 1 -> "Move the phone closer to the camera. Keep the whole white border visible."
    detection == 2 -> "Move the phone back toward the middle of the camera preview."
    !coverageReady -> ScreenLensSide.entries.firstOrNull { !hasSample(it) }?.let { "Move the phone toward the ${it.title.lowercase()} of the camera preview." }
        ?: "Move the phone closer to the camera. Keep the whole white border visible."
    // Advance the demonstration only with accepted samples, never with time.
    (revision / 2) % 2 == 0L -> "Bring the left edge closer to the camera, then the right edge. Pause at each angle."
    else -> "Bring the top edge closer to the camera, then the bottom edge. Pause at each angle."
}
