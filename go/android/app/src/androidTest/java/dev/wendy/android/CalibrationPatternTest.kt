package dev.wendy.android

import androidx.compose.foundation.layout.size
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.asAndroidBitmap
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.test.captureToImage
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.unit.dp
import androidx.test.ext.junit.runners.AndroidJUnit4
import org.junit.Assert.*
import org.junit.BeforeClass
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith
import org.opencv.android.OpenCVLoader

/** The pattern the phone shows, and the PNG it saves, are the 6×9 checkerboard the lens solver detects. */
@RunWith(AndroidJUnit4::class)
class CalibrationPatternTest {
    companion object { @BeforeClass @JvmStatic fun load() { assertTrue(OpenCVLoader.initLocal()) } }
    @get:Rule val compose = createComposeRule()

    private fun gray(bitmap: android.graphics.Bitmap): ByteArray {
        val pixels = IntArray(bitmap.width * bitmap.height); bitmap.getPixels(pixels, 0, bitmap.width, 0, 0, bitmap.width, bitmap.height)
        return ByteArray(pixels.size) { ((pixels[it] shr 8) and 255).toByte() }
    }

    /** A camera seeing the pattern on a grey wall: centred in a larger frame so the white border stays visible. */
    private fun detect(pattern: android.graphics.Bitmap): Int {
        val w = 1280; val h = 960
        val frame = android.graphics.Bitmap.createBitmap(w, h, android.graphics.Bitmap.Config.ARGB_8888).apply { eraseColor(android.graphics.Color.rgb(70, 70, 70)) }
        val scale = minOf(600f / pattern.width, 800f / pattern.height)
        val scaled = android.graphics.Bitmap.createScaledBitmap(pattern, (pattern.width * scale).toInt(), (pattern.height * scale).toInt(), true)
        android.graphics.Canvas(frame).drawBitmap(scaled, (w - scaled.width) / 2f, (h - scaled.height) / 2f, null)
        val calibration = ScreenLensCalibrator()
        calibration.add(gray(frame), w, h)
        return calibration.progress().let { if (it.detection == 4) it.count else -it.detection }
    }

    @Test fun theOnScreenPatternIsDetected() {
        compose.setContent { CalibrationCheckerboard(Modifier.size(360.dp, 640.dp).testTag("pattern")) }
        val shown = compose.onNodeWithTag("pattern").captureToImage().asAndroidBitmap()
        assertEquals(1, detect(shown))
    }

    @Test fun theSavedPatternIsDetected() {
        val png = calibrationPattern()
        assertEquals(900, png.width); assertEquals(1200, png.height)
        assertEquals(1, detect(png))
    }
}
