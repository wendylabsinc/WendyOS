package dev.wendy.android

import android.graphics.Bitmap
import android.graphics.Color
import android.os.Handler
import android.os.Looper
import android.view.PixelCopy
import android.view.SurfaceHolder
import androidx.test.core.app.ActivityScenario
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicReference
import org.junit.Assert.*
import org.junit.Assume.assumeNotNull
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class CameraPlayerTest {
    @Test fun rendersAllSlicesOfASensorLinkSizedPicture() {
        val instrumentation = InstrumentationRegistry.getInstrumentation()
        val video = instrumentation.context.assets.open("multislice-640x480.h264").use { it.readBytes() }
        render(video, 113) { bitmap ->
            // Sample every slice, not just the first decoded region.
            listOf(60, 180).all { y ->
                val c = bitmap.getPixel(320, y)
                Color.red(c) > 200 && Color.green(c) < 40 && Color.blue(c) < 40
            } && listOf(300, 420).all { y ->
                val c = bitmap.getPixel(320, y)
                Color.blue(c) > 200 && Color.red(c) < 40 && Color.green(c) < 40
            }
        }
    }

    /** Optional diagnostic replay; camera captures stay outside the repository. */
    @Test fun replaysCapturedStreamWhenProvided() {
        val name = InstrumentationRegistry.getArguments().getString("cameraFixture")
        assumeNotNull(name)
        val cache = InstrumentationRegistry.getInstrumentation().targetContext.cacheDir
        render(cache.resolve(name!!).readBytes(), 1024) { bitmap ->
            cache.resolve("camera-replay.png").outputStream().use { bitmap.compress(Bitmap.CompressFormat.PNG, 100, it) }
            true
        }
    }

    private fun render(video: ByteArray, chunkSize: Int, checkFrame: (Bitmap) -> Boolean) {
        val instrumentation = InstrumentationRegistry.getInstrumentation()
        val failure = AtomicReference<String?>()
        val ready = CountDownLatch(1)
        lateinit var player: CameraPlayer
        ActivityScenario.launch(MainActivity::class.java).use { scenario ->
            scenario.onActivity { activity ->
                activity.setTurnScreenOn(true)
                activity.setShowWhenLocked(true)
                player = CameraPlayer(activity, failed = { failure.set(it) })
                player.holder.addCallback(object : SurfaceHolder.Callback {
                    override fun surfaceCreated(holder: SurfaceHolder) { ready.countDown() }
                    override fun surfaceChanged(holder: SurfaceHolder, format: Int, width: Int, height: Int) {}
                    override fun surfaceDestroyed(holder: SurfaceHolder) {}
                })
                activity.setContentView(player)
            }
            try {
                assertTrue("Video surface was not created", ready.await(5, TimeUnit.SECONDS))
                // Deliberately split NAL units and pictures across input buffers.
                for (chunk in video.toList().chunked(chunkSize)) { player.append(chunk.toByteArray()); Thread.sleep(5) }
                var rendered = false
                var lastResult = -1
                var lastColors = ""
                repeat(30) {
                    if (!rendered && failure.get() == null) {
                        Thread.sleep(100)
                        val bitmap = Bitmap.createBitmap(640, 480, Bitmap.Config.ARGB_8888)
                        val copied = CountDownLatch(1)
                        var result = -1
                        PixelCopy.request(player, bitmap, { result = it; copied.countDown() }, Handler(Looper.getMainLooper()))
                        assertTrue(copied.await(3, TimeUnit.SECONDS))
                        lastResult = result
                        if (result == PixelCopy.SUCCESS) {
                            lastColors = listOf(60, 180, 300, 420).joinToString { y -> Integer.toHexString(bitmap.getPixel(320, y)) }
                            rendered = checkFrame(bitmap)
                        }
                        if (!rendered) instrumentation.targetContext.cacheDir.resolve("camera-decoder.png").outputStream().use {
                            bitmap.compress(Bitmap.CompressFormat.PNG, 100, it)
                        }
                        bitmap.recycle()
                    }
                }
                assertNull("Decoder error", failure.get())
                assertTrue("Decoded picture did not match; PixelCopy=$lastResult, colors=$lastColors", rendered)
            } finally { scenario.onActivity { player.close() } }
        }
    }
}
