package dev.wendy.android

import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test

class RecordingTest {
    private val bounds = TimelineViewport.fitted(0.0, 10.0)

    @Test fun zoomKeepsTheAnchorTimeFixedAndStaysInBounds() {
        val zoomed = bounds.zoomed(2.0, 0.25, bounds)
        assertEquals(5.0, zoomed.span, 1e-9)
        assertEquals(2.5, zoomed.time(0.25), 1e-9)
        assertEquals(bounds, bounds.zoomed(0.5, 0.5, bounds))
        // Zoom never goes below one millisecond.
        assertEquals(0.001, bounds.zoomed(1e9, 0.5, bounds).span, 1e-9)
        assertEquals(bounds, bounds.zoomed(Double.NaN, 0.5, bounds))
    }

    @Test fun panAndRevealClampToTheEpisode() {
        val window = TimelineViewport(2.0, 4.0)
        assertEquals(TimelineViewport(8.0, 10.0), window.panned(100.0, bounds))
        assertEquals(TimelineViewport(0.0, 2.0), window.panned(-100.0, bounds))
        assertEquals(window, window.revealing(3.0, bounds))
        val revealed = window.revealing(9.0, bounds)
        assertTrue(9.0 in revealed.start..revealed.end)
        assertEquals(2.0, revealed.span, 1e-9)
    }

    @Test fun upperBoundFindsLatestSample() {
        val times = doubleArrayOf(0.0, 1.0, 1.0, 2.5)
        assertEquals(0, upperBound(times, -1.0))
        assertEquals(3, upperBound(times, 1.0))
        assertEquals(4, upperBound(times, 9.0))
    }

    @Test fun snapshotReadsTracksSamplesMediaAndPoints() {
        val snapshot = RecordingSnapshot(JSONObject("""{
            "Start":0,"End":4000000000,
            "Tracks":[{"ID":"lidar","Name":"Lidar","Kind":"lidar"},{"ID":"temp","Name":"Temperature","Kind":"numeric"}],
            "Samples":[{"Track":"temp","Time":2000000000,"Value":21.5,"HasValue":true,"Unit":"C"},{"Track":"temp","Time":1000000000,"Value":20,"HasValue":true},
                       {"Track":"lidar","Time":500000000,"Points":[{"X":1,"Y":2,"Z":3},{"X":-1,"Y":0,"Z":0.5}]}],
            "Videos":[{"Track":"cam","Path":"/tmp/a.mp4","Times":[1000000000,2000000000]}],
            "Audio":[{"Track":"mic","Path":"/tmp/a.wav","Start":0,"End":3000000000}],
            "Warnings":["clipped"]
        }"""))
        assertEquals(4.0, snapshot.end, 0.0)
        assertEquals(listOf(20.0, 21.5), snapshot.numeric("temp").map { it.value })
        assertEquals(20.0, snapshot.latest("temp", 1.5)!!.value!!, 0.0)
        assertNull(snapshot.latest("temp", 0.5))
        assertArrayEquals(floatArrayOf(1f, 2f, 3f, -1f, 0f, 0.5f), snapshot.latest("lidar", 1.0)!!.points, 0f)
        assertEquals(1.0, snapshot.videos.single().frame(1.5)!!, 0.0)
        assertNull(snapshot.videos.single().frame(0.5))
        assertEquals(3.0, snapshot.audio.single().end, 0.0)
        assertEquals(listOf("/tmp/a.mp4", "/tmp/a.wav"), snapshot.mediaPaths)
        assertEquals("clipped", snapshot.warnings)
    }
}
