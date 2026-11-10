package dev.wendy.android

import android.graphics.Bitmap
import android.media.MediaCodec
import android.media.MediaCodecInfo
import android.media.MediaFormat
import android.os.Handler
import android.os.HandlerThread
import java.nio.ByteBuffer
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicInteger
import java.util.concurrent.atomic.AtomicLong

/**
 * Luma frames for camera calibration (iOS H264CalibrationFrames). While an owner holds it, each self-contained
 * IDR picture is decoded by a separate short-lived decoder, so the display path stays unchanged. A generation keeps
 * late output from an earlier camera or owner out of [latest].
 */
class CameraCalibrationFrames {
    private val owners = AtomicInteger(0)
    private val generation = AtomicLong(0)
    private val pending = AtomicBoolean(false)
    private val sequence = AtomicLong(0)
    @Volatile var latest: CameraCalibrationFrame? = null; private set
    /** The newest decoded picture in colour, kept only while [wantsColor] (iOS latestColor, for projection). */
    @Volatile var latestColor: CameraColorFrame? = null; private set
    @Volatile var wantsColor = false
    private val worker by lazy { Handler(HandlerThread("WendyCalibrationFrames").apply { start() }.looper) }

    val enabled get() = owners.get() > 0

    fun acquire() { if (owners.getAndIncrement() == 0) reset() }
    fun release() { if (owners.decrementAndGet() <= 0) { owners.set(0); reset() } }
    fun reset() { generation.incrementAndGet(); latest = null; latestColor = null; pending.set(false) }

    /** One access unit holding an IDR picture, with the stream's parameter sets. */
    internal fun acceptH264(sps: ByteArray, pps: ByteArray, accessUnit: ByteArray, width: Int, height: Int) {
        if (!enabled || width !in 1..8192 || height !in 1..8192 || width.toLong() * height > 32_000_000 || !pending.compareAndSet(false, true)) return
        val current = generation.get(); val received = System.currentTimeMillis(); val number = sequence.incrementAndGet()
        worker.post {
            try {
                if (!enabled || generation.get() != current) return@post
                val frame = decodeLuma(sps, pps, accessUnit, width, height) ?: return@post
                if (enabled && generation.get() == current) {
                    latest = CameraCalibrationFrame(frame.gray, frame.width, frame.height, received, number, "H.264")
                    frame.color?.let { latestColor = CameraColorFrame(it, received, number) }
                }
            } catch (_: Exception) { /* A frame that does not decode is skipped; the next IDR picture is tried. */ }
            finally { if (generation.get() == current) pending.set(false) }
        }
    }

    /** A decoded JPEG frame (iOS CameraCalibrationFrame.jpeg draws it into a grey context). */
    fun acceptJpeg(bitmap: Bitmap) {
        if (!enabled) return
        val width = bitmap.width; val height = bitmap.height
        if (width !in 1..8192 || height !in 1..8192 || width.toLong() * height > 32_000_000) return
        val pixels = IntArray(width * height); bitmap.getPixels(pixels, 0, width, 0, 0, width, height)
        val gray = ByteArray(pixels.size) { i -> val c = pixels[i]; ((((c shr 16) and 255) * 299 + ((c shr 8) and 255) * 587 + (c and 255) * 114 + 500) / 1000).toByte() }
        val now = System.currentTimeMillis(); val number = sequence.incrementAndGet()
        latest = CameraCalibrationFrame(gray, width, height, now, number, "JPEG")
        if (wantsColor) latestColor = CameraColorFrame(bitmap, now, number)
    }

    private class Decoded(val gray: ByteArray, val width: Int, val height: Int, val color: Bitmap?)

    private fun decodeLuma(sps: ByteArray, pps: ByteArray, accessUnit: ByteArray, width: Int, height: Int): Decoded? {
        val start = byteArrayOf(0, 0, 0, 1)
        val format = MediaFormat.createVideoFormat("video/avc", width, height).apply {
            setByteBuffer("csd-0", ByteBuffer.wrap(start + sps)); setByteBuffer("csd-1", ByteBuffer.wrap(start + pps))
            setInteger(MediaFormat.KEY_MAX_INPUT_SIZE, maxOf(accessUnit.size + 1024, 1024 * 1024))
            setInteger(MediaFormat.KEY_COLOR_FORMAT, MediaCodecInfo.CodecCapabilities.COLOR_FormatYUV420Flexible)
        }
        val codec = MediaCodec.createDecoderByType("video/avc")
        try {
            codec.configure(format, null, null, 0); codec.start()
            val deadline = System.nanoTime() + 1_500_000_000L
            var queued = false; var ended = false
            val info = MediaCodec.BufferInfo()
            while (System.nanoTime() < deadline) {
                if (!ended) {
                    val index = codec.dequeueInputBuffer(10_000)
                    if (index >= 0) {
                        val buffer = codec.getInputBuffer(index)!!
                        if (!queued) {
                            if (accessUnit.size > buffer.capacity()) return null
                            buffer.clear(); buffer.put(accessUnit); codec.queueInputBuffer(index, 0, accessUnit.size, 0, 0); queued = true
                        } else { codec.queueInputBuffer(index, 0, 0, 0, MediaCodec.BUFFER_FLAG_END_OF_STREAM); ended = true }
                    }
                }
                val out = codec.dequeueOutputBuffer(info, 10_000)
                if (out < 0) continue
                try {
                    if (info.size <= 0 && info.flags and MediaCodec.BUFFER_FLAG_END_OF_STREAM != 0) return null
                    val image = codec.getOutputImage(out) ?: continue
                    image.use {
                        val crop = it.cropRect; val w = crop.width(); val h = crop.height()
                        if (w <= 0 || h <= 0) return null
                        val plane = it.planes[0]; val data = plane.buffer; val rowStride = plane.rowStride; val pixelStride = plane.pixelStride
                        val gray = ByteArray(w * h)
                        for (y in 0 until h) for (x in 0 until w) gray[y * w + x] = data.get((crop.top + y) * rowStride + (crop.left + x) * pixelStride)
                        // Video-range luma is expanded to full range, as on iOS.
                        val limited = runCatching { codec.outputFormat.getInteger(MediaFormat.KEY_COLOR_RANGE) }.getOrNull() != MediaFormat.COLOR_RANGE_FULL
                        val color = if (wantsColor) colour(it, gray, w, h, limited) else null
                        if (limited) for (i in gray.indices) gray[i] = (((gray[i].toInt() and 255) - 16) * 255 / 219).coerceIn(0, 255).toByte()
                        return Decoded(gray, w, h, color)
                    }
                } finally { codec.releaseOutputBuffer(out, false) }
            }
            return null
        } finally { runCatching { codec.stop() }; codec.release() }
    }

    /** BT.601 YUV 4:2:0 to RGB, from the luma already read and the chroma planes. */
    private fun colour(image: android.media.Image, luma: ByteArray, w: Int, h: Int, limited: Boolean): Bitmap {
        val crop = image.cropRect; val u = image.planes[1]; val v = image.planes[2]
        val pixels = IntArray(w * h)
        for (y in 0 until h) for (x in 0 until w) {
            val cy = (crop.top + y) / 2; val cx = (crop.left + x) / 2
            val cb = (u.buffer.get(cy * u.rowStride + cx * u.pixelStride).toInt() and 255) - 128
            val cr = (v.buffer.get(cy * v.rowStride + cx * v.pixelStride).toInt() and 255) - 128
            val l = luma[y * w + x].toInt() and 255
            val yy = if (limited) (l - 16) * 1.164f else l.toFloat()
            val chroma = if (limited) 1.138f else 1f
            val r = (yy + 1.402f * chroma * cr).toInt().coerceIn(0, 255)
            val g = (yy - 0.344f * chroma * cb - 0.714f * chroma * cr).toInt().coerceIn(0, 255)
            val b = (yy + 1.772f * chroma * cb).toInt().coerceIn(0, 255)
            pixels[y * w + x] = (0xFF shl 24) or (r shl 16) or (g shl 8) or b
        }
        return Bitmap.createBitmap(pixels, w, h, Bitmap.Config.ARGB_8888)
    }
}

/** A colour picture from a camera, for projecting onto the site model (iOS SiteCameraColorFrame). */
class CameraColorFrame(val image: Bitmap, val received: Long, val sequence: Long) {
    fun matches(lens: SiteCameraLens, now: Long) = lens.valid && image.width == lens.width && image.height == lens.height && now - received in 0..3000
}
