package dev.wendy.android

import org.opencv.calib3d.Calib3d
import org.opencv.core.Core
import org.opencv.core.CvType
import org.opencv.core.Mat
import org.opencv.core.MatOfDMatch
import org.opencv.core.MatOfDouble
import org.opencv.core.MatOfKeyPoint
import org.opencv.core.MatOfPoint2f
import org.opencv.core.MatOfPoint3f
import org.opencv.core.Point
import org.opencv.core.Point3
import org.opencv.core.Rect
import org.opencv.core.Scalar
import org.opencv.core.Size
import org.opencv.core.TermCriteria
import org.opencv.features2d.FlannBasedMatcher
import org.opencv.features2d.SIFT
import org.opencv.imgproc.Imgproc
import kotlin.math.abs
import kotlin.math.acos
import kotlin.math.ceil
import kotlin.math.floor
import kotlin.math.hypot
import kotlin.math.max
import kotlin.math.min
import kotlin.math.sqrt

/*
 * A Kotlin port of the iOS app's SiteLocalization.cpp (PR #2715), on OpenCV's Java API. Keep the two in step:
 * every threshold, retry and validation split below matches the C++ so a camera aligns, or refuses to align,
 * the same way on both phones.
 */

object SiteLocalizationLimits {
    /** Each landmark is xyz in the scan's metric world, then 128 SIFT descriptor floats. */
    const val LANDMARK_FLOATS = 131
    const val MAX_LANDMARKS = 12000
    const val MIN_POSE_INLIERS = 12
    const val MIN_ESTIMATED_INLIERS = 8
    const val MIN_LENS_INLIERS = 16
    /** Alignment tolerances are relative to a 640-pixel long image edge; results stay in capture pixels. */
    const val REFERENCE_SIZE = 640
}

/** Screen calibration failure reasons, separate from room-localization reasons. */
object ScreenLensReason {
    const val VIEWS = 1; const val COVERAGE = 3; const val FIT = 4; const val VALIDATION = 5
    const val INTERNAL = 6; const val TILT = 7; const val UNCERTAIN = 8; const val DISAGREEMENT = 9
}

/** WSLocalization. [fittedLens]: 0 = existing measured lens, 1 = measured joint fit, 2 = approximate fit. */
class SiteLocalizationOutput(var reason: Int = 1, var matches: Int = 0) {
    var success = false
    var inliers = 0
    var fittedLens = 0
    var fx = 0.0; var fy = 0.0; var cx = 0.0; var cy = 0.0
    val distortion = DoubleArray(5)
    var rms = 0.0
    /** Column-major 4×4: optical camera (right, down, forward) to model world. */
    val worldFromCamera = DoubleArray(16)
}

private fun imageSize(w: Int, h: Int) = w >= 32 && h >= 32 && w <= 8192 && h <= 8192 && w.toLong() * h <= 32_000_000
private fun finite(values: DoubleArray?, count: Int) = values != null && values.size >= count && (0 until count).all { values[it].isFinite() }
private fun values(m: Mat): DoubleArray = DoubleArray(m.total().toInt() * m.channels()).also { m.get(0, 0, it) }
private fun lens(fx: Double, fy: Double, cx: Double, cy: Double) = Mat(3, 3, CvType.CV_64F).apply { put(0, 0, fx, 0.0, cx, 0.0, fy, cy, 0.0, 0.0, 1.0) }
private fun column(vararg v: Double) = Mat(v.size, 1, CvType.CV_64F).apply { put(0, 0, *v) }
private fun coefficients(d: Mat) = MatOfDouble(*values(d).copyOf(5))
private fun points3(p: List<Point3>) = MatOfPoint3f(*p.toTypedArray())
private fun points2(p: List<Point>) = MatOfPoint2f(*p.toTypedArray())
private fun distance(a: Point3, b: Point3) = sqrt((a.x - b.x) * (a.x - b.x) + (a.y - b.y) * (a.y - b.y) + (a.z - b.z) * (a.z - b.z))
private fun distance(a: Point, b: Point) = hypot(a.x - b.x, a.y - b.y)
private fun rodrigues(r: Mat): DoubleArray = Mat().also { Calib3d.Rodrigues(r, it) }.let(::values)
/** Row-major 3×3 helpers. */
private fun transpose(m: DoubleArray) = DoubleArray(9) { m[(it % 3) * 3 + it / 3] }
private fun multiply(a: DoubleArray, b: DoubleArray) = DoubleArray(9) { i -> (0..2).sumOf { a[(i / 3) * 3 + it] * b[it * 3 + i % 3] } }
private fun apply(m: DoubleArray, v: DoubleArray) = DoubleArray(3) { row -> m[row * 3] * v[0] + m[row * 3 + 1] * v[1] + m[row * 3 + 2] * v[2] }

/** RootSIFT compares normalized gradient histograms using Hellinger distance (Arandjelović and Zisserman, 2012). */
private fun rootDescriptors(rows: List<FloatArray>): Mat {
    val data = FloatArray(rows.size * 128)
    rows.forEachIndexed { i, row ->
        val sum = max(row.sumOf { it.toDouble() }, 1e-12)
        for (j in 0 until 128) data[i * 128 + j] = sqrt(row[j] / sum).toFloat()
    }
    return Mat(rows.size, 128, CvType.CV_32F).apply { put(0, 0, data) }
}
private fun descriptorRows(m: Mat): List<FloatArray> {
    if (m.empty()) return emptyList()
    val all = FloatArray(m.rows() * m.cols()).also { m.get(0, 0, it) }
    return (0 until m.rows()).map { all.copyOfRange(it * m.cols(), it * m.cols() + 128) }
}

private fun nonplanar(p: List<Point3>): Boolean {
    val mx = p.sumOf { it.x } / p.size; val my = p.sumOf { it.y } / p.size; val mz = p.sumOf { it.z } / p.size
    val a = Mat(p.size, 3, CvType.CV_64F).apply { put(0, 0, *p.flatMap { listOf(it.x - mx, it.y - my, it.z - mz) }.toDoubleArray()) }
    val s = Mat(); Core.SVDecomp(a, s, Mat(), Mat(), Core.SVD_NO_UV)
    val v = values(s)
    return v[2] > 0.04 * v[0] && v[2] / sqrt(p.size.toDouble()) > 0.06
}

private fun reprojection(xyz: List<Point3>, uv: List<Point>, k: Mat, d: Mat, r: Mat, t: Mat, maximumError: Double = 8.0): Double {
    val projected = MatOfPoint2f(); Calib3d.projectPoints(points3(xyz), r, t, k, coefficients(d), projected)
    val p = projected.toArray(); var sum = 0.0
    for (i in uv.indices) { val e = distance(p[i], uv[i]); if (!e.isFinite() || e > maximumError) return 1e9; sum += e * e }
    return sqrt(sum / uv.size)
}

/** Area of the convex hull of float points (Andrew's monotone chain), as cv::convexHull + contourArea on Point2f. */
internal fun hullArea(points: List<Point>): Double {
    val sorted = points.distinctBy { it.x to it.y }.sortedWith(compareBy({ it.x }, { it.y }))
    if (sorted.size < 3) return 0.0
    fun cross(o: Point, a: Point, b: Point) = (a.x - o.x) * (b.y - o.y) - (a.y - o.y) * (b.x - o.x)
    val lower = ArrayList<Point>(); for (p in sorted) { while (lower.size >= 2 && cross(lower[lower.size - 2], lower.last(), p) <= 0) lower.removeAt(lower.size - 1); lower += p }
    val upper = ArrayList<Point>(); for (p in sorted.asReversed()) { while (upper.size >= 2 && cross(upper[upper.size - 2], upper.last(), p) <= 0) upper.removeAt(upper.size - 1); upper += p }
    val hull = lower.dropLast(1) + upper.dropLast(1)
    return abs(hull.indices.sumOf { i -> val a = hull[i]; val b = hull[(i + 1) % hull.size]; a.x * b.y - b.x * a.y } / 2)
}

private fun validLens(k: Mat, d: Mat, w: Int, h: Int): Boolean {
    val kv = values(k); val dv = values(d)
    val fx = kv[0]; val fy = kv[4]; val cx = kv[2]; val cy = kv[5]
    if (!Core.checkRange(k) || !Core.checkRange(d) || fx < .3 * w || fx > 5 * w || fy < .3 * h || fy > 5 * w || cx < .15 * w || cx > .85 * w || cy < .15 * h || cy > .85 * h) return false
    if ((0 until 5).any { abs(dv[it]) > 2 }) return false
    for (y in 0..8) for (x in 0..8) {
        val a = (x * w / 8.0 - cx) / fx; val b = (y * h / 8.0 - cy) / fy; val r = a * a + b * b
        val radial = 1 + dv[0] * r + dv[1] * r * r + dv[4] * r * r * r
        val slope = 1 + 3 * dv[0] * r + 5 * dv[1] * r * r + 7 * dv[4] * r * r * r
        if (radial < .3 || slope < .2) return false
    }
    return true
}

/** The scan's landmarks: positions in the model world with their SIFT descriptors (WSMap). */
class SiteLandmarkMap {
    val points = ArrayList<Point3>()
    private val descriptors = ArrayList<FloatArray>()
    private var observations = 0UL
    private val sift by lazy { SIFT.create(800) }
    val count get() = points.size

    private fun voxel(p: Point3): Long {
        fun cell(v: Double) = (floor(v * 50).toLong() and 0x1FFFFF)
        return (cell(p.x) shl 42) or (cell(p.y) shl 21) or cell(p.z)
    }

    /** A bounded reservoir keeps sampling after the map fills; otherwise the first viewpoints take every slot. */
    private fun reservoirIndex(count: ULong): ULong {
        var value = count + 0x9e3779b97f4a7c15UL
        value = (value xor (value shr 30)) * 0xbf58476d1ce4e5b9UL
        value = (value xor (value shr 27)) * 0x94d049bb133111ebUL
        return (value xor (value shr 31)) % count
    }

    /** WSMapAdd: adds one depth-backed reference view. Returns the landmark count, or -1 when the input is invalid. */
    fun add(gray: ByteArray, w: Int, h: Int, depth: FloatArray, confidence: ByteArray, dw: Int, dh: Int, k: DoubleArray, transform: DoubleArray): Int {
        if (gray.size != w * h || !imageSize(w, h) || dw < 8 || dh < 8 || dw > 1024 || dh > 1024 || depth.size != dw * dh || confidence.size != dw * dh ||
            !finite(k, 4) || !finite(transform, 16) || k[0] <= 0 || k[1] <= 0) return -1
        return try {
            val keys = MatOfKeyPoint(); val desc = Mat()
            sift.detectAndCompute(Mat(h, w, CvType.CV_8U).apply { put(0, 0, gray) }, Mat(), keys, desc)
            val rows = descriptorRows(desc); val pts = keys.toArray()
            val seen = HashMap<Long, Int>(); for (v in points) seen.merge(voxel(v), 1, Int::plus)
            for (i in pts.indices) {
                val x = pts[i].pt.x; val y = pts[i].pt.y; val u = (x * dw / w).toInt(); val v = (y * dh / h).toInt()
                if (u < 1 || v < 1 || u >= dw - 1 || v >= dh - 1 || (confidence[v * dw + u].toInt() and 0xFF) < 2) continue
                val z = depth[v * dw + u]; if (!z.isFinite() || z < .25f || z > 6f) continue
                var smooth = true
                for (yy in v - 1..v + 1) for (xx in u - 1..u + 1) { val d = depth[yy * dw + xx]; if (!d.isFinite() || abs(d - z) > .05f) smooth = false }
                if (!smooth) continue
                val cx = (x - k[2]) * z / k[0]; val cy = (y - k[3]) * z / k[1]; val t = transform
                val world = Point3(t[0] * cx + t[4] * cy + t[8] * z + t[12], t[1] * cx + t[5] * cy + t[9] * z + t[13], t[2] * cx + t[6] * cy + t[10] * z + t[14])
                val key = voxel(world)
                if (!world.x.isFinite() || !world.y.isFinite() || !world.z.isFinite() || sqrt(world.dot(world)) > 1000 || (seen[key] ?: 0) > 0) continue
                observations++
                if (points.size < SiteLocalizationLimits.MAX_LANDMARKS) { points += world; descriptors += rows[i] }
                else {
                    val slot = reservoirIndex(observations)
                    if (slot >= SiteLocalizationLimits.MAX_LANDMARKS.toULong()) continue
                    val old = voxel(points[slot.toInt()]); seen[old] = (seen[old] ?: 1) - 1
                    points[slot.toInt()] = world; descriptors[slot.toInt()] = rows[i]
                }
                seen.merge(key, 1, Int::plus)
            }
            points.size
        } catch (_: Exception) { -1 }
    }

    /** WSMapExport: count × 131 floats. */
    fun export(): FloatArray {
        val out = FloatArray(points.size * SiteLocalizationLimits.LANDMARK_FLOATS)
        points.forEachIndexed { i, p ->
            val row = i * SiteLocalizationLimits.LANDMARK_FLOATS
            out[row] = p.x.toFloat(); out[row + 1] = p.y.toFloat(); out[row + 2] = p.z.toFloat()
            descriptors[i].copyInto(out, row + 3)
        }
        return out
    }

    /** WSMapImport: replaces the map only when every landmark is valid. Returns the count, or -1. */
    fun import(data: FloatArray, count: Int): Int {
        if (count < 0 || count > SiteLocalizationLimits.MAX_LANDMARKS || data.size < count * SiteLocalizationLimits.LANDMARK_FLOATS) return -1
        val nextPoints = ArrayList<Point3>(count); val nextDescriptors = ArrayList<FloatArray>(count)
        for (i in 0 until count) {
            val row = i * SiteLocalizationLimits.LANDMARK_FLOATS
            for (j in 0 until SiteLocalizationLimits.LANDMARK_FLOATS) {
                val v = data[row + j]
                if (!v.isFinite() || (if (j < 3) abs(v) > 1000 else v < 0 || v > 512)) return -1
            }
            nextPoints += Point3(data[row].toDouble(), data[row + 1].toDouble(), data[row + 2].toDouble())
            nextDescriptors += data.copyOfRange(row + 3, row + SiteLocalizationLimits.LANDMARK_FLOATS)
        }
        points.clear(); points.addAll(nextPoints); descriptors.clear(); descriptors.addAll(nextDescriptors); observations = count.toULong()
        return count
    }

    internal fun rootDescriptors(): Mat = rootDescriptors(descriptors)
}

object SiteSolver {
    private fun solveCameraFit(xyz: DoubleArray, uv: DoubleArray, n: Int, w: Int, h: Int, known: DoubleArray?, estimated: Boolean, refineFocal: Boolean = false): SiteLocalizationOutput {
        val out = SiteLocalizationOutput(reason = 1, matches = n)
        val pixelScale = max(w, h).toDouble() / SiteLocalizationLimits.REFERENCE_SIZE
        val minimum = if (estimated) SiteLocalizationLimits.MIN_ESTIMATED_INLIERS else if (known != null) SiteLocalizationLimits.MIN_POSE_INLIERS else SiteLocalizationLimits.MIN_LENS_INLIERS
        if (n < minimum || n > 1800 || !imageSize(w, h) || !finite(xyz, n * 3) || !finite(uv, n * 2) || (known != null && !finite(known, 9))) return out
        try {
            val points = ArrayList<Point3>(); val pixels = ArrayList<Point>()
            for (i in 0 until n) {
                if (abs(xyz[i * 3]) > 1000 || abs(xyz[i * 3 + 1]) > 1000 || abs(xyz[i * 3 + 2]) > 1000 || uv[i * 2] < 0 || uv[i * 2] >= w || uv[i * 2 + 1] < 0 || uv[i * 2 + 1] >= h) return out
                // Match the C++ float landmarks and keypoints exactly.
                points += Point3(xyz[i * 3].toFloat().toDouble(), xyz[i * 3 + 1].toFloat().toDouble(), xyz[i * 3 + 2].toFloat().toDouble())
                pixels += Point(uv[i * 2].toFloat().toDouble(), uv[i * 2 + 1].toFloat().toDouble())
            }
            var bestK = Mat(); var bestR = Mat(); var bestT = Mat(); var bestMask = IntArray(0); var best = 0; var bestError = 1e9
            val distortion = Mat.zeros(5, 1, CvType.CV_64F); if (known != null) for (i in 0 until 5) distortion.put(i, 0, known[i + 4])
            val guesses = if (known != null) listOf(known[0]) else listOf(.45 * w, .6 * w, .8 * w, 1.0 * w, 1.4 * w, 2.0 * w)
            val objects = points3(points); val images = points2(pixels)
            for (f in guesses) {
                val k = lens(f, known?.get(1) ?: f, known?.get(2) ?: (w / 2.0), known?.get(3) ?: (h / 2.0))
                val r = Mat(); val t = Mat(); val mask = Mat()
                val ok = Calib3d.solvePnPRansac(objects, images, k, coefficients(distortion), r, t, false, if (estimated) 3000 else 1500,
                    ((if (known != null && !estimated) 3.0 else 6.0) * pixelScale).toFloat(), .999, mask, Calib3d.SOLVEPNP_EPNP)
                if (ok && mask.rows() >= best && mask.rows() > 0) {
                    val index = IntArray(mask.rows()).also { mask.get(0, 0, it) }
                    val error = reprojection(index.map { points[it] }, index.map { pixels[it] }, k, distortion, r, t, 8 * pixelScale)
                    // Sparse sets often give several focal guesses the same consensus count. Choose the better
                    // geometric fit instead of depending on guess order.
                    if (mask.rows() > best || error < bestError) { best = mask.rows(); bestError = error; bestK = k; bestR = r; bestT = t; bestMask = index }
                }
            }
            out.reason = 2; out.inliers = best
            if (best < minimum || best < n * (if (estimated) .25 else .4)) return out
            val consensus = bestMask.map { points[it] }; val allUV = bestMask.map { pixels[it] }
            out.reason = 3
            val box = Imgproc.boundingRect(points2(allUV))
            val poseOnly = known != null || estimated
            if (hullArea(allUV) < (if (poseOnly) .04 else .20) * w * h || box.width < (if (poseOnly) .20 else .55) * w || box.height < (if (poseOnly) .20 else .55) * h) return out
            out.reason = 7; if (!poseOnly && !nonplanar(consensus)) return out
            var k = Mat(); var d = Mat(); var r = Mat(); var t = Mat()
            var validationSum = 0.0; var validationCount = 0
            // Small sets must survive four independent train/validation splits. Every correspondence is withheld
            // once; neither duplicated observations nor a low training residual can stand in for geometric evidence.
            val folds = if (best < 60) 4 else 1
            for (fold in 0 until folds) {
                val train = ArrayList<Point3>(); val hold = ArrayList<Point3>(); val trainUV = ArrayList<Point>(); val holdUV = ArrayList<Point>()
                for (i in 0 until best) if (i % 4 == fold) { hold += consensus[i]; holdUV += allUV[i] } else { train += consensus[i]; trainUV += allUV[i] }
                out.reason = if (refineFocal) 8 else 7; if ((refineFocal || (known == null && !estimated)) && !nonplanar(train)) return out
                val fitK = bestK.clone(); val fitD = distortion.clone(); var fitR = bestR.clone(); var fitT = bestT.clone()
                out.reason = 4
                if ((known != null || estimated) && !refineFocal) Calib3d.solvePnPRefineLM(points3(train), points2(trainUV), fitK, fitD, fitR, fitT)
                else {
                    val rotations = ArrayList<Mat>(); val translations = ArrayList<Mat>(); val sdK = Mat(); val sdPose = Mat(); val viewError = Mat()
                    var flags = Calib3d.CALIB_USE_INTRINSIC_GUESS or Calib3d.CALIB_FIX_ASPECT_RATIO or Calib3d.CALIB_ZERO_TANGENT_DIST or Calib3d.CALIB_FIX_K3
                    if (refineFocal) flags = flags or Calib3d.CALIB_FIX_PRINCIPAL_POINT or Calib3d.CALIB_FIX_K1 or Calib3d.CALIB_FIX_K2
                    Calib3d.calibrateCameraExtended(listOf<Mat>(points3(train)), listOf<Mat>(points2(trainUV)), Size(w.toDouble(), h.toDouble()), fitK, fitD,
                        rotations, translations, sdK, sdPose, viewError, flags, TermCriteria(TermCriteria.COUNT + TermCriteria.EPS, 100, 1e-9))
                    val sd = values(sdK); val fk = values(fitK)
                    if (rotations.isEmpty() || !Core.checkRange(sdK) || sd[1] > fk[4] * (if (estimated) .30 else .08) || sd[2] > w * .05 || sd[3] > h * .05) return out
                    fitR = rotations[0]; fitT = translations[0]
                }
                if (!validLens(fitK, fitD, w, h)) return out
                val limit = (if (estimated) 16 else 8) * pixelScale
                val training = reprojection(train, trainUV, fitK, fitD, fitR, fitT, limit); val validation = reprojection(hold, holdUV, fitK, fitD, fitR, fitT, limit)
                out.reason = 5; out.rms = validation
                if (training > (if (estimated) 5.0 else 2.5) * pixelScale || validation > (if (estimated) 5.0 else 2.5) * pixelScale) return out
                validationSum += validation * validation * hold.size; validationCount += hold.size
                if (fold == 0) { k = fitK; d = fitD; r = fitR; t = fitT }
                else {
                    val firstR = rodrigues(r); val nextR = rodrigues(fitR)
                    val delta = multiply(nextR, transpose(firstR))
                    val angle = acos(((delta[0] + delta[4] + delta[8] - 1) * .5).coerceIn(-1.0, 1.0))
                    val firstPosition = apply(transpose(firstR), values(t)).map { -it }; val nextPosition = apply(transpose(nextR), values(fitT)).map { -it }
                    val moved = sqrt((0..2).sumOf { (firstPosition[it] - nextPosition[it]).let { v -> v * v } })
                    val kv = values(k); val fv = values(fitK)
                    if (angle > Math.toRadians(if (estimated) 10.0 else 3.0) || moved > (if (estimated) .30 else .08) || abs(fv[0] / kv[0] - 1) > (if (estimated) .20 else .05) ||
                        abs(fv[2] - kv[2]) > .03 * w || abs(fv[5] - kv[5]) > .03 * h) return out
                    val edge = listOf(Point(0.0, 0.0), Point(w / 2.0, 0.0), Point(w.toDouble(), 0.0), Point(w.toDouble(), h / 2.0), Point(w.toDouble(), h.toDouble()),
                        Point(w / 2.0, h.toDouble()), Point(0.0, h.toDouble()), Point(0.0, h / 2.0))
                    val firstRays = MatOfPoint2f(); val nextRays = MatOfPoint2f()
                    Calib3d.undistortPoints(points2(edge), firstRays, k, d); Calib3d.undistortPoints(points2(edge), nextRays, fitK, fitD)
                    val a = firstRays.toArray(); val b = nextRays.toArray()
                    for (j in edge.indices) {
                        val dot = a[j].x * b[j].x + a[j].y * b[j].y + 1
                        val cosine = dot / (sqrt(a[j].x * a[j].x + a[j].y * a[j].y + 1) * sqrt(b[j].x * b[j].x + b[j].y * b[j].y + 1))
                        if (!cosine.isFinite() || acos(cosine.coerceIn(-1.0, 1.0)) > Math.toRadians(if (estimated) 10.0 else 3.0)) return out
                    }
                }
            }
            out.rms = sqrt(validationSum / validationCount)
            out.fittedLens = if (known != null) 0 else if (estimated) 2 else 1
            val rotation = rodrigues(r); val worldR = transpose(rotation); val tv = values(t)
            val position = apply(worldR, tv).map { -it }
            for (p in consensus) { val q = apply(rotation, doubleArrayOf(p.x, p.y, p.z)); if (q[2] + tv[2] <= .1) return out }
            if (sqrt(position.sumOf { it * it }) > 1000) return out
            for (col in 0..2) for (row in 0..2) out.worldFromCamera[col * 4 + row] = worldR[row * 3 + col]
            for (row in 0..2) out.worldFromCamera[12 + row] = position[row]; out.worldFromCamera[15] = 1.0
            val kv = values(k); out.fx = kv[0]; out.fy = kv[4]; out.cx = kv[2]; out.cy = kv[5]
            values(d).copyInto(out.distortion, 0, 0, 5); out.success = true; out.reason = 0
            return out
        } catch (_: Exception) { out.reason = 6; return out }
    }

    fun solveCamera(xyz: DoubleArray, uv: DoubleArray, n: Int, w: Int, h: Int, known: DoubleArray?, estimated: Boolean): SiteLocalizationOutput {
        if (!estimated || known != null) return solveCameraFit(xyz, uv, n, w, h, known, estimated)
        return try {
            // Validate every assumed focal length independently. Choosing a RANSAC winner first can discard a
            // valid solution, or hide focal/depth ambiguity.
            var best = SiteLocalizationOutput(reason = 1, matches = n)
            val candidates = ArrayList<SiteLocalizationOutput>()
            for (factor in listOf(.35, .45, .6, .8, 1.0, 1.4, 2.0)) {
                val assumed = doubleArrayOf(factor * w, factor * w, w / 2.0, h / 2.0, 0.0, 0.0, 0.0, 0.0, 0.0)
                val fit = solveCameraFit(xyz, uv, n, w, h, assumed, true)
                if (fit.success) candidates += fit
                else if (fit.inliers > best.inliers || (fit.inliers == best.inliers && fit.reason > best.reason)) best = fit
                // With real depth variation, solve the focal length continuously rather than quantizing distance to
                // the seed grid. The principal point and distortion stay explicitly assumed.
                val refined = solveCameraFit(xyz, uv, n, w, h, assumed, true, true)
                if (refined.success) candidates += refined
            }
            if (candidates.isEmpty()) return best
            best = candidates.first()
            for (fit in candidates) if (fit.inliers > best.inliers || (fit.inliers == best.inliers && fit.rms < best.rms)) best = fit
            for (fit in candidates) {
                if (fit.inliers < ceil(best.inliers * .9) || fit.rms > max(best.rms * 1.5, best.rms + .5 * max(w, h).toDouble() / SiteLocalizationLimits.REFERENCE_SIZE)) continue
                var squared = 0.0; var trace = 0.0
                for (row in 0..2) {
                    val delta = fit.worldFromCamera[12 + row] - best.worldFromCamera[12 + row]; squared += delta * delta
                    for (col in 0..2) trace += fit.worldFromCamera[col * 4 + row] * best.worldFromCamera[col * 4 + row]
                }
                val angle = acos(((trace - 1) * .5).coerceIn(-1.0, 1.0))
                if (sqrt(squared) > .30 || angle > Math.toRadians(10.0)) { best.success = false; best.reason = 8; return best }
            }
            best.fittedLens = 2
            best
        } catch (_: Exception) { SiteLocalizationOutput(reason = 6, matches = n) }
    }

    /**
     * WSLocalize (known measured lens or joint fit), WSLocalizeWithAssumedLens (known lens, [estimated]) and
     * WSLocalizeEstimated (no lens, [estimated]).
     */
    fun localize(map: SiteLandmarkMap, gray: ByteArray, w: Int, h: Int, known: DoubleArray?, estimated: Boolean): SiteLocalizationOutput {
        val out = SiteLocalizationOutput(reason = 1)
        if (gray.size != w * h || !imageSize(w, h)) return out
        try {
            if (map.count < 120) return out
            var full = Mat(h, w, CvType.CV_8U).apply { put(0, 0, gray) }
            var mask = Mat(); var k = Mat(); var d = Mat()
            if (known != null) {
                if (!finite(known, 9)) return out
                k = lens(known[0], known[1], known[2], known[3]); d = column(known[4], known[5], known[6], known[7], known[8])
                if (!validLens(k, d, w, h)) return out
                // Match undistorted feature patches to the phone's room references. Using distortion only during
                // PnP is too late if warped patches did not match.
                val rectified = Mat(); Calib3d.undistort(full, rectified, k, d, k); full = rectified
                val valid = Mat(h, w, CvType.CV_8U, Scalar(255.0)); Calib3d.undistort(valid, mask, k, d, k)
                Imgproc.threshold(mask, mask, 254.0, 255.0, Imgproc.THRESH_BINARY)
                Imgproc.erode(mask, mask, Mat(), Point(-1.0, -1.0), 4)
            }
            val scale = min(1.0, 960.0 / max(w, h)); val image = Mat(); Imgproc.resize(full, image, Size(), scale, scale, Imgproc.INTER_AREA)
            if (!mask.empty()) Imgproc.resize(mask, mask, image.size(), 0.0, 0.0, Imgproc.INTER_NEAREST)
            val keys = MatOfKeyPoint(); val descriptors = Mat()
            SIFT.create(1800).detectAndCompute(image, mask, keys, descriptors)
            val keypoints = keys.toArray()
            val minimum = if (estimated) SiteLocalizationLimits.MIN_ESTIMATED_INLIERS else if (known != null) SiteLocalizationLimits.MIN_POSE_INLIERS else SiteLocalizationLimits.MIN_LENS_INLIERS
            if (keypoints.size < minimum) return out
            val matches = ArrayList<MatOfDMatch>()
            FlannBasedMatcher.create().knnMatch(rootDescriptors(descriptorRows(descriptors)), map.rootDescriptors(), matches, 8)
            val candidates = ArrayList<org.opencv.core.DMatch>()
            for (list in matches) {
                val pair = list.toArray(); if (pair.size < 2) continue
                // Repeat observations of one physical feature must not be its own ambiguity competitor. Compare
                // against a spatially distinct alternative.
                var competitor = 1
                while (competitor < pair.size && distance(map.points[pair[competitor].trainIdx], map.points[pair[0].trainIdx]) < .08) competitor++
                if (competitor < pair.size && pair[0].distance < .75f * pair[competitor].distance) candidates += pair[0]
            }
            candidates.sortBy { it.distance }
            val xyz = ArrayList<Double>(); val uv = ArrayList<Double>(); val distinctPoints = ArrayList<Point3>(); val distinctPixels = ArrayList<Point>()
            for (match in candidates) {
                val point = map.points[match.trainIdx]; val key = keypoints[match.queryIdx].pt
                val pixel = Point((key.x / scale.toFloat()).toFloat().toDouble(), (key.y / scale.toFloat()).toFloat().toDouble())
                if (distinctPoints.indices.any { distance(point, distinctPoints[it]) < .03 || distance(pixel, distinctPixels[it]) < 2 }) continue
                distinctPoints += point; distinctPixels += pixel
                xyz += listOf(point.x, point.y, point.z); uv += listOf(pixel.x, pixel.y)
            }
            var xyzArray = xyz.toDoubleArray(); var uvArray = uv.toDoubleArray()
            if (known != null && uvArray.isNotEmpty()) {
                // PnP and its held-out residuals use the original camera model. Convert the rectified keypoints back
                // to raw pixels, applying lens distortion once.
                val rays = (0 until uvArray.size / 2).map { Point3((uvArray[it * 2] - known[2]) / known[0], (uvArray[it * 2 + 1] - known[3]) / known[1], 1.0) }
                val raw = MatOfPoint2f(); Calib3d.projectPoints(points3(rays), column(0.0, 0.0, 0.0), column(0.0, 0.0, 0.0), k, coefficients(d), raw)
                val keptXYZ = ArrayList<Double>(); val keptUV = ArrayList<Double>()
                raw.toArray().forEachIndexed { i, p ->
                    if (p.x >= 0 && p.x < w && p.y >= 0 && p.y < h) { keptXYZ += listOf(xyzArray[i * 3], xyzArray[i * 3 + 1], xyzArray[i * 3 + 2]); keptUV += listOf(p.x, p.y) }
                }
                xyzArray = keptXYZ.toDoubleArray(); uvArray = keptUV.toDoubleArray()
            }
            return solveCamera(xyzArray, uvArray, uvArray.size / 2, w, h, known, estimated)
        } catch (_: Exception) { out.reason = 6; return out }
    }
}

/** Progress of a phone-screen checkerboard calibration (WSScreenLensProgress). */
data class ScreenLensProgress(val count: Int = 0, val detection: Int = 0, val revision: Long = 0, val x: Double = 0.0, val y: Double = 0.0, val width: Double = 0.0, val height: Double = 0.0,
                              val coverage: Double = 0.0, val minX: Double = 0.0, val maxX: Double = 0.0, val minY: Double = 0.0, val maxY: Double = 0.0, val coverageReady: Boolean = false)

/** Measures a fixed camera's lens from views of the 6×9 inner-corner checkerboard (WSScreenLens). */
class ScreenLensCalibrator {
    private var width = 0; private var height = 0; private var detection = 0; private var revision = 0L
    private var lastBox = Rect()
    private val views = ArrayList<List<Point>>()
    private val pattern = (0 until 9).flatMap { y -> (0 until 6).map { x -> Point3(x.toDouble(), y.toDouble(), 0.0) } }

    /** Returns the number of views kept, -2 when the resolution changed, or -1 for an invalid image. */
    @Synchronized fun add(gray: ByteArray, w: Int, h: Int): Int {
        if (gray.size != w * h || !imageSize(w, h)) return -1
        return try {
            if (width != 0 && (width != w || height != h)) return -2
            width = w; height = h; detection = 0; lastBox = Rect()
            val found = Mat()
            if (!Calib3d.findChessboardCornersSB(Mat(h, w, CvType.CV_8U).apply { put(0, 0, gray) }, Size(6.0, 9.0), found, Calib3d.CALIB_CB_NORMALIZE_IMAGE)) return views.size
            val corners = MatOfPoint2f(found).toArray().map { Point(it.x, it.y) }
            if (corners.size != 54) return views.size
            val box = Imgproc.boundingRect(MatOfPoint2f(*corners.toTypedArray()))
            lastBox = box
            if (box.width < 60 || box.height < 60 || box.area() < .025 * w * h) { detection = 1; return views.size }
            if (box.x < 4 || box.y < 4 || box.x + box.width > w - 4 || box.y + box.height > h - 4) { detection = 2; return views.size }
            for (prior in views) {
                var sum = 0.0; var reversed = 0.0
                for (j in corners.indices) { val a = distance(corners[j], prior[j]); val b = distance(corners[j], prior[corners.size - 1 - j]); sum += a * a; reversed += b * b }
                val ca = Point(corners.sumOf { it.x } / corners.size, corners.sumOf { it.y } / corners.size); val cb = Point(prior.sumOf { it.x } / prior.size, prior.sumOf { it.y } / prior.size)
                val radiusA = corners.sumOf { distance(it, ca) } / corners.size; val radiusB = prior.sumOf { distance(it, cb) } / prior.size
                var shape = 0.0
                for (j in corners.indices) {
                    val k = if (sum <= reversed) j else corners.size - 1 - j
                    val dx = (corners[j].x - ca.x) / radiusA - (prior[k].x - cb.x) / radiusB; val dy = (corners[j].y - ca.y) / radiusA - (prior[k].y - cb.y) / radiusB
                    shape += dx * dx + dy * dy
                }
                if (sqrt(min(sum, reversed) / corners.size) < .04 * hypot(w.toDouble(), h.toDouble()) && sqrt(shape / corners.size) < .04) { detection = 3; return views.size }
            }
            if (views.size >= 30) views.removeAt(0)
            views += corners; detection = 4; revision++
            views.size
        } catch (_: Exception) { -1 }
    }

    @Synchronized fun progress(): ScreenLensProgress {
        if (width == 0 || height == 0) return ScreenLensProgress(views.size, detection, revision)
        val box = ScreenLensProgress(views.size, detection, revision, lastBox.x.toDouble() / width, lastBox.y.toDouble() / height, lastBox.width.toDouble() / width, lastBox.height.toDouble() / height)
        if (views.isEmpty()) return box
        return try {
            val all = views.flatten(); val centres = views.map { v -> Point(v.sumOf { it.x } / v.size, v.sumOf { it.y } / v.size) }
            val hull = hullArea(all); val b = Imgproc.boundingRect(MatOfPoint2f(*centres.toTypedArray()))
            val coverage = hull / (width.toDouble() * height)
            box.copy(coverage = coverage, minX = b.x.toDouble() / width, maxX = (b.x + b.width).toDouble() / width, minY = b.y.toDouble() / height, maxY = (b.y + b.height).toDouble() / height,
                coverageReady = coverage >= .4 && b.width >= .3 * width && b.height >= .3 * height)
        } catch (_: Exception) { box }
    }

    /** WSScreenLensSolve. On success the output carries the lens; [SiteLocalizationOutput.reason] is a [ScreenLensReason]. */
    @Synchronized fun solve(): SiteLocalizationOutput {
        val out = SiteLocalizationOutput(reason = ScreenLensReason.VIEWS)
        return try {
            out.matches = views.size; if (views.size < 12) return out
            val all = views.flatten(); val centres = views.map { v -> Point(v.sumOf { it.x } / v.size, v.sumOf { it.y } / v.size) }
            val bounds = Imgproc.boundingRect(MatOfPoint2f(*centres.toTypedArray()))
            out.reason = ScreenLensReason.COVERAGE
            if (hullArea(all) < .4 * width * height || bounds.width < .3 * width || bounds.height < .3 * height) return out
            var firstK = Mat(); var firstD = Mat(); var totalError = 0.0; var tested = 0
            for (fold in 0 until 4) {
                val images = ArrayList<Mat>(); val objects = ArrayList<Mat>()
                views.forEachIndexed { i, v -> if (i % 4 != fold) { images += MatOfPoint2f(*v.toTypedArray()); objects += points3(pattern) } }
                val k = lens(width.toDouble(), width.toDouble(), width / 2.0, height / 2.0); val d = Mat.zeros(5, 1, CvType.CV_64F)
                val sdK = Mat(); val sdPose = Mat(); val errors = Mat(); val rotations = ArrayList<Mat>(); val translations = ArrayList<Mat>()
                // QR avoids repeated SVD solves of the large per-view parameter system. Keep the uncertainty,
                // independent-view and withheld-point checks below.
                val flags = Calib3d.CALIB_USE_INTRINSIC_GUESS or Calib3d.CALIB_FIX_ASPECT_RATIO or Calib3d.CALIB_ZERO_TANGENT_DIST or Calib3d.CALIB_FIX_K3 or Calib3d.CALIB_USE_QR
                out.reason = ScreenLensReason.FIT
                val rms = Calib3d.calibrateCameraExtended(objects, images, Size(width.toDouble(), height.toDouble()), k, d, rotations, translations, sdK, sdPose, errors, flags)
                out.rms = rms
                if (!rms.isFinite() || rms > 1.5 || !validLens(k, d, width, height)) return out
                // Require independently tilted views; moving a front-facing screen alone leaves focal length and
                // distance ambiguous.
                var tiltSpread = 0.0
                val normals = rotations.map { rodrigues(it).let { m -> doubleArrayOf(m[2], m[5], m[8]) } }
                for (a in normals.indices) for (b in 0 until a) tiltSpread = max(tiltSpread, sqrt((0..2).sumOf { (normals[a][it] - normals[b][it]).let { v -> v * v } }))
                out.reason = ScreenLensReason.TILT; if (tiltSpread < .35) return out
                out.reason = ScreenLensReason.UNCERTAIN
                val sd = values(sdK); val kv = values(k)
                if (!Core.checkRange(sdK) || sd[1] > kv[4] * .05 || sd[2] > width * .03 || sd[3] > height * .03) return out
                out.reason = ScreenLensReason.DISAGREEMENT
                if (fold == 0) { firstK = k; firstD = d }
                else { val fv = values(firstK); if (abs(kv[0] / fv[0] - 1) > .05 || abs(kv[2] - fv[2]) > .03 * width || abs(kv[5] - fv[5]) > .03 * height) return out }
                views.forEachIndexed { i, view ->
                    if (i % 4 != fold) return@forEachIndexed
                    val train = ArrayList<Point3>(); val hold = ArrayList<Point3>(); val trainUV = ArrayList<Point>(); val holdUV = ArrayList<Point>()
                    for (j in pattern.indices) if (j % 4 == 0) { hold += pattern[j]; holdUV += view[j] } else { train += pattern[j]; trainUV += view[j] }
                    val r = Mat(); val t = Mat(); out.reason = ScreenLensReason.VALIDATION
                    if (!Calib3d.solvePnP(points3(train), points2(trainUV), k, coefficients(d), r, t, false, Calib3d.SOLVEPNP_ITERATIVE)) return out
                    val error = reprojection(hold, holdUV, k, d, r, t, 4.0); if (error > 1.5) return out
                    totalError += error * error; tested++
                }
            }
            val kv = values(firstK); out.fx = kv[0]; out.fy = kv[4]; out.cx = kv[2]; out.cy = kv[5]
            values(firstD).copyInto(out.distortion, 0, 0, 5)
            out.rms = sqrt(totalError / tested); out.inliers = views.size * 54; out.fittedLens = 1; out.reason = 0; out.success = true
            out
        } catch (_: Exception) { out.reason = ScreenLensReason.INTERNAL; out }
    }
}
