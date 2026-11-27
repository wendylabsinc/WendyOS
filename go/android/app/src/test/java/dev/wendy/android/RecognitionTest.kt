package dev.wendy.android

import org.junit.Assert.*
import org.junit.Test
import org.opencv.core.Point

class RecognitionTest {
    @Test fun labelsMustMatchAWholeNameOfSixOrMoreCharacters() {
        val names = listOf("pi-lab-01", "Pi", "Jetson Orin")
        assertEquals(listOf(0), DeviceLabelMatcher.matches("  PI-LAB-01 ", names))
        assertEquals(listOf(2), DeviceLabelMatcher.matches("jetson   orin", names))
        assertTrue(DeviceLabelMatcher.matches("Pi", names).isEmpty())
        assertTrue(DeviceLabelMatcher.matches("pi-lab-01 rack", names).isEmpty())
    }

    @Test fun overlappingMatchesShareOneCard() {
        fun square(x: Double, id: String, device: String?) = DeviceVisualMatch(id, "Board", device, listOf(SightPoint(x, 0.1), SightPoint(x + 0.3, 0.1), SightPoint(x + 0.3, 0.4), SightPoint(x, 0.4)), 30, 1.0)
        val sights = DeviceSight.group(listOf(square(0.1, "a", "d1"), square(0.12, "b", "d2"), square(0.6, "c", null)))
        assertEquals(2, sights.size)
        assertEquals("Several devices match", sights[0].label)
        assertEquals(listOf("d1", "d2"), sights[0].deviceIDs)
        assertEquals("Hardware model match", sights[1].evidence)
    }

    @Test fun sightsNeedTwoConsistentFrames() {
        val sight = DeviceSight("a", "Board", emptyList(), listOf(SightPoint(0.4, 0.4), SightPoint(0.6, 0.6)), "Hardware model match")
        val confirmation = DeviceSightConfirmation()
        assertTrue(confirmation.accept(listOf(sight), 1.0).isEmpty())
        assertEquals(1, confirmation.accept(listOf(sight), 1.2).size)
        // A jump across the frame, or a stale gap, is not confirmation.
        val moved = sight.copy(corners = listOf(SightPoint(0.0, 0.0), SightPoint(0.1, 0.1)))
        assertTrue(confirmation.accept(listOf(moved), 1.3).isEmpty())
        assertTrue(confirmation.accept(listOf(moved), 4.0).isEmpty())
    }

    @Test fun polygonChecksMatchOpenCv() {
        val square = listOf(Point(0.0, 0.0), Point(10.0, 0.0), Point(10.0, 10.0), Point(0.0, 10.0))
        assertEquals(100.0, RecognitionGeometry.area(square), 1e-9)
        assertTrue(RecognitionGeometry.convex(square))
        assertFalse(RecognitionGeometry.convex(listOf(Point(0.0, 0.0), Point(10.0, 10.0), Point(10.0, 0.0), Point(0.0, 10.0))))
        val hull = RecognitionGeometry.hull(square + Point(5.0, 5.0))
        assertEquals(4, hull.size)
        assertEquals(100.0, kotlin.math.abs(RecognitionGeometry.area(hull)), 1e-9)
    }
}
