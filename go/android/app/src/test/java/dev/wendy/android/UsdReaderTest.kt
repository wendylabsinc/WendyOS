package dev.wendy.android

import org.junit.Assert.*
import org.junit.Test

/** Fixtures were written by Apple's usdcat and usdzip, and by SceneKit's USDZ export, on macOS. */
class UsdReaderTest {
    private fun fixture(name: String) = javaClass.classLoader!!.getResourceAsStream("sites/$name")!!.readBytes()

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
