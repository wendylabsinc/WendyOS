package dev.wendy.android

import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.util.Base64
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.material3.*
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.FilterQuality
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.text.TextMeasurer
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.drawText
import androidx.compose.ui.text.rememberTextMeasurer
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.viewinterop.AndroidView
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import org.json.JSONObject
import java.nio.ByteBuffer

@OptIn(ExperimentalMaterial3Api::class)
@Composable fun CamerasScreen(model: WendyModel) {
    val scope = rememberCoroutineScope()
    var refreshing by remember { mutableStateOf(false) }
    PullToRefreshBox(refreshing, onRefresh = { scope.launch { refreshing = true; model.refreshDetails(); refreshing = false } }) {
        PageColumn {
            model.camerasError?.let { item { FormSection { FailureNote(it) } } }
            if (model.cameras.isEmpty()) item { EmptyMessage("No cameras found", "Connect a camera or pair an IP camera in device settings.", Icons.Outlined.VideocamOff) }
            items(model.cameras, key = { "${it.number("id")}/${it.text("stable_id")}" }) { camera -> CameraTile(model, camera) }
        }
    }
}

/** Measured temperatures for a thermal frame, using the same rules as iOS ThermalCameraFrame. */
class ThermalOverlay(frame: JSONObject, private val receivedAt: Long) {
    data class Point(val x: Int, val y: Int, val label: String?)
    val width = frame.number("Width").toInt()
    val height = frame.number("Height").toInt()
    private val temperature = frame.optJSONObject("Temperature")
    private val summary = frame.optJSONObject("Summary")
    private val summaryUpdated = frame.number("SummaryUpdatedNS")
    private fun raw(point: JSONObject?): Point? {
        if (point == null) return null
        val raw = point.number("Raw")
        val label = temperature?.let { "%.1f°C".format(raw * it.optDouble("kelvin_per_count") + it.optDouble("kelvin_offset") - 273.15) }
        return Point(point.number("X").toInt(), point.number("Y").toInt(), label)
    }
    /** A device summary is used only while it is under three seconds old. */
    private val summaryFresh get() = summary != null && summaryUpdated > 0 && (System.currentTimeMillis() - summaryUpdated / 1_000_000) in -1000..2999
    val coldest: Point? get() = if (summaryFresh) summary?.optJSONObject("Coldest")?.let { Point(it.number("X").toInt(), it.number("Y").toInt(), "%.1f°C".format(summary.optDouble("ColdCelsius"))) } else raw(frameCold)
    val hottest: Point? get() = if (summaryFresh) summary?.optJSONObject("Hottest")?.let { Point(it.number("X").toInt(), it.number("Y").toInt(), "%.1f°C".format(summary.optDouble("HotCelsius"))) } else raw(frameHot)
    private val frameCold = frame.optJSONObject("Coldest")
    private val frameHot = frame.optJSONObject("Hottest")
    fun fresh(now: Long) = now - receivedAt < 3000
}

/**
 * One live camera. The tile owns its stream: it starts when shown or played and
 * stops when paused, scrolled away, or when the device session ends.
 */
@Composable fun CameraTile(model: WendyModel, camera: JSONObject, compact: Boolean = false, onOpen: (() -> Unit)? = null) {
    val session = model.session
    val id = camera.number("id")
    var running by remember(session, id) { mutableStateOf(true) }
    var stream by remember(session, id) { mutableLongStateOf(0L) }
    var player by remember { mutableStateOf<CameraPlayer?>(null) }
    var bitmap by remember { mutableStateOf<Bitmap?>(null) }
    var thermal by remember { mutableStateOf<ThermalOverlay?>(null) }
    var error by remember { mutableStateOf<String?>(null) }
    var received by remember { mutableStateOf(false) }
    var showingError by remember { mutableStateOf(false) }
    // Thermal markers fade when frames stop, so the tile re-evaluates freshness each second.
    var now by remember { mutableLongStateOf(System.currentTimeMillis()) }
    LaunchedEffect(thermal != null) { while (thermal != null) { kotlinx.coroutines.delay(1000); now = System.currentTimeMillis() } }
    // A new stream ID per play keeps late frames from a paused stream out of the view.
    val visible = LocalPageVisible.current
    LaunchedEffect(running, session, id, visible) {
        if (!running || session == 0L || !visible) { stream = 0L; return@LaunchedEffect }
        val next = model.nextID()
        error = null; received = false
        stream = next
        try { model.backend.call("StartCamera", id, next, session, camera.text("stable_id")) } catch (e: Exception) { if (stream == next) error = e.message }
    }
    DisposableEffect(stream) {
        val current = stream
        if (current != 0L) model.videoConsumers[current] = { value -> if (value.number("Kind") == 6L) { received = true; player?.append(Base64.decode(value.text("Bytes"), Base64.DEFAULT)) } }
        onDispose {
            if (current != 0L) {
                model.videoConsumers.remove(current); model.cameraEvents.remove(current)
                model.task { model.backend.call("StopCamera", id, current, session) }
            }
        }
    }
    DisposableEffect(Unit) { onDispose { player?.close() } }
    val event = model.cameraEvents[stream]
    LaunchedEffect(event) {
        if (event == null) return@LaunchedEffect
        when (event.number("Kind").toInt()) {
            7 -> error = event.text("Text")
            8 -> { received = true; bitmap = withContext(Dispatchers.Default) { val bytes = Base64.decode(event.text("Bytes"), Base64.DEFAULT); BitmapFactory.decodeByteArray(bytes, 0, bytes.size) } }
            9 -> {
                val frame = event.child("Thermal")
                val decoded = withContext(Dispatchers.Default) {
                    val width = frame.number("Width").toInt(); val height = frame.number("Height").toInt()
                    val bytes = Base64.decode(frame.text("RGBA"), Base64.DEFAULT)
                    if (width in 1..4096 && height in 1..4096 && bytes.size.toLong() == width.toLong() * height * 4) Bitmap.createBitmap(width, height, Bitmap.Config.ARGB_8888).apply { copyPixelsFromBuffer(ByteBuffer.wrap(bytes)) } else null
                }
                if (decoded == null) error = "The camera sent an invalid thermal frame." else { received = true; bitmap = decoded; thermal = ThermalOverlay(frame, System.currentTimeMillis()) }
            }
        }
    }
    val name = camera.text("name").ifEmpty { "Camera" }
    val shape = RoundedCornerShape(if (compact) 24.dp else 16.dp)
    Box(Modifier.fillMaxWidth().aspectRatio(4f / 3f).clip(shape).background(Color.Black).let { if (onOpen != null) it.clickable(onClick = onOpen) else it }) {
        AndroidView(factory = { context -> CameraPlayer(context) { error = it }.also { player = it } }, modifier = Modifier.fillMaxSize())
        bitmap?.let { image ->
            Image(image.asImageBitmap(), contentDescription = name, modifier = Modifier.fillMaxSize(), contentScale = ContentScale.Fit,
                filterQuality = if (thermal != null) FilterQuality.None else FilterQuality.Low, alpha = if (thermal?.fresh(now) == false) 0.35f else 1f)
        }
        thermal?.let { if (running && it.fresh(now)) ThermalMarkers(it) }
        Row(Modifier.align(Alignment.BottomCenter).fillMaxWidth().background(Brush.verticalGradient(listOf(Color.Transparent, Color.Black.copy(alpha = .8f)))).padding(12.dp).padding(top = 24.dp),
            verticalAlignment = Alignment.Bottom, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
            Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                Text(name, color = Color.White, style = if (compact) MaterialTheme.typography.titleMedium else MaterialTheme.typography.titleSmall, maxLines = 2)
                val failure = error
                if (failure != null) Row(Modifier.clickable { showingError = true }, horizontalArrangement = Arrangement.spacedBy(6.dp), verticalAlignment = Alignment.CenterVertically) {
                    Icon(Icons.Outlined.WarningAmber, null, Modifier.size(14.dp), tint = Color.White)
                    Text(PresentedFailure(failure).message, color = Color.White, style = MaterialTheme.typography.bodySmall, maxLines = 3)
                } else if (!running || !received) Text(if (running) "Connecting…" else "Paused", color = Color.White.copy(alpha = .8f), style = MaterialTheme.typography.bodySmall)
                else if (compact) Text("Live", color = Color.White, style = MaterialTheme.typography.bodySmall)
            }
            if (!compact) IconButton(onClick = { running = !running; if (!running) { error = null; received = false } },
                modifier = Modifier.size(44.dp).background(Color.Black.copy(alpha = .5f), CircleShape)) {
                Icon(if (running) Icons.Outlined.Pause else Icons.Outlined.PlayArrow, "${if (running) "Pause" else "Play"} $name", tint = Color.White)
            }
        }
    }
    if (showingError) DetailsDialog("Camera error", error ?: "No current error") { showingError = false }
}

@Composable
private fun ThermalMarkers(overlay: ThermalOverlay) {
    val measurer = rememberTextMeasurer()
    Canvas(Modifier.fillMaxSize()) {
        if (overlay.width <= 0 || overlay.height <= 0) return@Canvas
        val scale = minOf(size.width / overlay.width, size.height / overlay.height)
        fun position(p: ThermalOverlay.Point) = Offset((size.width - overlay.width * scale) / 2 + (p.x + 0.5f) * scale, (size.height - overlay.height * scale) / 2 + (p.y + 0.5f) * scale)
        listOfNotNull(overlay.coldest?.let { Triple(it, "MIN", Color(0xFF4FD8EB)) }, overlay.hottest?.let { Triple(it, "MAX", Color(0xFFFF6B4A)) }).forEach { (point, label, color) ->
            val center = position(point)
            drawCircle(Color.Black.copy(alpha = .6f), 9f, center, style = Stroke(5f))
            drawCircle(color, 9f, center, style = Stroke(3f))
            drawThermalLabel(measurer, "$label ${point.label ?: ""}".trim(), center, color)
        }
    }
}

private fun androidx.compose.ui.graphics.drawscope.DrawScope.drawThermalLabel(measurer: TextMeasurer, text: String, center: Offset, color: Color) {
    val layout = measurer.measure(text, TextStyle(color = Color.White, fontSize = 11.sp))
    val x = (center.x + if (center.x < size.width / 2) 14f else -14f - layout.size.width).coerceIn(4f, size.width - layout.size.width - 4f)
    val y = (center.y - layout.size.height / 2f).coerceIn(4f, size.height - layout.size.height - 4f)
    drawRoundRect(Color.Black.copy(alpha = .65f), Offset(x - 6, y - 3), androidx.compose.ui.geometry.Size(layout.size.width + 12f, layout.size.height + 6f), androidx.compose.ui.geometry.CornerRadius(8f))
    drawRect(color, Offset(x - 6, y - 3), androidx.compose.ui.geometry.Size(3f, layout.size.height + 6f))
    drawText(layout, topLeft = Offset(x, y))
}
