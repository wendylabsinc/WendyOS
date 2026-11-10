package dev.wendy.android

import android.graphics.BitmapFactory
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import org.junit.Assert.*
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.opencv.android.Utils
import org.opencv.core.CvType
import org.opencv.core.Mat
import org.opencv.core.MatOfPoint2f
import org.opencv.core.Point
import org.opencv.core.Scalar
import org.opencv.core.Size
import org.opencv.imgproc.Imgproc
import kotlin.random.Random

/** Runs the real OpenCV pipeline on the device: a tilted photo of a known board must be found where it was drawn. */
@RunWith(AndroidJUnit4::class)
class DeviceRecognitionTest {
    private val context = InstrumentationRegistry.getInstrumentation().targetContext
    private lateinit var engine: DeviceRecognitionEngine
    private val frameWidth = 1280
    private val frameHeight = 960

    @Before fun setUp() { engine = DeviceRecognitionEngine(context).also { it.load() } }

    private fun asset(name: String): Mat {
        val bitmap = context.assets.open("DeviceRecognition/$name.jpg").use { BitmapFactory.decodeStream(it) }
        val rgba = Mat(); Utils.bitmapToMat(bitmap, rgba)
        return Mat().also { Imgproc.cvtColor(rgba, it, Imgproc.COLOR_RGBA2GRAY) }
    }

    /** Draws [image] into a grey frame so its corners land on [target]; returns the frame and the mapping. */
    private fun place(image: Mat, target: List<Point>): Pair<ByteArray, Mat> {
        val source = MatOfPoint2f(Point(0.0, 0.0), Point(image.cols().toDouble(), 0.0), Point(image.cols().toDouble(), image.rows().toDouble()), Point(0.0, image.rows().toDouble()))
        val mapping = Imgproc.getPerspectiveTransform(source, MatOfPoint2f(*target.toTypedArray()))
        val frame = Mat()
        Imgproc.warpPerspective(image, frame, mapping, Size(frameWidth.toDouble(), frameHeight.toDouble()), Imgproc.INTER_LINEAR, org.opencv.core.Core.BORDER_CONSTANT, Scalar(128.0))
        return ByteArray(frameWidth * frameHeight).also { frame.get(0, 0, it) } to mapping
    }

    private fun expected(quad: List<Point>, mapping: Mat): List<SightPoint> {
        val out = MatOfPoint2f(); org.opencv.core.Core.perspectiveTransform(MatOfPoint2f(*quad.toTypedArray()), out, mapping)
        return out.toList().map { SightPoint(it.x / frameWidth, it.y / frameHeight) }
    }

    private fun assertCorners(expected: List<SightPoint>, actual: List<SightPoint>) {
        assertEquals(4, actual.size)
        expected.zip(actual).forEach { (e, a) ->
            assertEquals("x", e.x, a.x, 0.03); assertEquals("y", e.y, a.y, 0.03)
        }
    }

    @Test fun findsATiltedBundledBoardWhereItWasDrawn() {
        val (frame, mapping) = place(asset("seeed-xiao"), listOf(Point(260.0, 120.0), Point(1060.0, 170.0), Point(1020.0, 860.0), Point(300.0, 820.0)))
        val report = engine.inspect(frame, frameWidth, frameHeight)
        val match = report.matches.singleOrNull { it.id == "bundled:seeed-xiao" }
        assertNotNull("stage ${report.stage}, ${report.bestMatches} matches, ${report.bestInliers} inliers", match)
        // The bundled quad is in the original photo's pixels.
        val quad = listOf(Point(480.0, 280.0), Point(1190.0, 280.0), Point(1190.0, 1100.0), Point(480.0, 1100.0))
        assertCorners(expected(quad, mapping), match!!.corners)
        assertTrue(match.residual <= 3)
    }

    @Test fun aFeaturelessFrameMatchesNothing() {
        val report = engine.inspect(ByteArray(frameWidth * frameHeight) { 128.toByte() }, frameWidth, frameHeight)
        assertTrue(report.matches.isEmpty())
        assertEquals(1, report.stage)
    }

    @Test fun aRememberedAppearanceIsFoundAndCanBeForgotten() {
        // A synthetic board: random blocks and rings with a fixed seed, so the test needs no photo of real hardware.
        val board = Mat(480, 640, CvType.CV_8U, Scalar(200.0))
        val random = Random(7)
        repeat(120) {
            val x = random.nextInt(600).toDouble(); val y = random.nextInt(440).toDouble()
            val shade = Scalar(random.nextInt(0, 160).toDouble())
            if (it % 2 == 0) Imgproc.rectangle(board, Point(x, y), Point(x + random.nextInt(8, 60), y + random.nextInt(8, 60)), shade, -1)
            else Imgproc.circle(board, Point(x, y), random.nextInt(5, 30), shade, 3)
        }
        val gray = ByteArray(640 * 480).also { board.get(0, 0, it) }
        val reference = DeviceVisualReference(label = "Test board", deviceID = "test:recognition", width = 640, height = 480, gray = gray,
            quad = doubleArrayOf(40.0, 40.0, 600.0, 40.0, 600.0, 440.0, 40.0, 440.0))
        engine.remember(reference)
        try {
            assertTrue(engine.registered().any { it.id == reference.id })
            val (frame, mapping) = place(board, listOf(Point(300.0, 200.0), Point(980.0, 240.0), Point(940.0, 760.0), Point(330.0, 720.0)))
            val match = engine.inspect(frame, frameWidth, frameHeight).matches.singleOrNull { it.id == reference.id }
            assertNotNull(match)
            assertEquals("test:recognition", match!!.deviceID)
            assertCorners(expected(listOf(Point(40.0, 40.0), Point(600.0, 40.0), Point(600.0, 440.0), Point(40.0, 440.0)), mapping), match.corners)
            // A second engine reads the same library from disk.
            assertTrue(DeviceRecognitionEngine(context).also { it.load() }.registered().any { it.id == reference.id })
        } finally {
            engine.forget(reference.id)
        }
        assertTrue(engine.registered().none { it.id == reference.id })
        val (frame, _) = place(board, listOf(Point(300.0, 200.0), Point(980.0, 240.0), Point(940.0, 760.0), Point(330.0, 720.0)))
        assertTrue(engine.inspect(frame, frameWidth, frameHeight).matches.none { it.id == reference.id })
    }
}
