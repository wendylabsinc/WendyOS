package dev.wendy.android

import org.junit.Assert.*
import org.junit.Test
import java.nio.ByteBuffer
import java.nio.ByteOrder

/** Fixtures were written by Apple's usdcat and usdzip, and by SceneKit's USDZ export, on macOS. */
class UsdReaderTest {
    private fun fixture(name: String) = javaClass.classLoader!!.getResourceAsStream("sites/$name")!!.readBytes()

    private fun longs(vararg values: Long): ByteArray = ByteBuffer.allocate(values.size * 8).order(ByteOrder.LITTLE_ENDIAN).apply { values.forEach { putLong(it) } }.array()

    /** A usdc layer: the bootstrap, then [sections] in order, then their table of contents. */
    private fun crate(vararg sections: Pair<String, ByteArray>): ByteArray {
        val toc = 88 + sections.sumOf { it.second.size }
        val out = ByteBuffer.allocate(toc + 8 + sections.size * 32).order(ByteOrder.LITTLE_ENDIAN)
        out.put("PXR-USDC".toByteArray(Charsets.US_ASCII)); out.put(byteArrayOf(0, 8, 0)); out.putLong(16, toc.toLong())
        out.position(88)
        for ((_, bytes) in sections) out.put(bytes)
        out.putLong(sections.size.toLong())
        var start = 88L
        for ((name, bytes) in sections) { out.put(name.toByteArray(Charsets.US_ASCII).copyOf(16)); out.putLong(start); out.putLong(bytes.size.toLong()); start += bytes.size }
        return out.array()
    }

    // One token, "a": TfFastCompression's header, then an LZ4 block of two literal bytes.
    private val tokens = "TOKENS" to (longs(1, 2, 4) + byteArrayOf(0, 0x20, 'a'.code.toByte(), 0))
    private val noStrings = "STRINGS" to longs(0)
    private val noFields = "FIELDS" to (longs(0, 0, 1) + byteArrayOf(0))
    private val noFieldSets = "FIELDSETS" to longs(0, 0)

    /** Compressed integers as USD writes them: the byte count, then [raw] as one literal LZ4 block after TfFastCompression's header. */
    private fun ints(raw: ByteArray): ByteArray {
        val length = if (raw.size < 15) byteArrayOf((raw.size shl 4).toByte())
            else byteArrayOf(0xF0.toByte()) + ByteArray((raw.size - 15) / 255) { 0xFF.toByte() } + byteArrayOf(((raw.size - 15) % 255).toByte())
        val framed = byteArrayOf(0) + length + raw
        return longs(framed.size.toLong()) + framed
    }

    @Test fun textBinaryAndArchiveLayersReadTheSameRoom() {
        val geometries = listOf("room.usda", "room.usdc", "room.usdz").map { UsdReader.read(fixture(it)) }
        for (geometry in geometries) {
            assertEquals(listOf("Panel", "Box"), geometry.meshes.map { it.name })
            // A quad and a triangle make three triangles; a cube makes twelve.
            assertEquals(3, geometry.meshes[0].positions.size / 9)
            assertEquals(12, geometry.meshes[1].positions.size / 9)
            // Centimetres become metres and Z up becomes Y up: (x, y, z) to (x, z, -y).
            assertArrayEquals(floatArrayOf(-0.5f, -0.5f, -3.5f), geometry.min, 1e-4f)
            assertArrayEquals(floatArrayOf(3f, 3f, 0f), geometry.max, 1e-4f)
            assertArrayEquals(floatArrayOf(0.8f, 0.2f, 0.2f), geometry.meshes[0].color, 1e-4f)
            // The wall's transform moved its first corner to one metre along X.
            assertArrayEquals(floatArrayOf(1f, 0f, 0f), geometry.meshes[0].positions.copyOfRange(0, 3), 1e-4f)
        }
    }

    @Test fun sceneKitExportsReadWithCompressedIndicesAndMaterials() {
        val geometry = UsdReader.read(fixture("scene.usdz"))
        assertEquals(4, geometry.meshes.size)
        assertEquals(48, geometry.triangles)
        assertEquals(0f, geometry.min[1], 1e-3f)
        assertEquals(2.5f, geometry.max[1], 1e-3f)
        assertEquals(-2.05f, geometry.min[0], 1e-3f)
        assertEquals(2f, geometry.max[0], 1e-3f)
        assertEquals(2.05f, geometry.max[2], 1e-3f)
        // Walls are light grey through their UsdPreviewSurface, not the reader's fallback.
        assertArrayEquals(floatArrayOf(0.6666667f, 0.6666667f, 0.6666667f), geometry.meshes[0].color, 1e-4f)
        geometry.meshes.forEach { mesh -> assertEquals(mesh.positions.size, mesh.normals.size) }
    }

    @Test fun damagedOrForeignFilesAreRejected() {
        assertThrows(UsdFormatException::class.java) { UsdReader.read("not a model".toByteArray()) }
        val truncated = fixture("room.usdc").copyOf(400)
        assertThrows(Exception::class.java) { UsdReader.read(truncated) }
    }

    @Test fun unclosedPathsAndDeepNestingAreDamagedModels() {
        val layers = listOf("def Xform \"a\" { asset x = @foo }", "def Mesh \"a\" { rel material:binding = </Looks/Wall }",
            "def Mesh \"a\" { float[] x = " + "[".repeat(100_000) + " }", "def \"a\" {\n".repeat(100_000))
        for (layer in layers) assertThrows(UsdFormatException::class.java) { UsdReader.read("#usda 1.0\n$layer\n".toByteArray()) }
    }

    @Test fun crateCountsAreCheckedBeforeAllocating() {
        // Each count takes eight bytes of the file but would size an array of gigabytes.
        val layers = listOf(
            crate("TOKENS" to (longs(0x7FFFFFFF, 2, 4) + byteArrayOf(0, 0x20, 'a'.code.toByte(), 0))),
            crate(tokens, "STRINGS" to longs(0x7FFFFFFF)),
            crate(tokens, noStrings, "FIELDS" to (longs(1_000_000, 6) + byteArrayOf(0, 0x40, 0, 0, 0, 0))),
            crate(tokens, noStrings, noFields, noFieldSets, "PATHS" to longs(0x7FFFFFFF, 0, 0, 0, 0)))
        for (layer in layers) assertThrows(UsdFormatException::class.java) { UsdReader.read(layer) }
    }

    @Test(timeout = 10_000) fun repeatedPathsAndChildrenAreReadOnce() {
        // Every prim lists the same child twice; visited naively, 40 levels would take 2^40 visits.
        val layer = StringBuilder("#usda 1.0\n")
        repeat(40) { layer.append("def \"x\" {}\ndef \"x\" {\n") }
        layer.append("def Cube \"c\" {}\n").append("}\n".repeat(40))
        assertEquals(listOf("c"), UsdReader.read(layer.toString().toByteArray()).meshes.map { it.name })
        // Sixty crate paths whose every entry has both a child and a sibling at the next entry, which names each slot again.
        fun codes(vararg ones: Int) = ByteArray(15).also { c -> ones.forEach { c[it / 4] = (c[it / 4].toInt() or (1 shl (it % 4 * 2))).toByte() } }
        val indexes = ints(byteArrayOf(1, 0, 0, 0) + codes(0) + byteArrayOf(0))
        val elements = ints(ByteArray(4) + codes())
        val jumps = ints(ByteArray(4) + codes(0, 59) + byteArrayOf(1, -3))
        val table = crate(tokens, noStrings, noFields, noFieldSets, "PATHS" to (longs(60, 60) + indexes + elements + jumps))
        assertThrows(UsdFormatException::class.java) { UsdReader.read(table) }
    }

    @Test fun oversizedMeshesFailBeforeTheyAreExpanded() {
        // A face count alone implies 2,000,001 triangles, one over the limit; no vertex needs to be read to refuse it.
        val layer = "#usda 1.0\ndef Mesh \"m\" {\n    int[] faceVertexCounts = [2000003]\n    int[] faceVertexIndices = [0, 1, 2]\n    point3f[] points = [(0, 0, 0), (1, 0, 0), (0, 1, 0)]\n}\n"
        val error = assertThrows(UsdFormatException::class.java) { UsdReader.read(layer.toByteArray()) }
        assertEquals("This model has too many triangles to show.", error.message)
    }

    @Test fun integerAndLz4CodecsMatchUsd() {
        // Common delta 1, codes: common, int8 +3, int16 +300, int32 -100000.
        val raw = byteArrayOf(1, 0, 0, 0, 0b11_10_01_00.toByte(), 3, 0x2C, 0x01, 0x60, 0x79, 0xFE.toByte(), 0xFF.toByte())
        assertArrayEquals(intArrayOf(1, 4, 304, 304 - 100000), UsdIntegers.decode32(raw, 4))
        // An LZ4 block: literal "ab", then a four-byte match at distance two ("abab").
        val block = byteArrayOf(0x20, 'a'.code.toByte(), 'b'.code.toByte(), 2, 0)
        val out = ByteArray(6)
        assertEquals(6, Lz4.block(block, 0, block.size, out, 0))
        assertEquals("ababab", String(out))
    }
}
