package dev.wendy.android

import android.content.Context
import android.media.MediaCodec
import android.media.MediaFormat
import android.os.Handler
import android.os.HandlerThread
import android.view.Surface
import android.view.SurfaceHolder
import android.view.SurfaceView
import java.nio.ByteBuffer
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.atomic.AtomicInteger

class CameraPlayer(context: Context, private val failed: (String) -> Unit, private val resized: (Int, Int) -> Unit = { _, _ -> }) : SurfaceView(context), SurfaceHolder.Callback {
    private val thread = HandlerThread("WendyCamera").apply { start() }
    private val worker = Handler(thread.looper)
    private val closed = AtomicBoolean(false)
    private val queued = AtomicInteger(0)
    private var parser = AnnexBParser()
    private var accessUnits = H264AccessUnits()
    private var codec: MediaCodec? = null
    private var surface: Surface? = null
    private var sps: ByteArray? = null
    private var pps: ByteArray? = null
    private var waitingForKeyframe = true
    private val drainOutput = object : Runnable {
        override fun run() {
            val decoder = codec ?: return
            try { drain(decoder) }
            catch (e: Exception) { resetStream(); post { failed(e.message ?: "Camera playback failed") }; return }
            if (!closed.get()) worker.postDelayed(this, 10)
        }
    }
    init { holder.addCallback(this) }
    override fun surfaceCreated(holder: SurfaceHolder) { worker.post { surface = holder.surface } }
    override fun surfaceChanged(holder: SurfaceHolder, format: Int, width: Int, height: Int) {}
    override fun surfaceDestroyed(holder: SurfaceHolder) { worker.post { resetStream(); surface = null } }
    fun append(bytes: ByteArray) {
        if (closed.get()) return
        if (queued.incrementAndGet() > 64) { queued.decrementAndGet(); worker.post { resetStream() }; return }
        worker.post {
            try { if (!closed.get()) parser.append(bytes).forEach(::nal) }
            catch (e: Exception) { resetStream(); post { failed(e.message ?: "Camera playback failed") } }
            finally { queued.decrementAndGet() }
        }
    }
    private fun nal(nal: ByteArray) {
        if (nal.isEmpty()) return
        // MediaCodec requires a complete access unit, not one buffer per slice.
        // Flush the previous picture before replacing its parameter sets.
        accessUnits.append(nal)?.let(::frame)
        when (nal[0].toInt() and 31) {
            7 -> { if (sps != null && !sps!!.contentEquals(nal)) releaseCodec(); sps = nal; return }
            8 -> { if (pps != null && !pps!!.contentEquals(nal)) releaseCodec(); pps = nal; return }
        }
    }
    private fun frame(frame: ByteArray) {
        if (surface == null || sps == null || pps == null) return
        val keyframe = frame[4].toInt() and 31 == 5
        if (waitingForKeyframe && !keyframe) return
        if (codec == null) {
            val dimensions = h264Dimensions(sps!!)
            post { resized(dimensions.width, dimensions.height) }
            val format = MediaFormat.createVideoFormat("video/avc", dimensions.width, dimensions.height).apply {
                setByteBuffer("csd-0", ByteBuffer.wrap(byteArrayOf(0,0,0,1) + sps!!))
                setByteBuffer("csd-1", ByteBuffer.wrap(byteArrayOf(0,0,0,1) + pps!!))
                setInteger(MediaFormat.KEY_MAX_INPUT_SIZE, 4 * 1024 * 1024)
            }
            val decoder = MediaCodec.createDecoderByType("video/avc")
            try { decoder.configure(format, surface, null, 0); decoder.start(); codec = decoder; worker.post(drainOutput) }
            catch (e: Exception) { decoder.release(); throw e }
        }
        val decoder = codec ?: return
        drain(decoder)
        val index = decoder.dequeueInputBuffer(10000)
        if (index < 0) { waitingForKeyframe = true; return }
        val buffer = decoder.getInputBuffer(index)!!
        if (frame.size > buffer.capacity()) throw IllegalArgumentException("Camera frame exceeds decoder capacity")
        buffer.clear(); buffer.put(frame)
        decoder.queueInputBuffer(index, 0, frame.size, System.nanoTime() / 1000, 0)
        waitingForKeyframe = false
        drain(decoder)
    }
    private fun drain(decoder: MediaCodec) {
        val info = MediaCodec.BufferInfo()
        repeat(16) { val index = decoder.dequeueOutputBuffer(info, 0); if (index >= 0) decoder.releaseOutputBuffer(index, true) else if (index == MediaCodec.INFO_TRY_AGAIN_LATER) return }
    }
    private fun releaseCodec() { worker.removeCallbacks(drainOutput); codec?.let { try { it.stop() } finally { it.release() } }; codec = null; waitingForKeyframe = true }
    private fun resetStream() { releaseCodec(); parser = AnnexBParser(); accessUnits = H264AccessUnits(); sps = null; pps = null }
    fun close() { if (closed.compareAndSet(false, true)) worker.post { releaseCodec(); thread.quitSafely() } }
}
