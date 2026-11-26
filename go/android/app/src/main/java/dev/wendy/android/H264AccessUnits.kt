package dev.wendy.android

import java.io.ByteArrayOutputStream

/** Groups AVC slices into pictures, independently of transport chunk boundaries. */
internal class H264AccessUnits {
    private val slices = mutableListOf<ByteArray>()
    private var size = 0

    fun append(nal: ByteArray): ByteArray? {
        if (nal.isEmpty()) return null
        val type = nal[0].toInt() and 31
        val slice = type == 1 || type == 5
        // first_mb_in_slice is an unsigned Exp-Golomb value: zero starts with 1.
        val firstSlice = slice && nal.size > 1 && nal[1].toInt() and 0x80 != 0
        val boundary = firstSlice || type in 6..9 || type in 14..18 || type == 10 || type == 11
        val completed = if (boundary) flush() else null
        if (slice) {
            require(nal.size > 1) { "Truncated H.264 slice" }
            // Ignore continuation slices if playback started in the middle of a picture.
            if (firstSlice || slices.isNotEmpty()) {
                require(size.toLong() + nal.size + 4 <= 16 * 1024 * 1024) { "Camera frame exceeds 16 MB" }
                slices.add(nal)
                size += nal.size + 4
            }
        }
        return completed
    }

    private fun flush(): ByteArray? {
        if (slices.isEmpty()) return null
        val output = ByteArrayOutputStream(size)
        for (slice in slices) { output.write(byteArrayOf(0, 0, 0, 1)); output.write(slice) }
        slices.clear()
        size = 0
        return output.toByteArray()
    }
}

internal data class H264Dimensions(val width: Int, val height: Int)

/** Reads picture dimensions from SPS, including high-profile fields and cropping. */
internal fun h264Dimensions(sps: ByteArray): H264Dimensions {
    require(sps.isNotEmpty() && sps[0].toInt() and 31 == 7) { "Missing H.264 SPS" }
    val bits = H264Bits(sps)
    bits.read(8)
    val profile = bits.read(8)
    bits.read(16)
    bits.ue()
    var chroma = 1
    var separateColourPlane = false
    if (profile in setOf(100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135)) {
        chroma = bits.ue()
        require(chroma in 0..3) { "Invalid H.264 chroma format" }
        if (chroma == 3) separateColourPlane = bits.read(1) == 1
        bits.ue(); bits.ue(); bits.read(1)
        if (bits.read(1) == 1) {
            repeat(if (chroma == 3) 12 else 8) { index ->
                if (bits.read(1) == 1) {
                    var last = 8
                    var next = 8
                    repeat(if (index < 6) 16 else 64) {
                        if (next != 0) next = (last + bits.se() + 256) and 255
                        if (next != 0) last = next
                    }
                }
            }
        }
    }
    bits.ue()
    when (bits.ue()) {
        0 -> bits.ue()
        1 -> {
            bits.read(1); bits.se(); bits.se()
            val cycle = bits.ue()
            require(cycle <= 255) { "Invalid H.264 picture order cycle" }
            repeat(cycle) { bits.se() }
        }
        2 -> Unit
        else -> error("Invalid H.264 picture order type")
    }
    bits.ue(); bits.read(1)
    val widthMBs = bits.ue().toLong() + 1
    val heightMapUnits = bits.ue().toLong() + 1
    val frameOnly = bits.read(1)
    if (frameOnly == 0) bits.read(1)
    bits.read(1)
    var cropX = 0L
    var cropY = 0L
    if (bits.read(1) == 1) {
        cropX = bits.ue().toLong() + bits.ue()
        cropY = bits.ue().toLong() + bits.ue()
    }
    val chromaArray = if (separateColourPlane) 0 else chroma
    val cropUnitX = if (chromaArray == 1 || chromaArray == 2) 2 else 1
    val cropUnitY = (if (chromaArray == 1) 2 else 1) * (2 - frameOnly)
    val width = widthMBs * 16 - cropX * cropUnitX
    val height = heightMapUnits * 16 * (2 - frameOnly) - cropY * cropUnitY
    require(width in 1..8192 && height in 1..8192) { "Invalid H.264 picture size" }
    return H264Dimensions(width.toInt(), height.toInt())
}

private class H264Bits(nal: ByteArray) {
    private val bytes = ByteArrayOutputStream(nal.size).apply {
        var zeros = 0
        for (byte in nal) {
            val value = byte.toInt() and 255
            if (zeros == 2 && value == 3) { zeros = 0; continue }
            write(value)
            zeros = if (value == 0) zeros + 1 else 0
        }
    }.toByteArray()
    private var position = 0
    fun read(count: Int): Int {
        require(position.toLong() + count <= bytes.size.toLong() * 8) { "Truncated H.264 SPS" }
        var value = 0
        repeat(count) {
            value = (value shl 1) or ((bytes[position / 8].toInt() ushr (7 - position % 8)) and 1)
            position++
        }
        return value
    }
    fun ue(): Int {
        var zeros = 0
        while (read(1) == 0) { zeros++; require(zeros < 31) { "Invalid H.264 Exp-Golomb value" } }
        return (1 shl zeros) - 1 + read(zeros)
    }
    fun se(): Int { val value = ue(); return if (value and 1 == 0) -(value / 2) else value / 2 + 1 }
}
