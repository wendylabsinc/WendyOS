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

/** Live tiles, full screen on tap, and each camera's controls (iOS CameraWallView). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable fun CamerasScreen(model: WendyModel) {
    val nav = LocalNavigator.current
    val scope = rememberCoroutineScope()
    var refreshing by remember { mutableStateOf(false) }
    var fullscreen by remember { mutableStateOf<JSONObject?>(null) }
    val session = model.session
    LaunchedEffect(session) { fullscreen = null }
    LaunchedEffect(model.cameras) { fullscreen?.let { current -> if (model.cameras.none { cameraKey(it) == cameraKey(current) }) fullscreen = null } }
    PullToRefreshBox(refreshing, onRefresh = { scope.launch { refreshing = true; model.refreshDetails(); refreshing = false } }) {
        PageColumn {
            model.camerasError?.let { item { FormSection { FailureNote(it) } } }
            if (model.cameras.isEmpty() && model.camerasError == null && model.detailsSession != session) item { Surface(shape = RoundedCornerShape(22.dp), color = MaterialTheme.colorScheme.surface) { LensLoading("Loading cameras…") } }
            else if (model.cameras.isEmpty() && model.camerasError == null) item { EmptyMessage("No cameras attached", "Connect a camera or pair an IP camera in device settings.", Icons.Outlined.VideocamOff) }
            // A full-screen camera owns the stream, so the wall pauses its tile meanwhile.
            items(model.cameras, key = { cameraKey(it) }) { camera -> if (fullscreen?.let(::cameraKey) != cameraKey(camera)) CameraTile(model, camera, onOpen = { fullscreen = camera }, onExpand = { fullscreen = camera }) else Box(Modifier.fillMaxWidth().aspectRatio(4f / 3f).clip(RoundedCornerShape(16.dp)).background(Color.Black)) }
            if (model.cameras.isNotEmpty()) item {
                LensSection("Camera controls") {
                    model.cameras.forEachIndexed { index, camera ->
                        if (index > 0) HorizontalDivider(Modifier.padding(start = 70.dp), color = MaterialTheme.colorScheme.outlineVariant)
                        val name = camera.text("name").ifEmpty { "Camera" }
                        Row(Modifier.fillMaxWidth().clickable(onClickLabel = "Settings for $name") { nav.push(name) { CameraSettingsPage(model, camera, session) } }.padding(16.dp),
                            horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
                            Box(Modifier.size(42.dp).background(Color(0xFF32ADE6), RoundedCornerShape(11.dp)), contentAlignment = Alignment.Center) { Icon(Icons.Outlined.Videocam, null, tint = Color.White) }
                            Text(name, Modifier.weight(1f), style = MaterialTheme.typography.bodyLarge, fontWeight = androidx.compose.ui.text.font.FontWeight.SemiBold, maxLines = 2)
                            Icon(Icons.Outlined.ChevronRight, null, Modifier.size(16.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
                        }
                    }
                }
            }
        }
    }
    fullscreen?.let { camera ->
        androidx.compose.ui.window.Dialog(onDismissRequest = { fullscreen = null }, properties = androidx.compose.ui.window.DialogProperties(usePlatformDefaultWidth = false)) {
            Box(Modifier.fillMaxSize().background(Color.Black), contentAlignment = Alignment.Center) {
                CameraTile(model, camera)
                IconButton(onClick = { fullscreen = null }, Modifier.align(Alignment.TopEnd).windowInsetsPadding(WindowInsets.systemBars).padding(12.dp).size(44.dp).background(Color.Black.copy(alpha = .6f), CircleShape)) {
                    Icon(Icons.Outlined.Close, "Close full screen", tint = Color.White)
                }
            }
        }
    }
}

private fun cameraKey(camera: JSONObject) = "${camera.number("id")}/${camera.text("stable_id")}"

data class CameraControl(val name: String, val value: Int, val minimum: Int, val maximum: Int, val step: Int, val mutable: Boolean) {
    val title get() = name.replace('_', ' ').split(' ').joinToString(" ") { word -> word.lowercase().replaceFirstChar { it.uppercase() } }
    val adjustable get() = mutable && minimum < maximum
    /** In range and on a step from the minimum, as the device validates it. */
    fun accepts(candidate: Int) = candidate in minimum..maximum && (candidate.toLong() - minimum) % maxOf(1, step) == 0L
    companion object {
        fun parse(value: JSONObject) = CameraControl(value.text("name", "Name"), value.number("value", "Value").toInt(), value.number("minimum", "Minimum").toInt(),
            value.number("maximum", "Maximum").toInt(), value.number("step", "Step").toInt(), value.flag("mutable", "Mutable"))
    }
}

/** Resolution and the camera's adjustable controls; changes apply now and reset on restart (iOS MobileCameraSettings). */
@Composable
private fun CameraSettingsPage(model: WendyModel, camera: JSONObject, session: Long) {
    val nav = LocalNavigator.current
    val scope = rememberCoroutineScope()
    var controls by remember { mutableStateOf(emptyList<CameraControl>()) }
    var loading by remember { mutableStateOf(true) }
    var saving by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    val current = model.session == session && session != 0L && model.cameras.any { cameraKey(it) == cameraKey(camera) }
    LaunchedEffect(model.session) { if (model.session != session) nav.pop() }
    suspend fun load(name: String = "", value: Int = 0) {
        if (!current || saving) return
        saving = true
        try {
            val result = model.backend.call("CameraControls", session, camera.number("id"), camera.text("stable_id"), name, value).items().map(CameraControl::parse)
            if (model.session == session) { controls = result; error = null }
        } catch (e: kotlinx.coroutines.CancellationException) { throw e } catch (e: Exception) { if (model.session == session) error = PresentedFailure(e.message ?: "").message }
        finally { saving = false; loading = false }
    }
    LaunchedEffect(Unit) { load() }
    PageColumn {
        formSection("Stream") {
            LabeledRow("Resolution", camera.text("resolution").ifEmpty { "Shown on the live tile" })
            SecondaryText("Resolution can’t be changed here.")
        }
        formSection("Camera controls") {
            when {
                loading && controls.isEmpty() && error == null -> LoadingRow("Loading controls…")
                error != null -> { FailureNote(error!!); TextButton(onClick = { scope.launch { load() } }) { Text("Retry") } }
                controls.isEmpty() -> SecondaryText("This camera does not expose adjustable controls.")
                else -> {
                    controls.forEach { control -> CameraControlRow(control, saving || !current) { value -> scope.launch { load(control.name, value) } } }
                    SecondaryText("Changes apply immediately and reset when the device restarts.")
                }
            }
        }
    }
}

@Composable
private fun CameraControlRow(control: CameraControl, disabled: Boolean, save: (Int) -> Unit) {
    var value by remember(control.value) { mutableStateOf(control.value.toString()) }
    Column(Modifier.padding(vertical = 8.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
        Row(verticalAlignment = Alignment.CenterVertically) {
            Text(control.title, Modifier.weight(1f), style = MaterialTheme.typography.titleSmall)
            Text("Current ${control.value}", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        if (control.adjustable) {
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
                OutlinedTextField(value, { value = it.filter { ch -> ch.isDigit() || ch == '-' } }, Modifier.weight(1f), singleLine = true, label = { Text(control.title) },
                    keyboardOptions = androidx.compose.foundation.text.KeyboardOptions(keyboardType = androidx.compose.ui.text.input.KeyboardType.Number))
                val parsed = value.toIntOrNull()
                TextButton(onClick = { parsed?.let(save) }, enabled = !disabled && parsed != null && control.accepts(parsed) && parsed != control.value) { Text("Apply") }
            }
            SecondaryText("Range ${control.minimum}…${control.maximum}, step ${maxOf(1, control.step)}")
        } else LabeledRow("Read only", control.value.toString())
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
@Composable fun CameraTile(model: WendyModel, camera: JSONObject, compact: Boolean = false, onOpen: (() -> Unit)? = null, onExpand: (() -> Unit)? = null) {
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
    var retry by remember { mutableIntStateOf(0) }
    var resolution by remember { mutableStateOf<String?>(null) }
    var lastReceived by remember { mutableLongStateOf(0L) }
    var clock by remember { mutableLongStateOf(System.currentTimeMillis()) }
    LaunchedEffect(running) { while (running) { kotlinx.coroutines.delay(1000); clock = System.currentTimeMillis() } }
    // Thermal markers fade when frames stop, so the tile re-evaluates freshness each second.
    var now by remember { mutableLongStateOf(System.currentTimeMillis()) }
    LaunchedEffect(thermal != null) { while (thermal != null) { kotlinx.coroutines.delay(1000); now = System.currentTimeMillis() } }
    // A new stream ID per play keeps late frames from a paused stream out of the view.
    val visible = LocalPageVisible.current
    LaunchedEffect(running, session, id, visible, retry) {
        if (!running || session == 0L || !visible) { stream = 0L; return@LaunchedEffect }
        val next = model.nextID()
        error = null; received = false
        stream = next
        try { model.backend.call("StartCamera", id, next, session, camera.text("stable_id")) } catch (e: Exception) { if (stream == next) error = e.message }
    }
    DisposableEffect(stream) {
        val current = stream
        if (current != 0L) model.videoConsumers[current] = { value -> if (value.number("Kind") == 6L) { received = true; lastReceived = System.currentTimeMillis(); player?.append(Base64.decode(value.text("Bytes"), Base64.DEFAULT)) } }
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
            8 -> { received = true; lastReceived = System.currentTimeMillis(); bitmap = withContext(Dispatchers.Default) { val bytes = Base64.decode(event.text("Bytes"), Base64.DEFAULT); BitmapFactory.decodeByteArray(bytes, 0, bytes.size) }; bitmap?.let { resolution = "${it.width}×${it.height}" } }
            9 -> {
                val frame = event.child("Thermal")
                val decoded = withContext(Dispatchers.Default) {
                    val width = frame.number("Width").toInt(); val height = frame.number("Height").toInt()
                    val bytes = Base64.decode(frame.text("RGBA"), Base64.DEFAULT)
                    if (width in 1..4096 && height in 1..4096 && bytes.size.toLong() == width.toLong() * height * 4) Bitmap.createBitmap(width, height, Bitmap.Config.ARGB_8888).apply { copyPixelsFromBuffer(ByteBuffer.wrap(bytes)) } else null
                }
                if (decoded == null) error = "The camera sent an invalid thermal frame." else { received = true; lastReceived = System.currentTimeMillis(); resolution = "${decoded.width}×${decoded.height}"; bitmap = decoded; thermal = ThermalOverlay(frame, System.currentTimeMillis()) }
            }
        }
    }
    val name = camera.text("name").ifEmpty { "Camera" }
    val shape = RoundedCornerShape(if (compact) 24.dp else 16.dp)
    Box(Modifier.fillMaxWidth().aspectRatio(4f / 3f).clip(shape).background(Color.Black).let { if (onOpen != null) it.clickable(onClick = onOpen) else it }) {
        AndroidView(factory = { context -> CameraPlayer(context, { error = it }, { w, h -> resolution = "$w×$h" }).also { player = it } }, modifier = Modifier.fillMaxSize())
        bitmap?.let { image ->
            Image(image.asImageBitmap(), contentDescription = name, modifier = Modifier.fillMaxSize(), contentScale = ContentScale.Fit,
                filterQuality = if (thermal != null) FilterQuality.None else FilterQuality.Low, alpha = if (thermal?.fresh(now) == false) 0.35f else 1f)
        }
        thermal?.let { if (running && it.fresh(now)) ThermalMarkers(it) }
        if (running && !received && error == null) Row(Modifier.align(Alignment.Center), horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
            CircularProgressIndicator(Modifier.size(18.dp), color = Color.White, strokeWidth = 2.dp); Text("Waiting for video…", color = Color.White, style = MaterialTheme.typography.bodySmall)
        }
        if (!running) Icon(if (error == null) Icons.Outlined.Pause else Icons.Outlined.VideocamOff, null, Modifier.align(Alignment.Center).size(40.dp), tint = Color.White.copy(alpha = .6f))
        Column(Modifier.align(Alignment.BottomCenter).fillMaxWidth().background(Brush.verticalGradient(listOf(Color.Transparent, Color.Black.copy(alpha = .8f)))).padding(12.dp).padding(top = 24.dp),
            verticalArrangement = Arrangement.spacedBy(6.dp)) {
        Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.Bottom, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
            Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                Text(name, color = Color.White, style = if (compact) MaterialTheme.typography.titleMedium else MaterialTheme.typography.titleSmall, maxLines = 2)
                val failure = error
                if (failure != null) Row(Modifier.clickable { showingError = true }, horizontalArrangement = Arrangement.spacedBy(6.dp), verticalAlignment = Alignment.CenterVertically) {
                    Icon(Icons.Outlined.WarningAmber, null, Modifier.size(14.dp), tint = Color.White)
                    Text(PresentedFailure(failure).message, color = Color.White, style = MaterialTheme.typography.bodySmall, maxLines = 3)
                } else if (!running || !received) Text(if (running) "Connecting…" else "Paused", color = Color.White.copy(alpha = .8f), style = MaterialTheme.typography.bodySmall)
                else if (compact) Text("Live", color = Color.White, style = MaterialTheme.typography.bodySmall)
            }
            if (!compact) IconButton(onClick = {
                if (error != null) { error = null; received = false; running = true; retry++ } else { running = !running; if (!running) { error = null; received = false } }
            }, modifier = Modifier.size(44.dp).background(Color.Black.copy(alpha = .5f), CircleShape)) {
                Icon(if (error != null) Icons.Outlined.Refresh else if (running) Icons.Outlined.Pause else Icons.Outlined.PlayArrow, "${if (error != null) "Retry" else if (running) "Pause" else "Play"} $name", tint = Color.White)
            }
            if (onExpand != null) IconButton(onClick = onExpand, Modifier.size(44.dp)) { Icon(Icons.Outlined.OpenInFull, "Full screen $name", tint = Color.White) }
        }
        if (!compact && (resolution != null || lastReceived != 0L)) Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            resolution?.let { Text(it, color = Color.White.copy(alpha = .85f), fontSize = 10.sp) }
            Spacer(Modifier.weight(1f))
            if (lastReceived != 0L) {
                Text("Received ${java.text.DateFormat.getTimeInstance(java.text.DateFormat.MEDIUM).format(java.util.Date(lastReceived))}", color = Color.White.copy(alpha = .85f), fontSize = 10.sp)
                if (running && clock - lastReceived > 5000) Text("Stale", color = Color(0xFFFFD60A), fontSize = 10.sp)
            }
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
