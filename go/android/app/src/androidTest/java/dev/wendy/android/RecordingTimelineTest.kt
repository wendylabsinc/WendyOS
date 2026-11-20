package dev.wendy.android

import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.test.*
import androidx.compose.ui.test.junit4.createComposeRule
import androidx.compose.ui.unit.dp
import androidx.test.ext.junit.runners.AndroidJUnit4
import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Rule
import org.junit.Test
import org.junit.runner.RunWith

@RunWith(AndroidJUnit4::class)
class RecordingTimelineTest {
    @get:Rule val compose = createComposeRule()

    private val snapshot = RecordingSnapshot(JSONObject("""{"Start":0,"End":10000000000,
        "Tracks":[{"ID":"temp","Name":"Temperature","Kind":"numeric"}],
        "Samples":[{"Track":"temp","Time":1000000000,"Value":1,"HasValue":true},{"Track":"temp","Time":9000000000,"Value":2,"HasValue":true}]}"""))

    private fun show(): Pair<MutableState<TimelineViewport>, MutableState<Double>> {
        val viewport = mutableStateOf(TimelineViewport.fitted(0.0, 10.0))
        val cursor = mutableStateOf(0.0)
        compose.setContent {
            WendyTheme { RecordingTimeline(snapshot, cursor.value, { cursor.value = it }, false, {}, viewport.value) { viewport.value = it } }
        }
        return viewport to cursor
    }

    @Test fun zoomButtonsAndFitChangeTheWindow() {
        val (viewport, _) = show()
        compose.onNodeWithContentDescription("Zoom in").performClick()
        compose.runOnIdle { assertEquals(5.0, viewport.value.span, 1e-6) }
        compose.onNodeWithText("Fit").performClick()
        compose.runOnIdle { assertEquals(10.0, viewport.value.span, 1e-6) }
    }

    @Test fun pinchZoomsAndRulerDragPans() {
        val (viewport, _) = show()
        compose.onRoot().performTouchInput { pinch(center - Offset(40f, 0f), center - Offset(200f, 0f), center + Offset(40f, 0f), center + Offset(200f, 0f)) }
        compose.runOnIdle { assertTrue("pinch should zoom in: ${viewport.value}", viewport.value.span < 9.0) }
        val before = viewport.value
        compose.onNodeWithText("Drag ruler to pan").performTouchInput { swipeLeft() }
        compose.runOnIdle { assertTrue("drag should pan later: $before -> ${viewport.value}", viewport.value.start > before.start) }
    }

    @Test fun tappingATrackSeeksWithinTheWindow() {
        val (_, cursor) = show()
        compose.onNodeWithContentDescription("Temperature timeline").performTouchInput { click(Offset(width * 0.75f, height / 2f)) }
        compose.runOnIdle { assertEquals(7.5, cursor.value, 0.3) }
    }

    @Test fun pointCloudHandlesGestures() {
        val points = FloatArray(3000) { (it % 7) * 0.1f }
        compose.setContent { WendyTheme { PointCloudView(points, Modifier.fillMaxWidth().height(240.dp)) } }
        compose.onRoot().performTouchInput { swipeLeft(); pinch(center - Offset(20f, 0f), center - Offset(120f, 0f), center + Offset(20f, 0f), center + Offset(120f, 0f)); doubleClick() }
        compose.waitForIdle()
    }
}
