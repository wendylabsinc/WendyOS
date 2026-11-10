package dev.wendy.android

import android.graphics.Bitmap
import androidx.compose.foundation.layout.size
import androidx.compose.runtime.mutableStateOf
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.asAndroidBitmap
import androidx.compose.ui.platform.testTag
import androidx.compose.ui.test.captureToImage
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.test.onNodeWithTag
import androidx.compose.ui.unit.dp
import androidx.compose.runtime.CompositionLocalProvider
import androidx.test.ext.junit.runners.AndroidJUnit4
import org.junit.Assert.*
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith

/** A camera's picture is painted onto the model where that camera sees it, and only there. */
@RunWith(AndroidJUnit4::class)
class SiteProjectionRenderTest {
    @get:Rule val compose = createComposeRule()
    private val revision = Site(name = "Room").modelRevision

    private fun quad(name: String, a: FloatArray, b: FloatArray, c: FloatArray, d: FloatArray, normal: FloatArray): SiteMesh {
        val corners = listOf(a, b, c, d)
        val positions = listOf(0, 1, 2, 0, 2, 3).flatMap { corners[it].toList() }.toFloatArray()
        return SiteMesh(name, positions, FloatArray(positions.size) { normal[it % 3] }, floatArrayOf(.55f, .55f, .55f))
    }

    // A back wall at z = −3 and a side wall at x = 3, which the camera (looking along −z, 70° wide) cannot see.
    private val geometry = SiteGeometry(listOf(
        quad("Wall0", floatArrayOf(-3f, 0f, -3f), floatArrayOf(3f, 0f, -3f), floatArrayOf(3f, 3f, -3f), floatArrayOf(-3f, 3f, -3f), floatArrayOf(0f, 0f, 1f)),
        quad("Wall1", floatArrayOf(3f, 0f, -3f), floatArrayOf(3f, 0f, 3f), floatArrayOf(3f, 3f, 3f), floatArrayOf(3f, 3f, -3f), floatArrayOf(-1f, 0f, 0f)),
        quad("Storage0", floatArrayOf(-0.4f, 1.1f, -1.5f), floatArrayOf(0.4f, 1.1f, -1.5f), floatArrayOf(0.4f, 1.9f, -1.5f), floatArrayOf(-0.4f, 1.9f, -1.5f), floatArrayOf(0f, 0f, 1f))))

    private fun red(bitmap: Bitmap): Int {
        var count = 0
        for (y in 0 until bitmap.height step 4) for (x in 0 until bitmap.width step 4) { val c = bitmap.getPixel(x, y); if ((c shr 16 and 255) > 180 && (c shr 8 and 255) < 80 && (c and 255) < 80) count++ }
        return count
    }

    @Test fun projectsOnlyWhereTheCameraSees() {
        val camera = SiteManualAlignment(revision, 180.0, 0.0, 180.0, 70.0, 640, 480).geometry(SitePoint(0.0, 1.5, 0.0), revision, System.currentTimeMillis())!!
        val picture = Bitmap.createBitmap(640, 480, Bitmap.Config.ARGB_8888).apply { eraseColor(android.graphics.Color.RED) }
        val projectors = mutableStateOf(emptyList<SiteProjector>())
        val statuses = mutableStateOf(emptyMap<String, String>())
        compose.setContent {
            WendyTheme {
                SiteSceneView(geometry, emptyList(), false, {}, {}, Modifier.size(360.dp, 480.dp).testTag("scene"), projectors.value) { statuses.value = it }
            }
        }
        compose.waitForIdle(); Thread.sleep(1500)
        val plain = compose.onNodeWithTag("scene").captureToImage().asAndroidBitmap()
        assertEquals("No red before projecting", 0, red(plain))
        compose.runOnIdle { projectors.value = listOf(SiteProjector("door", camera, picture, System.currentTimeMillis())) }
        var shown = 0
        repeat(20) { if (shown < 200) { Thread.sleep(250); compose.waitForIdle(); shown = red(compose.onNodeWithTag("scene").captureToImage().asAndroidBitmap()) } }
        assertTrue("red samples: $shown, status: ${statuses.value}", shown >= 200)
        // The panel between the camera and the wall leaves an unpainted shadow inside the painted area.
        val frame = compose.onNodeWithTag("scene").captureToImage().asAndroidBitmap()
        val reds = ArrayList<Pair<Int, Int>>()
        for (y in 0 until frame.height step 4) for (x in 0 until frame.width step 4) { val c = frame.getPixel(x, y); if ((c shr 16 and 255) > 180 && (c shr 8 and 255) < 80 && (c and 255) < 80) reds += x to y }
        val left = reds.minOf { it.first }; val right = reds.maxOf { it.first }; val top = reds.minOf { it.second }; val bottom = reds.maxOf { it.second }
        var shadow = 0
        for (y in top until bottom step 4) for (x in left until right step 4) {
            val c = frame.getPixel(x, y); val r = c shr 16 and 255; val g = c shr 8 and 255; val b = c and 255
            if (r in 40..140 && kotlin.math.abs(r - g) < 12 && kotlin.math.abs(g - b) < 12) shadow++
        }
        assertTrue("shadow samples: $shadow", shadow >= 100)
        androidx.test.platform.app.InstrumentationRegistry.getInstrumentation().targetContext.cacheDir.resolve("site-projection.png").outputStream().use {
            compose.onNodeWithTag("scene").captureToImage().asAndroidBitmap().compress(Bitmap.CompressFormat.PNG, 100, it)
        }
        assertEquals("Live camera color · Manually aligned", statuses.value["door"])
    }
}
