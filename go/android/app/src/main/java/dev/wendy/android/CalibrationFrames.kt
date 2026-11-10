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
    private val worker by lazy { Handler(HandlerThread("WendyCalibrationFrames").apply { start() }.looper) }

    val enabled get() = owners.get() > 0

    fun acquire() { if (owners.getAndIncrement() == 0) reset() }
    fun release() { if (owners.decrementAndGet() <= 0) { owners.set(0); reset() } }
    fun reset() { generation.incrementAndGet(); latest = null; pending.set(false) }

    /** One access unit holding an IDR picture, with the stream's parameter sets. */
    internal fun acceptH264(sps: ByteArray, pps: ByteArray, accessUnit: ByteArray, width: Int, height: Int) {
        if (!enabled || width !in 1..8192 || height !in 1..8192 || width.toLong() * height > 32_000_000 || !pending.compareAndSet(false, true)) return
        val current = generation.get(); val received = System.currentTimeMillis(); val number = sequence.incrementAndGet()
        worker.post {
            try {
                if (!enabled || generation.get() != current) return@post
                val frame = decodeLuma(sps, pps, accessUnit, width, height) ?: return@post
                if (enabled && generation.get() == current) latest = CameraCalibrationFrame(frame.first, frame.second, frame.third, received, number, "H.264")
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
        latest = CameraCalibrationFrame(gray, width, height, System.currentTimeMillis(), sequence.incrementAndGet(), "JPEG")
    }

    private fun decodeLuma(sps: ByteArray, pps: ByteArray, accessUnit: ByteArray, width: Int, height: Int): Triple<ByteArray, Int, Int>? {
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
                        if (limited) for (i in gray.indices) gray[i] = (((gray[i].toInt() and 255) - 16) * 255 / 219).coerceIn(0, 255).toByte()
                        return Triple(gray, w, h)
                    }
                } finally { codec.releaseOutputBuffer(out, false) }
            }
            return null
        } finally { runCatching { codec.stop() }; codec.release() }
    }
}
