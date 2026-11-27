package dev.wendy.android

import android.content.Context
import android.graphics.BitmapFactory
import android.util.Base64
import org.json.JSONArray
import org.json.JSONObject
import org.opencv.android.OpenCVLoader
import org.opencv.android.Utils
import org.opencv.calib3d.Calib3d
import org.opencv.core.*
import org.opencv.features2d.BFMatcher
import org.opencv.features2d.SIFT
import org.opencv.imgproc.Imgproc
import java.io.File
import java.util.UUID
import kotlin.math.abs
import kotlin.math.hypot
import kotlin.math.sqrt

// Device recognition (iOS DeviceRecognition.swift and VisualRecognition.cpp): appearance matching with
// SIFT features and a verified homography, plus whole-line device name labels. A match is visual evidence,
// never a hardware identity: opening a match still uses the normal authenticated connection.

/** A point in normalized top-left image coordinates (0…1). */
data class SightPoint(val x: Double, val y: Double)

data class DeviceVisualMatch(val id: String, val label: String, val deviceID: String?, val corners: List<SightPoint>, val inliers: Int, val residual: Double)

/** A saved appearance: a grayscale photo and the device's four corners in it, in pixels. */
data class DeviceVisualReference(val id: String = UUID.randomUUID().toString().uppercase(), val label: String, val deviceID: String?, val width: Int, val height: Int, val gray: ByteArray, val quad: DoubleArray) {
    val valid get() = width in 16..1920 && height in 16..1920 && gray.size == width * height && gray.size <= 3_686_400 && label.isNotEmpty() && label.length <= 256 &&
        (deviceID?.length ?: 0) <= 1024 && quad.size == 8 && quad.withIndex().all { (i, v) -> v.isFinite() && v >= 0 && v < (if (i % 2 == 0) width else height) }
    fun json(): JSONObject = JSONObject().put("id", id).put("label", label).put("width", width).put("height", height)
        .put("gray", Base64.encodeToString(gray, Base64.NO_WRAP)).put("quad", JSONArray(quad.toList())).also { o -> deviceID?.let { o.put("deviceID", it) } }
    override fun equals(other: Any?) = other is DeviceVisualReference && other.id == id
    override fun hashCode() = id.hashCode()
    companion object {
        fun parse(v: JSONObject) = DeviceVisualReference(v.getString("id"), v.getString("label"), v.optString("deviceID").ifEmpty { null }, v.getInt("width"), v.getInt("height"),
            Base64.decode(v.getString("gray"), Base64.DEFAULT), v.getJSONArray("quad").let { a -> DoubleArray(a.length()) { a.getDouble(it) } })
    }
}

class RecognitionException(message: String) : Exception(message)

data class DeviceRecognitionReport(val matches: List<DeviceVisualMatch>, val referenceCount: Int, val keypoints: Int, val bestMatches: Int, val bestInliers: Int, val stage: Int, val usedCenterCrop: Boolean = false) {
    val summary get() = "$referenceCount references · $keypoints features · $bestMatches matches · $bestInliers inliers · stage $stage" + if (usedCenterCrop) " · centre crop" else ""
    val guidance get() = when {
        referenceCount == 0 -> "No appearance references loaded."
        keypoints < 16 -> "Not enough detail. Hold steady in good light."
        matches.isEmpty() -> "No verified match. Centre the board with its lens facing you."
        else -> "Checking the match. Hold steady."
    }
}

/** Matches a camera frame against bundled hardware photos and appearances saved on this phone. */
class DeviceRecognitionEngine(private val context: Context) {
    private class Reference(val source: DeviceVisualReference, val points: List<Point>, val descriptors: Mat, val quad: List<Point>)
    private val registry = File(context.filesDir, "WendyDeviceRecognition/references.json")
    private var references = emptyList<Reference>()
    private var saved = emptyList<DeviceVisualReference>()
    private var loaded = false
    private val sift by lazy { SIFT.create(1800) }
    private val matcher by lazy { BFMatcher.create(Core.NORM_L2, false) }

    @Synchronized fun load() {
        if (loaded) return
        // OpenCV is pinned to 4.10: 4.11+ bundles KleidiCV, which runs SVE2 code whenever the CPU reports SVE2,
        // and the Android emulator on Apple silicon reports SVE2 it cannot execute (SIGILL in detectAndCompute).
        if (!OpenCVLoader.initLocal()) throw RecognitionException("Recognition could not process this image.")
        var records = emptyList<DeviceVisualReference>()
        if (registry.isFile) {
            if (registry.length() > 32L * 1024 * 1024) throw RecognitionException("The saved appearance library could not be read.")
            records = runCatching { JSONArray(registry.readText()).let { a -> (0 until a.length()).map { DeviceVisualReference.parse(a.getJSONObject(it)) } } }.getOrNull()
                ?.takeIf { it.size <= 12 && it.all { r -> r.valid } && it.map { r -> r.id }.toSet().size == it.size } ?: throw RecognitionException("The saved appearance library could not be read.")
        }
        rebuild(records); saved = records; loaded = true
    }
    @Synchronized fun registered() = saved

    @Synchronized fun remember(reference: DeviceVisualReference) {
        if (!reference.valid || reference.deviceID == null) throw RecognitionException("The reference image is invalid.")
        val next = saved.filter { it.deviceID != reference.deviceID } + reference
        if (next.size > 12) throw RecognitionException("The appearance library is full. Remove an old reference first.")
        // Check that the photo has enough detail before changing the library or writing anything.
        prepare(reference) ?: throw RecognitionException("Not enough visible detail. Move closer, improve the lighting, and keep only the device inside the frame.")
        write(next); rebuild(next); saved = next
    }
    @Synchronized fun forget(id: String) { val next = saved.filter { it.id != id }; write(next); rebuild(next); saved = next }

    private fun write(records: List<DeviceVisualReference>) {
        val text = JSONArray(records.map { it.json() }).toString()
        if (text.length > 32 * 1024 * 1024) throw RecognitionException("The appearance library is full. Remove an old reference first.")
        registry.parentFile?.mkdirs()
        SiteModelStore.atomic(registry, text.toByteArray())
    }
    private fun rebuild(records: List<DeviceVisualReference>) {
        val next = (bundled() + records).map { prepare(it) ?: throw RecognitionException("Not enough visible detail. Move closer, improve the lighting, and keep only the device inside the frame.") }
        references = next
    }
    private fun prepare(reference: DeviceVisualReference): Reference? {
        if (!reference.valid) return null
        val quad = (0 until 4).map { Point(reference.quad[it * 2], reference.quad[it * 2 + 1]) }
        if (!RecognitionGeometry.convex(quad) || abs(RecognitionGeometry.area(quad)) < 256) return null
        val image = Mat(reference.height, reference.width, CvType.CV_8U).apply { put(0, 0, reference.gray) }
        val mask = Mat.zeros(reference.height, reference.width, CvType.CV_8U)
        Imgproc.fillConvexPoly(mask, MatOfPoint(*quad.map { Point(Math.round(it.x).toDouble(), Math.round(it.y).toDouble()) }.toTypedArray()), Scalar(255.0))
        val keypoints = MatOfKeyPoint(); val descriptors = Mat()
        sift.detectAndCompute(image, mask, keypoints, descriptors)
        val points = keypoints.toArray().map { it.pt }
        if (points.size < 16) return null
        return Reference(reference, points, descriptors, quad)
    }

    /** Photographs identify appearance only; they are not calibrated metric planes. */
    private fun bundled(): List<DeviceVisualReference> = listOf(
        Triple("seeed-sense", "Seeed XIAO ESP32S3 Sense", doubleArrayOf(730.0, 461.0, 1121.0, 616.0, 1008.0, 880.0, 679.0, 665.0)),
        Triple("seeed-xiao", "Seeed XIAO ESP32S3", doubleArrayOf(480.0, 280.0, 1190.0, 280.0, 1190.0, 1100.0, 480.0, 1100.0)),
    ).mapNotNull { (name, label, quad) ->
        val bitmap = runCatching { context.assets.open("DeviceRecognition/$name.jpg").use { BitmapFactory.decodeStream(it) } }.getOrNull() ?: return@mapNotNull null
        val scale = minOf(1.0, 960.0 / maxOf(bitmap.width, bitmap.height))
        val rgba = Mat(); Utils.bitmapToMat(bitmap, rgba)
        val gray = Mat(); Imgproc.cvtColor(rgba, gray, Imgproc.COLOR_RGBA2GRAY)
        val small = Mat(); Imgproc.resize(gray, small, Size(), scale, scale, Imgproc.INTER_AREA)
        val bytes = ByteArray(small.cols() * small.rows()); small.get(0, 0, bytes)
        DeviceVisualReference(id = "bundled:$name", label = label, deviceID = null, width = small.cols(), height = small.rows(), gray = bytes, quad = DoubleArray(8) { quad[it] * scale })
    }

    /** Detects on the whole frame, then on an unscaled centre crop when a large frame found nothing. */
    @Synchronized fun inspect(gray: ByteArray, width: Int, height: Int): DeviceRecognitionReport {
        val full = detect(gray, width, height)
        if (full.matches.isNotEmpty() || maxOf(width, height) <= 960) return full
        val side = minOf(960, minOf(width, height))
        val left = (width - side) / 2; val top = (height - side) / 2
        val crop = ByteArray(side * side)
        for (row in 0 until side) System.arraycopy(gray, (top + row) * width + left, crop, row * side, side)
        val zoom = detect(crop, side, side).let { r ->
            r.copy(usedCenterCrop = true, matches = r.matches.map { m -> m.copy(corners = m.corners.map { SightPoint((it.x * side + left) / width, (it.y * side + top) / height) }) })
        }
        return if (zoom.stage >= full.stage) zoom else full
    }

    private fun detect(gray: ByteArray, width: Int, height: Int): DeviceRecognitionReport {
        if (width !in 16..4096 || height !in 16..4096 || gray.size != width * height) throw RecognitionException("Recognition could not process this image.")
        var stage = 1; var bestMatches = 0; var bestInliers = 0
        val full = Mat(height, width, CvType.CV_8U).apply { put(0, 0, gray) }
        val scale = minOf(1.0, 960.0 / maxOf(width, height))
        val frame = Mat(); Imgproc.resize(full, frame, Size(), scale, scale, Imgproc.INTER_AREA)
        val keypoints = MatOfKeyPoint(); val descriptors = Mat()
        sift.detectAndCompute(frame, Mat(), keypoints, descriptors)
        val points = keypoints.toArray().map { it.pt }
        if (points.size < 16) return DeviceRecognitionReport(emptyList(), references.size, points.size, 0, 0, stage)
        stage = 2
        val matches = mutableListOf<DeviceVisualMatch>()
        for (reference in references) {
            if (matches.size >= 8) break
            val nearest = mutableListOf<MatOfDMatch>()
            matcher.knnMatch(reference.descriptors, descriptors, nearest, 2)
            val used = BooleanArray(points.size)
            val source = mutableListOf<Point>(); val target = mutableListOf<Point>()
            for (pair in nearest) {
                val p = pair.toArray()
                if (p.size != 2 || p[0].distance >= 0.70f * p[1].distance || used[p[0].trainIdx]) continue
                used[p[0].trainIdx] = true
                source += reference.points[p[0].queryIdx]; target += points[p[0].trainIdx]
            }
            bestMatches = maxOf(bestMatches, source.size)
            if (source.size < 16) continue
            stage = maxOf(stage, 3)
            val inlierMask = Mat()
            val homography = Calib3d.findHomography(MatOfPoint2f(*source.toTypedArray()), MatOfPoint2f(*target.toTypedArray()), Calib3d.RANSAC, 2.5, inlierMask, 1500, 0.995)
            if (homography.empty()) continue
            val mask = ByteArray(inlierMask.rows()); inlierMask.get(0, 0, mask)
            val a = source.filterIndexed { i, _ -> mask.getOrElse(i) { 0 }.toInt() != 0 }; val b = target.filterIndexed { i, _ -> mask.getOrElse(i) { 0 }.toInt() != 0 }
            bestInliers = maxOf(bestInliers, a.size)
            stage = maxOf(stage, 4)
            if (a.size < 16 || a.size < source.size * 0.50) continue
            stage = maxOf(stage, 5)
            if (abs(RecognitionGeometry.area(RecognitionGeometry.hull(a))) < abs(RecognitionGeometry.area(reference.quad)) * 0.12) continue
            stage = maxOf(stage, 6)
            val projected = transform(reference.quad, homography)
            if (!RecognitionGeometry.convex(projected) || RecognitionGeometry.area(projected) * RecognitionGeometry.area(reference.quad) <= 0) continue
            stage = maxOf(stage, 7)
            val area = abs(RecognitionGeometry.area(projected))
            if (area < 1600 || area > frame.total() * 0.85) continue
            if (projected.any { !it.x.isFinite() || !it.y.isFinite() || it.x < 0 || it.y < 0 || it.x >= frame.cols() || it.y >= frame.rows() }) continue
            stage = maxOf(stage, 8)
            val predicted = transform(a, homography)
            val error = sqrt(predicted.indices.sumOf { i -> val dx = predicted[i].x - b[i].x; val dy = predicted[i].y - b[i].y; dx * dx + dy * dy } / b.size)
            if (error > 2) continue
            stage = 9
            matches += DeviceVisualMatch(reference.source.id, reference.source.label, reference.source.deviceID,
                projected.map { SightPoint(it.x / frame.cols(), it.y / frame.rows()) }, a.size, error / scale)
        }
        return DeviceRecognitionReport(matches, references.size, points.size, bestMatches, bestInliers, stage)
    }
    private fun transform(points: List<Point>, homography: Mat): List<Point> {
        val out = MatOfPoint2f(); Core.perspectiveTransform(MatOfPoint2f(*points.toTypedArray()), out, homography); return out.toList()
    }

    /** One camera frame kept on this phone so a missed match can be diagnosed; it is never uploaded. */
    fun saveDiagnostic(gray: ByteArray, width: Int, height: Int, report: String) {
        val file = File(context.filesDir, "RecognitionDiagnostic.json")
        SiteModelStore.atomic(file, JSONObject().put("capturedAt", System.currentTimeMillis()).put("width", width).put("height", height)
            .put("gray", Base64.encodeToString(gray, Base64.NO_WRAP)).put("report", report).toString().toByteArray())
    }
}

/** Plain polygon helpers, so geometry checks do not depend on integer OpenCV contours. */
object RecognitionGeometry {
    fun area(points: List<Point>): Double = points.indices.sumOf { i -> val a = points[i]; val b = points[(i + 1) % points.size]; a.x * b.y - b.x * a.y } / 2
    fun convex(points: List<Point>): Boolean {
        if (points.size < 3) return false
        var sign = 0
        for (i in points.indices) {
            val a = points[i]; val b = points[(i + 1) % points.size]; val c = points[(i + 2) % points.size]
            val cross = (b.x - a.x) * (c.y - b.y) - (b.y - a.y) * (c.x - b.x)
            if (abs(cross) < 1e-9) continue
            val s = if (cross > 0) 1 else -1
            if (sign == 0) sign = s else if (s != sign) return false
        }
        return sign != 0
    }
    fun hull(points: List<Point>): List<Point> {
        val sorted = points.distinctBy { it.x to it.y }.sortedWith(compareBy<Point> { it.x }.thenBy { it.y })
        if (sorted.size < 3) return sorted
        fun cross(o: Point, a: Point, b: Point) = (a.x - o.x) * (b.y - o.y) - (a.y - o.y) * (b.x - o.x)
        val lower = mutableListOf<Point>(); for (p in sorted) { while (lower.size >= 2 && cross(lower[lower.size - 2], lower.last(), p) <= 0) lower.removeAt(lower.lastIndex); lower += p }
        val upper = mutableListOf<Point>(); for (p in sorted.reversed()) { while (upper.size >= 2 && cross(upper[upper.size - 2], upper.last(), p) <= 0) upper.removeAt(upper.lastIndex); upper += p }
        return lower.dropLast(1) + upper.dropLast(1)
    }
}

/** Labels must match a whole observed line; a model name or partial name never becomes a device (iOS DeviceLabelMatcher). */
object DeviceLabelMatcher {
    fun matches(text: String, names: List<String>): List<Int> {
        val value = normalized(text)
        if (value.length < 6) return emptyList()
        return names.indices.filter { normalized(names[it]) == value }
    }
    private fun normalized(text: String) = text.trim().lowercase().split(Regex("\\s+")).filter { it.isNotEmpty() }.joinToString(" ")
}

data class DeviceSight(val id: String, val label: String, val deviceIDs: List<String>, val corners: List<SightPoint>, val evidence: String) {
    val center get() = if (corners.isEmpty()) SightPoint(0.0, 0.0) else SightPoint(corners.sumOf { it.x } / corners.size, corners.sumOf { it.y } / corners.size)
    companion object {
        /** Two references can describe the same object: keep every candidate device in one card. */
        fun group(matches: List<DeviceVisualMatch>): List<DeviceSight> {
            val groups = mutableListOf<MutableList<DeviceVisualMatch>>()
            for (match in matches) {
                val index = groups.indexOfFirst { g -> g.any { overlap(it.corners, match.corners) > 0.4 } }
                if (index >= 0) groups[index] += match else groups += mutableListOf(match)
            }
            return groups.map { group ->
                val first = group.first()
                val ids = group.mapNotNull { it.deviceID }.toSet().sorted()
                DeviceSight(first.id, if (ids.size > 1) "Several devices match" else first.label, ids, first.corners,
                    if (ids.size > 1) "Multiple appearance matches" else if (ids.isEmpty()) "Hardware model match" else "Saved appearance match")
            }
        }
        fun overlap(a: List<SightPoint>, b: List<SightPoint>): Double {
            if (a.isEmpty() || b.isEmpty()) return 0.0
            val ax0 = a.minOf { it.x }; val ax1 = a.maxOf { it.x }; val ay0 = a.minOf { it.y }; val ay1 = a.maxOf { it.y }
            val bx0 = b.minOf { it.x }; val bx1 = b.maxOf { it.x }; val by0 = b.minOf { it.y }; val by1 = b.maxOf { it.y }
            val w = minOf(ax1, bx1) - maxOf(ax0, bx0); val h = minOf(ay1, by1) - maxOf(ay0, by0)
            if (w <= 0 || h <= 0) return 0.0
            val intersection = w * h
            val union = (ax1 - ax0) * (ay1 - ay0) + (bx1 - bx0) * (by1 - by0) - intersection
            return if (union > 0) intersection / union else 0.0
        }
    }
}

/** Only fresh, consistent evidence counts: a sight must appear in two frames under two seconds apart, nearly in place. */
class DeviceSightConfirmation {
    private var previous = emptyMap<String, SightPoint>()
    private var timestamp: Double? = null
    fun accept(sights: List<DeviceSight>, next: Double): List<DeviceSight> {
        if (!next.isFinite()) { reset(); return emptyList() }
        val gap = timestamp?.let { next - it }
        val eligible = gap != null && gap > 0 && gap < 2
        val accepted = if (eligible) sights.filter { sight ->
            val last = previous[sight.id] ?: return@filter false
            hypot(sight.center.x - last.x, sight.center.y - last.y) < 0.15 && sight.corners.all { it.x.isFinite() && it.y.isFinite() }
        } else emptyList()
        previous = sights.associate { it.id to it.center }
        timestamp = next
        return accepted
    }
    fun reset() { previous = emptyMap(); timestamp = null }
}
