package dev.wendy.android

import androidx.test.ext.junit.runners.AndroidJUnit4
import org.junit.Assert.*
import org.junit.BeforeClass
import org.junit.Test
import org.junit.runner.RunWith
import org.opencv.android.OpenCVLoader
import org.opencv.core.Core
import org.opencv.core.CvType
import org.opencv.core.Mat
import org.opencv.core.Point
import org.opencv.core.Scalar
import org.opencv.core.Size
import org.opencv.imgproc.Imgproc
import kotlin.math.abs
import kotlin.math.acos
import kotlin.math.cos
import kotlin.math.sin
import kotlin.math.sqrt
import kotlin.random.Random

/**
 * Runs the ported site solver on synthetic scenes whose answer is known: a camera, a phone-screen checkerboard
 * and a textured room. Nothing here needs hardware; everything runs the real OpenCV code on the device.
 */
@RunWith(AndroidJUnit4::class)
class SiteLocalizationTest {
    companion object {
        @BeforeClass @JvmStatic fun load() { assertTrue(OpenCVLoader.initLocal()) }
    }

    private class Camera(val position: DoubleArray, target: DoubleArray, val fx: Double, val w: Int, val h: Int) {
        val cx = w / 2.0; val cy = h / 2.0
        val forward = normalize(sub(target, position))
        val right = normalize(cross(forward, doubleArrayOf(0.0, 1.0, 0.0)))
        val down = cross(forward, right)
        /** Optical (right, down, forward) to world, column-major 4×4. */
        fun opticalToWorld() = doubleArrayOf(right[0], right[1], right[2], 0.0, down[0], down[1], down[2], 0.0, forward[0], forward[1], forward[2], 0.0, position[0], position[1], position[2], 1.0)
        fun optical(p: DoubleArray): DoubleArray { val d = sub(p, position); return doubleArrayOf(dot(d, right), dot(d, down), dot(d, forward)) }
        fun project(p: DoubleArray): DoubleArray? { val c = optical(p); if (c[2] <= .05) return null; return doubleArrayOf(fx * c[0] / c[2] + cx, fx * c[1] / c[2] + cy) }
        fun ray(x: Double, y: Double): DoubleArray { val o = doubleArrayOf((x - cx) / fx, (y - cy) / fx, 1.0); return doubleArrayOf(right[0] * o[0] + down[0] * o[1] + forward[0], right[1] * o[0] + down[1] * o[1] + forward[1], right[2] * o[0] + down[2] * o[1] + forward[2]) }
        val known get() = doubleArrayOf(fx, fx, cx, cy, 0.0, 0.0, 0.0, 0.0, 0.0)
    }

    /** A textured rectangle: [origin] + a·[u] + b·[v], a ∈ [0, lu], b ∈ [0, lv], with unit axes. */
    private class Plane(val origin: DoubleArray, val u: DoubleArray, val v: DoubleArray, val lu: Double, val lv: Double, seed: Int) {
        val texture: Mat = Mat((lv * 200).toInt(), (lu * 200).toInt(), CvType.CV_8U, Scalar(180.0)).also { m ->
            val random = Random(seed)
            repeat(900) {
                val x = random.nextInt(m.cols()).toDouble(); val y = random.nextInt(m.rows()).toDouble(); val shade = Scalar(random.nextInt(0, 255).toDouble())
                when (it % 3) {
                    0 -> Imgproc.rectangle(m, Point(x, y), Point(x + random.nextInt(6, 40), y + random.nextInt(6, 40)), shade, -1)
                    1 -> Imgproc.circle(m, Point(x, y), random.nextInt(4, 22), shade, random.nextInt(2, 5))
                    else -> Imgproc.line(m, Point(x, y), Point(x + random.nextInt(-40, 40), y + random.nextInt(-40, 40)), shade, 3)
                }
            }
        }
        val normal = cross(u, v)
        fun at(a: Double, b: Double) = DoubleArray(3) { origin[it] + u[it] * a + v[it] * b }
        /** Distance along a world ray, or null when it misses the rectangle. */
        fun hit(from: DoubleArray, direction: DoubleArray): Double? {
            val denominator = dot(normal, direction); if (abs(denominator) < 1e-9) return null
            val t = dot(normal, sub(origin, from)) / denominator; if (t <= 0) return null
            val p = DoubleArray(3) { from[it] + direction[it] * t }; val a = dot(sub(p, origin), u); val b = dot(sub(p, origin), v)
            return if (a in 0.0..lu && b in 0.0..lv) t else null
        }
    }

    private val room = listOf(
        Plane(doubleArrayOf(-2.5, 0.0, 5.0), doubleArrayOf(1.0, 0.0, 0.0), doubleArrayOf(0.0, 1.0, 0.0), 5.0, 3.0, 1),   // back wall
        Plane(doubleArrayOf(-2.5, 0.0, 1.0), doubleArrayOf(0.0, 0.0, 1.0), doubleArrayOf(0.0, 1.0, 0.0), 4.0, 3.0, 2),   // left wall
        Plane(doubleArrayOf(2.5, 0.0, 1.0), doubleArrayOf(0.0, 0.0, 1.0), doubleArrayOf(0.0, 1.0, 0.0), 4.0, 3.0, 3),    // right wall
        Plane(doubleArrayOf(-2.5, 0.0, 1.0), doubleArrayOf(1.0, 0.0, 0.0), doubleArrayOf(0.0, 0.0, 1.0), 5.0, 4.0, 4),   // floor
    )

    /** Renders the room; walls only meet at edges, so painting needs no depth test from inside the room. */
    private fun render(camera: Camera): ByteArray {
        val frame = Mat(camera.h, camera.w, CvType.CV_8U, Scalar(90.0))
        for (plane in room) {
            val sx = plane.lu / plane.texture.cols(); val sy = plane.lv / plane.texture.rows()
            // Texture pixel → world → image, as a homography K · Rcw · [u·sx, v·sy, origin − C].
            val columns = listOf(DoubleArray(3) { plane.u[it] * sx }, DoubleArray(3) { plane.v[it] * sy }, sub(plane.origin, camera.position))
            val optical = columns.map { c -> doubleArrayOf(dot(c, camera.right), dot(c, camera.down), dot(c, camera.forward)) }
            val k = doubleArrayOf(camera.fx, 0.0, camera.cx, 0.0, camera.fx, camera.cy, 0.0, 0.0, 1.0)
            val homography = Mat(3, 3, CvType.CV_64F)
            for (row in 0..2) for (col in 0..2) homography.put(row, col, (0..2).sumOf { k[row * 3 + it] * optical[col][it] })
            val warped = Mat(); val mask = Mat()
            Imgproc.warpPerspective(plane.texture, warped, homography, Size(camera.w.toDouble(), camera.h.toDouble()), Imgproc.INTER_LINEAR)
            Imgproc.warpPerspective(Mat(plane.texture.size(), CvType.CV_8U, Scalar(255.0)), mask, homography, Size(camera.w.toDouble(), camera.h.toDouble()), Imgproc.INTER_NEAREST)
            warped.copyTo(frame, mask)
        }
        return ByteArray(camera.w * camera.h).also { frame.get(0, 0, it) }
    }

    private fun depth(camera: Camera, dw: Int, dh: Int) = FloatArray(dw * dh) { i ->
        val ray = camera.ray((i % dw + .5) * camera.w / dw, (i / dw + .5) * camera.h / dh)
        (room.mapNotNull { it.hit(camera.position, ray) }.minOrNull() ?: Double.NaN).toFloat() // ray has unit optical z, so t is z-depth
    }

    private fun map(vararg views: Camera): SiteLandmarkMap {
        val map = SiteLandmarkMap()
        for (view in views) {
            val dw = 192; val dh = 144
            assertTrue(map.add(render(view), view.w, view.h, depth(view, dw, dh), ByteArray(dw * dh) { 2 }, dw, dh,
                doubleArrayOf(view.fx, view.fx, view.cx, view.cy), view.opticalToWorld()) > 0)
        }
        return map
    }

    private fun assertPose(truth: Camera, out: SiteLocalizationOutput, metres: Double, degrees: Double) {
        assertTrue("reason ${out.reason}, ${out.matches} matches, ${out.inliers} inliers", out.success)
        val t = out.worldFromCamera
        val moved = sqrt((0..2).sumOf { (t[12 + it] - truth.position[it]).let { d -> d * d } })
        assertTrue("moved $moved m", moved < metres)
        // Optical forward is the third column.
        val angle = Math.toDegrees(acos(dot(doubleArrayOf(t[8], t[9], t[10]), truth.forward).coerceIn(-1.0, 1.0)))
        assertTrue("turned $angle°", angle < degrees)
    }

    @Test fun solvesAKnownCameraFromCorrespondences() {
        val truth = Camera(doubleArrayOf(0.4, 1.6, -0.3), doubleArrayOf(0.0, 1.0, 4.0), 820.0, 1280, 960)
        val random = Random(11)
        val xyz = ArrayList<Double>(); val uv = ArrayList<Double>()
        repeat(240) {
            // Pixels spread over the image at varied depth, so the points are far from planar.
            val x = random.nextDouble(20.0, 1260.0); val y = random.nextDouble(20.0, 940.0); val z = random.nextDouble(2.0, 5.5)
            val ray = truth.ray(x, y); val p = DoubleArray(3) { truth.position[it] + ray[it] * z }
            xyz += p.toList(); uv += listOf(x + random.nextDouble(-.3, .3), y + random.nextDouble(-.3, .3))
        }
        val points = xyz.toDoubleArray(); val pixels = uv.toDoubleArray()
        val measured = SiteSolver.solveCamera(points, pixels, 240, 1280, 960, truth.known, false)
        assertPose(truth, measured, .01, .5); assertEquals(0, measured.fittedLens)
        // Joint lens calibration recovers the focal length.
        val joint = SiteSolver.solveCamera(points, pixels, 240, 1280, 960, null, false)
        assertPose(truth, joint, .03, 1.0); assertEquals(1, joint.fittedLens)
        assertEquals(820.0, joint.fx, 820 * .02)
        // An estimated lens gives an approximate pose.
        val estimated = SiteSolver.solveCamera(points, pixels, 240, 1280, 960, null, true)
        assertPose(truth, estimated, .30, 10.0); assertEquals(2, estimated.fittedLens)
        // Points on one wall (world z = 4) cannot measure a lens.
        val flatXYZ = ArrayList<Double>(); val flatUV = ArrayList<Double>()
        repeat(240) {
            val x = random.nextDouble(20.0, 1260.0); val y = random.nextDouble(20.0, 940.0); val ray = truth.ray(x, y)
            val t = (4.0 - truth.position[2]) / ray[2]
            flatXYZ += (0..2).map { truth.position[it] + ray[it] * t }; flatUV += listOf(x, y)
        }
        val planar = SiteSolver.solveCamera(flatXYZ.toDoubleArray(), flatUV.toDoubleArray(), 240, 1280, 960, null, false)
        assertFalse(planar.success); assertEquals(7, planar.reason)
        // The same wall still gives a pose when the lens is known.
        assertPose(truth, SiteSolver.solveCamera(flatXYZ.toDoubleArray(), flatUV.toDoubleArray(), 240, 1280, 960, truth.known, false), .02, .5)
    }

    @Test fun localizesALiveViewAgainstARoomMap() {
        val map = map(Camera(doubleArrayOf(0.3, 1.5, 0.2), doubleArrayOf(0.0, 1.2, 5.0), 700.0, 960, 720),
            Camera(doubleArrayOf(-0.4, 1.4, 0.5), doubleArrayOf(0.5, 1.0, 5.0), 700.0, 960, 720))
        assertTrue("${map.count} landmarks", map.count >= 120)
        val live = Camera(doubleArrayOf(0.1, 1.6, 0.3), doubleArrayOf(-0.3, 1.1, 5.0), 700.0, 960, 720)
        val frame = render(live)
        assertPose(live, SiteSolver.localize(map, frame, 960, 720, live.known, false), .08, 3.0)
        val joint = SiteSolver.localize(map, frame, 960, 720, null, false)
        assertPose(live, joint, .15, 3.0); assertEquals(700.0, joint.fx, 700 * .05)
        // A featureless frame finds nothing.
        assertFalse(SiteSolver.localize(map, ByteArray(960 * 720) { 90 }, 960, 720, live.known, false).success)
    }

    @Test fun theEngineAlignsFromASavedVisualMap() {
        val reference = map(Camera(doubleArrayOf(0.3, 1.5, 0.2), doubleArrayOf(0.0, 1.2, 5.0), 700.0, 960, 720),
            Camera(doubleArrayOf(-0.4, 1.4, 0.5), doubleArrayOf(0.5, 1.0, 5.0), 700.0, 960, 720))
        val model = "usdz".toByteArray(); val revision = Site(name = "Room").modelRevision
        val bytes = java.nio.ByteBuffer.allocate(reference.count * 131 * 4).order(java.nio.ByteOrder.LITTLE_ENDIAN).apply { reference.export().forEach { putFloat(it) } }.array()
        // Saved and read back in the iOS file format.
        val saved = SiteVisualMap(1, revision, SiteVisualMap.digest(model), 784_000_000.0, 2, bytes)
        val engine = SiteLocalizationEngine().apply { load(SiteVisualMap.parse(saved.json().toString().toByteArray()), revision, model) }
        val live = Camera(doubleArrayOf(0.1, 1.6, 0.3), doubleArrayOf(-0.3, 1.1, 5.0), 700.0, 960, 720)
        val result = engine.localize(render(live), 960, 720, revision, null)
        assertTrue(result.lens.measured); assertTrue(result.fittedLens)
        val p = result.pose.position
        assertTrue(sqrt((p.x - live.position[0]).let { it * it } + (p.y - live.position[1]).let { it * it } + (p.z - live.position[2]).let { it * it }) < .15)
        val m = result.pose.matrix()
        assertTrue(Math.toDegrees(acos(dot(doubleArrayOf(m[6], m[7], m[8]), live.forward).coerceIn(-1.0, 1.0))) < 3)
        // It passes the checks that guard saving on the gateway.
        SiteCalibrationRecords.alignment("usb-1", result)
        // With that lens known, a second frame verifies the position.
        val verification = engine.localize(render(live), 960, 720, revision, result.lens)
        assertFalse(verification.fittedLens)
        // A map for another model revision is refused.
        assertThrows(SiteLocalizationException::class.java) { SiteLocalizationEngine().load(saved, Site(name = "Other").modelRevision, model) }
    }

    @Test fun landmarkMapsRoundTripAndRejectDamage() {
        val map = map(Camera(doubleArrayOf(0.3, 1.5, 0.2), doubleArrayOf(0.0, 1.2, 5.0), 700.0, 960, 720))
        val exported = map.export()
        assertEquals(map.count * 131, exported.size)
        val copy = SiteLandmarkMap(); assertEquals(map.count, copy.import(exported, map.count))
        assertArrayEquals(exported, copy.export(), 0f)
        assertEquals(-1, SiteLandmarkMap().import(exported.copyOf().also { it[0] = Float.NaN }, map.count))
        assertEquals(-1, SiteLandmarkMap().import(exported.copyOf().also { it[5] = 600f }, map.count))
    }

    @Test fun measuresALensFromPhoneScreenViews() {
        // The phone pattern: white border, 7×10 squares, 6×9 inner corners (iOS RobotCalibrationPattern).
        val pattern = Mat(1200, 900, CvType.CV_8U, Scalar(255.0))
        for (row in 0 until 10) for (column in 0 until 7) if ((row + column) % 2 == 0)
            Imgproc.rectangle(pattern, Point((column + 1) * 100.0, (row + 1) * 100.0), Point((column + 2) * 100.0 - 1, (row + 2) * 100.0 - 1), Scalar(0.0), -1)
        val w = 1280; val h = 960; val f = 900.0; val square = .024 // metres per square: a 21.6 cm × 28.8 cm screen held about 45 cm away
        val calibration = ScreenLensCalibrator(); val outcomes = ArrayList<Int>()
        val placements = listOf(
            Triple(-.18, -.08, .45), Triple(.18, -.08, .45), Triple(-.18, .08, .45), Triple(.18, .08, .45), Triple(0.0, 0.0, .42),
            Triple(-.10, 0.0, .55), Triple(.10, 0.0, .55), Triple(0.0, -.08, .50), Triple(0.0, .08, .50), Triple(-.2, 0.0, .55),
            Triple(.2, 0.0, .55), Triple(0.0, 0.0, .6), Triple(-.12, -.08, .5), Triple(.12, .08, .5), Triple(.12, -.08, .5), Triple(-.12, .08, .5))
        placements.forEachIndexed { i, (x, y, z) ->
            // Tilt alternately about the vertical and horizontal axes, both ways.
            val tilt = Math.toRadians(listOf(28.0, -28.0, 0.0, 0.0, 18.0, -18.0)[i % 6]); val nod = Math.toRadians(listOf(0.0, 0.0, 28.0, -28.0, 18.0, -18.0)[i % 6])
            val ry = doubleArrayOf(cos(tilt), 0.0, sin(tilt), 0.0, 1.0, 0.0, -sin(tilt), 0.0, cos(tilt))
            val rx = doubleArrayOf(1.0, 0.0, 0.0, 0.0, cos(nod), -sin(nod), 0.0, sin(nod), cos(nod))
            val r = DoubleArray(9) { k -> (0..2).sumOf { ry[(k / 3) * 3 + it] * rx[it * 3 + k % 3] } }
            // Pattern pixel → plane metres (centred) → camera, as H = K [r1 r2 t] A.
            val a = doubleArrayOf(square / 100, 0.0, -4.5 * square, 0.0, square / 100, -6.0 * square, 0.0, 0.0, 1.0)
            val rt = doubleArrayOf(r[0], r[1], x, r[3], r[4], y, r[6], r[7], z)
            val k = doubleArrayOf(f, 0.0, w / 2.0, 0.0, f, h / 2.0, 0.0, 0.0, 1.0)
            fun mul(p: DoubleArray, q: DoubleArray) = DoubleArray(9) { m -> (0..2).sumOf { p[(m / 3) * 3 + it] * q[it * 3 + m % 3] } }
            val homography = mul(mul(k, rt), a)
            val frame = Mat()
            Imgproc.warpPerspective(pattern, frame, Mat(3, 3, CvType.CV_64F).apply { put(0, 0, *homography) }, Size(w.toDouble(), h.toDouble()), Imgproc.INTER_LINEAR, Core.BORDER_CONSTANT, Scalar(70.0))
            calibration.add(ByteArray(w * h).also { frame.get(0, 0, it) }, w, h)
            outcomes += calibration.progress().detection
        }
        val progress = calibration.progress()
        assertTrue("${progress.count} views, coverage ${progress.coverage}, detections $outcomes", progress.count >= 12 && progress.coverageReady)
        val lens = calibration.solve()
        assertTrue("reason ${lens.reason}, rms ${lens.rms}", lens.success)
        assertEquals(f, lens.fx, f * .03); assertEquals(w / 2.0, lens.cx, w * .03); assertEquals(h / 2.0, lens.cy, h * .03)
        // A different resolution is refused rather than mixed in.
        assertEquals(-2, calibration.add(ByteArray(640 * 480), 640, 480))
    }
}

private fun sub(a: DoubleArray, b: DoubleArray) = DoubleArray(3) { a[it] - b[it] }
private fun dot(a: DoubleArray, b: DoubleArray) = a[0] * b[0] + a[1] * b[1] + a[2] * b[2]
private fun cross(a: DoubleArray, b: DoubleArray) = doubleArrayOf(a[1] * b[2] - a[2] * b[1], a[2] * b[0] - a[0] * b[2], a[0] * b[1] - a[1] * b[0])
private fun normalize(a: DoubleArray): DoubleArray { val l = sqrt(dot(a, a)); return DoubleArray(3) { a[it] / l } }
