package dev.wendy.android

import org.junit.Assert.*
import org.junit.Test

class H264AccessUnitsTest {
    private val prefix = byteArrayOf(0, 0, 0, 1)
    private val first = byteArrayOf(0x65, 0x80.toByte(), 0x11)
    private val continuation = byteArrayOf(0x65, 0x40, 0x22)
    private val next = byteArrayOf(0x41, 0x80.toByte(), 0x33)

    @Test fun collectsEverySliceBeforeSubmittingAPicture() {
        val units = H264AccessUnits()
        assertNull(units.append(first))
        assertNull(units.append(continuation))
        assertArrayEquals(prefix + first + prefix + continuation, units.append(next))
        assertArrayEquals(prefix + next, units.append(byteArrayOf(9, 0x10)))
    }

    @Test fun parameterSetsCloseThePreviousPicture() {
        for (type in listOf(6, 7, 8, 9)) {
            val units = H264AccessUnits()
            units.append(first)
            assertArrayEquals(prefix + first, units.append(byteArrayOf(type.toByte(), 0)))
            assertNull(units.append(byteArrayOf(type.toByte(), 0)))
        }
    }

    @Test fun transportPartitionsDoNotSplitPictures() {
        val bytes = prefix + first + prefix + continuation + prefix + next + prefix + byteArrayOf(9, 0x10) + prefix
        for (size in 1..bytes.size) {
            val parser = AnnexBParser()
            val units = H264AccessUnits()
            val pictures = bytes.toList().chunked(size).flatMap { parser.append(it.toByteArray()) }.mapNotNull(units::append)
            assertEquals(2, pictures.size)
            assertArrayEquals(prefix + first + prefix + continuation, pictures[0])
            assertArrayEquals(prefix + next, pictures[1])
        }
    }

    @Test fun ignoresOrphanedContinuationSlices() {
        val units = H264AccessUnits()
        assertNull(units.append(continuation))
        assertNull(units.append(first))
        assertArrayEquals(prefix + first, units.append(next))
    }

    @Test fun readsBaselineAndHighProfileDimensionsWithCropping() {
        // SPS from x264, including 1920x1080 cropped from 1088 coded lines.
        assertEquals(H264Dimensions(640, 480), h264Dimensions(hex("67 42 c0 16 d9 00 a0 3d b0 11 00 00 03 00 01 00 00 03 00 14 8f 16 2e 48")))
        assertEquals(H264Dimensions(1920, 1080), h264Dimensions(hex("67 64 00 28 ac d9 40 78 02 27 e5 c0 44 00 00 03 00 04 00 00 03 00 50 3c 60 c6 58")))
    }

    @Test fun rejectsTruncatedParameterSets() {
        for (sps in listOf(byteArrayOf(), byteArrayOf(0x67), byteArrayOf(0x67, 100, 0, 40, 0))) {
            assertThrows(IllegalArgumentException::class.java) { h264Dimensions(sps) }
        }
    }

    private fun hex(value: String) = value.split(' ').map { it.toInt(16).toByte() }.toByteArray()
}
