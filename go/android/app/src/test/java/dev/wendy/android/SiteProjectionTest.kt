package dev.wendy.android

import org.junit.Assert.*
import org.junit.Test

class SiteProjectionTest {
    private val revision = Site(name = "Room").modelRevision
    /** Looking along −z from (0, 1.5, 0): manual yaw 180, roll 180. */
    private fun pose(x: Double = 0.0, z: Double = 0.0) = SiteManualAlignment(revision, 180.0, 0.0, 180.0, 70.0, 640, 480).geometry(SitePoint(x, 1.5, z), revision, 0)!!

    private fun apply(m: FloatArray, p: FloatArray) = FloatArray(4) { row -> (0..3).sumOf { col -> (m[col * 4 + row] * p[col]).toDouble() }.toFloat() }

    @Test fun worldToCameraPutsTheCameraAtTheOrigin() {
        val g = pose(0.5, 1.0)
        val w = SiteProjection.worldToCamera(g.pose)
        assertArrayEquals(floatArrayOf(0f, 0f, 0f, 1f), apply(w, floatArrayOf(0.5f, 1.5f, 1f, 1f)), 1e-5f)
        // Two metres ahead is +z; above the camera is −y (optical down is +y).
        assertArrayEquals(floatArrayOf(0f, 0f, 2f, 1f), apply(w, floatArrayOf(0.5f, 1.5f, -1f, 1f)), 1e-5f)
        assertEquals(-1f, apply(w, floatArrayOf(0.5f, 2.5f, 1f, 1f))[1], 1e-5f)
    }

    @Test fun depthProjectionCoversNearToFarAndTheLens() {
        val lens = pose().lens!!
        val extent = SiteProjection.extent(lens)!!
        assertEquals(kotlin.math.tan(Math.toRadians(35.0)) * 1.02, extent[0].toDouble(), 1e-3)
        val p = SiteProjection.depthProjection(extent[0], extent[1], 0.01f, 10f)
        fun ndc(x: Float, y: Float, z: Float) = apply(p, floatArrayOf(x, y, z, 1f)).let { floatArrayOf(it[0] / it[3], it[1] / it[3], it[2] / it[3]) }
        assertEquals(-1f, ndc(0f, 0f, 0.01f)[2], 1e-4f); assertEquals(1f, ndc(0f, 0f, 10f)[2], 1e-4f)
        assertEquals(1f / 1.02f, ndc(extent[0] / 1.02f * 3, 0f, 3f)[0], 1e-4f)
    }

    private fun wall(name: String, x0: Float, x1: Float, z0: Float, z1: Float): SiteMesh {
        // A box 3 m tall; only its extent matters to the rule.
        val corners = listOf(floatArrayOf(x0, 0f, z0), floatArrayOf(x1, 0f, z0), floatArrayOf(x1, 3f, z1), floatArrayOf(x0, 3f, z1))
        val positions = listOf(0, 1, 2, 0, 2, 3).flatMap { corners[it].toList() }.toFloatArray()
        return SiteMesh(name, positions, FloatArray(positions.size), floatArrayOf(.5f, .5f, .5f))
    }

    @Test fun onlyTheMountingWallIsLeftOutOfTheDepthView() {
        // The camera hangs on the wall at z ≈ 0 (10 cm thick) and looks into the room along −z.
        val geometry = SiteGeometry(listOf(wall("Wall0", -3f, 3f, -0.05f, 0.05f), wall("Wall1", -3f, 3f, -4.05f, -3.95f), wall("Table", -3f, 3f, -0.05f, 0.05f)))
        assertEquals(setOf(0), SiteProjection.mountingWalls(geometry, pose().pose))
        // Away from any wall, nothing is left out.
        assertEquals(emptySet<Int>(), SiteProjection.mountingWalls(geometry, pose(z = -2.0).pose))
        assertEquals("Live camera color · Manually aligned · Mounting wall overlap", SiteProjection.status(pose(), true))
        assertTrue(SiteProjection.far(geometry, pose().pose) > 5f)
    }
}
