package dev.wendy.android

import org.junit.Assert.*
import org.junit.Test

class AnnexBParserTest {
    @Test fun splitStartCodesAndMixedPrefixes() {
        val parser = AnnexBParser()
        assertTrue(parser.append(byteArrayOf(99,0,0)).isEmpty())
        assertTrue(parser.append(byteArrayOf(0,1,0x67,2,0)).isEmpty())
        val first = parser.append(byteArrayOf(0,1,0x68,3,0,0,0))
        assertEquals(1, first.size); assertArrayEquals(byteArrayOf(0x67,2), first[0])
        val second = parser.append(byteArrayOf(1,0x65,4))
        assertArrayEquals(byteArrayOf(0x68,3), second.single())
    }
    @Test fun partitioningDoesNotChangeNALs() {
        val bytes = byteArrayOf(0,0,0,1,0x67,4,0,0,1,0x68,5,0,0,0,1,0x65,7,0,0,1,9)
        val expected = AnnexBParser().append(bytes)
        for (size in 1..bytes.size) {
            val parser = AnnexBParser()
            val actual = bytes.toList().chunked(size).flatMap { parser.append(it.toByteArray()) }
            assertEquals(expected.size, actual.size)
            expected.indices.forEach { assertArrayEquals("chunk size $size", expected[it], actual[it]) }
        }
    }
    @Test fun malformedInputIsBoundedAndRecovers() {
        val parser = AnnexBParser()
        assertTrue(parser.append(ByteArray(17 * 1024 * 1024)).isEmpty())
        val actual = parser.append(byteArrayOf(0,0,1,0x65,7,0,0,1,9))
        assertArrayEquals(byteArrayOf(0x65,7), actual.single())
    }
}
