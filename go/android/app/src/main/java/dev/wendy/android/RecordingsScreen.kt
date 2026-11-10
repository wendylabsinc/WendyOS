package dev.wendy.android

import android.content.Intent
import android.graphics.BitmapFactory
import android.media.MediaPlayer
import android.util.Base64
import android.widget.VideoView
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.gestures.detectDragGestures
import androidx.compose.foundation.gestures.detectTapGestures
import androidx.compose.foundation.gestures.detectTransformGestures
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.material.icons.automirrored.outlined.*
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.input.pointer.pointerInput
import androidx.compose.ui.layout.onSizeChanged
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.core.content.FileProvider
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import org.json.JSONArray
import org.json.JSONObject
import java.io.File
import java.text.DateFormat
import java.util.Date
import kotlin.math.*

/** The episode-relative window every timeline panel shares; panning never moves the cursor (iOS DataTimelineViewport). */
data class TimelineViewport(val start: Double, val end: Double) {
    val span get() = max(end - start, 0.000_001)
    fun clamped(bounds: TimelineViewport): TimelineViewport {
        val width = min(span, bounds.span)
        val left = min(max(start, bounds.start), bounds.end - width)
        return TimelineViewport(left, left + width)
    }
    fun panned(seconds: Double, bounds: TimelineViewport) = if (!seconds.isFinite()) this else TimelineViewport(start + seconds, end + seconds).clamped(bounds)
    fun zoomed(factor: Double, anchor: Double, bounds: TimelineViewport): TimelineViewport {
        if (!factor.isFinite() || factor <= 0 || !anchor.isFinite()) return this
        val position = anchor.coerceIn(0.0, 1.0)
        val fixed = start + span * position
        val width = min(max(span / factor, min(0.001, bounds.span)), bounds.span)
        return TimelineViewport(fixed - width * position, fixed + width * (1 - position)).clamped(bounds)
    }
    fun revealing(time: Double, bounds: TimelineViewport) = if (time in start..end) this else TimelineViewport(time - span * 0.8, time + span * 0.2).clamped(bounds)
    fun time(fraction: Double) = start + fraction.coerceIn(0.0, 1.0) * span
    companion object { fun fitted(start: Double, end: Double) = TimelineViewport(start, max(end, start + 0.000_001)) }
}

/** The index after the last value at or before [time], for sorted values. */
fun upperBound(values: DoubleArray, time: Double): Int {
    var low = 0; var high = values.size
    while (low < high) { val mid = (low + high) ushr 1; if (values[mid] <= time) low = mid + 1 else high = mid }
    return low
}

class RecordingSample(val track: String, val label: String, val detail: String, val unit: String, val seconds: Double, val value: Double?, val image: String, val points: FloatArray)
class RecordingVideo(val track: String, val path: String, val times: DoubleArray) { fun frame(cursor: Double) = upperBound(times, cursor).let { if (it == 0) null else times[it - 1] } }
class RecordingAudio(val track: String, val path: String, val start: Double, val end: Double)
data class RecordingTrack(val id: String, val name: String, val kind: String, val detail: String)

/** One loaded episode. Times are seconds relative to the episode. */
class RecordingSnapshot(value: JSONObject) {
    val start = value.number("Start") / 1e9
    val end = value.number("End") / 1e9
    val tracks = value.list("Tracks").map { RecordingTrack(it.text("ID"), it.text("Name", "ID"), it.text("Kind"), it.text("Detail")) }
    val samples: List<RecordingSample> = value.list("Samples").map { s ->
        val points = s.optJSONArray("Points")?.let { array -> FloatArray(array.length() * 3).also { out -> for (i in 0 until array.length()) array.optJSONObject(i)?.let { p -> out[i * 3] = p.optDouble("X").toFloat(); out[i * 3 + 1] = p.optDouble("Y").toFloat(); out[i * 3 + 2] = p.optDouble("Z").toFloat() } } } ?: FloatArray(0)
        RecordingSample(s.text("Track"), s.text("Label"), s.text("Detail"), s.text("Unit"), s.number("Time") / 1e9, if (s.flag("HasValue")) s.optDouble("Value") else null, s.text("Image"), points)
    }.sortedBy { it.seconds }
    val videos = value.list("Videos").map { v -> RecordingVideo(v.text("Track"), v.text("Path"), v.optJSONArray("Times")?.let { a -> DoubleArray(a.length()) { a.optLong(it) / 1e9 } } ?: DoubleArray(0)) }
    val audio = value.list("Audio").map { RecordingAudio(it.text("Track"), it.text("Path"), it.number("Start") / 1e9, it.number("End") / 1e9) }
    val warnings = value.optJSONArray("Warnings")?.let { a -> (0 until a.length()).joinToString("\n") { a.optString(it) } } ?: ""
    private val byTrack = samples.groupBy { it.track }
    val timestamps: Map<String, DoubleArray> = byTrack.mapValues { (_, values) -> DoubleArray(values.size) { values[it].seconds } }
    fun latest(track: String, cursor: Double): RecordingSample? {
        val times = timestamps[track] ?: return null
        val index = upperBound(times, cursor)
        return if (index == 0) null else byTrack.getValue(track)[index - 1]
    }
    fun numeric(track: String) = byTrack[track].orEmpty().filter { it.value != null }
    val mediaPaths get() = videos.map { it.path } + audio.map { it.path }
}

/** Manual recordings plus every episode on the device (iOS "All recordings"). Starting a recording is an Android addition. */
@Composable
fun RecordingsScreen(model: WendyModel) {
    val nav = LocalNavigator.current
    val active = rememberRemote(model, "VisualRecordingStatus", model.session)
    var revision by remember { mutableIntStateOf(0) }
    Column(Modifier.fillMaxSize()) {
        Row(Modifier.fillMaxWidth().padding(horizontal = 20.dp, vertical = 4.dp), verticalAlignment = Alignment.CenterVertically) {
            val recording = active.value.obj().text("id").isNotBlank()
            if (recording) {
                StatusPill("Recording ${active.value.obj().text("name")}".trim())
                Spacer(Modifier.weight(1f))
                TextButton(onClick = { model.task { model.backend.call("StopVisualRecording", model.session); active.refresh(); revision++ } }) { Text("Stop recording") }
            } else {
                Spacer(Modifier.weight(1f))
                TextButton(onClick = { nav.push("Start recording") { RecordingEditor(model) { nav.pop(); active.refresh(); revision++ } } }) { Icon(Icons.Outlined.FiberManualRecord, null, Modifier.size(16.dp)); Spacer(Modifier.width(6.dp)); Text("Start recording") }
            }
        }
        key(revision) { Box(Modifier.weight(1f)) { EpisodeViewer(model, null) } }
    }
}

@Composable private fun RecordingEditor(model: WendyModel, done: () -> Unit) {
    val sources = rememberRemote(model, "CampaignSources", model.session)
    var name by remember { mutableStateOf("") }
    val selected = remember { mutableStateMapOf<String, Boolean>() }
    PageColumn {
        item { RemoteStatus(sources) }
        formSection { OutlinedTextField(name, { name = it }, Modifier.fillMaxWidth(), label = { Text("Recording name") }, singleLine = true) }
        formSection("Sources") {
            sources.value.items().forEach { source -> ToggleRow(source.text("name", "id"), selected[source.text("id")] == true, enabled = source.flag("healthy") || !source.has("healthy")) { selected[source.text("id")] = it } }
            if (!sources.loading && sources.value.items().isEmpty()) SecondaryText("No sources discovered")
        }
        item {
            Button(enabled = selected.values.any { it }, onClick = { model.task { model.backend.call("StartVisualRecording", model.session, name, JSONArray(selected.filterValues { it }.keys.toList())); model.record("Started recording ${name.ifBlank { "on device" }}"); done() } },
                modifier = Modifier.fillMaxWidth()) { Text("Record") }
        }
    }
}

/** A recording with synced channels (iOS DataVisualizerView on iPhone), plus audio playback and point clouds. */
@Composable
fun EpisodeScreen(model: WendyModel, episode: String) = EpisodeViewer(model, episode)

@Composable
fun EpisodeViewer(model: WendyModel, initialEpisode: String?) {
    val context = LocalContext.current
    val session = model.session
    var episodes by remember { mutableStateOf(emptyList<JSONObject>()) }
    var episode by remember { mutableStateOf(initialEpisode ?: "") }
    var listed by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    var reload by remember { mutableIntStateOf(0) }
    LaunchedEffect(session, reload) {
        try {
            var rows = model.backend.call("VisualEpisodes", session).items().sortedByDescending { it.number("started_unix_nanos") }
            if (initialEpisode != null) rows = rows.filter { it.text("id") == initialEpisode }
            episodes = rows
            if (rows.none { it.text("id") == episode }) episode = rows.firstOrNull()?.text("id") ?: ""
            error = null
        } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { listed = true }
    }
    Column(Modifier.fillMaxSize()) {
        if (initialEpisode == null && episodes.isNotEmpty()) Box(Modifier.padding(horizontal = 20.dp)) {
            ChoiceField("Recording", episodes.map { it.text("id") to "${it.text("name").ifEmpty { it.text("campaign_name").ifEmpty { "Recording" } }} · ${DateFormat.getDateTimeInstance(DateFormat.MEDIUM, DateFormat.SHORT).format(Date(it.number("started_unix_nanos") / 1_000_000))}" }, episode) { episode = it }
        }
        Box(Modifier.weight(1f)) {
            val current = episode
            when {
                current.isNotEmpty() -> EpisodeSnapshotViewer(model, current, export = { shareExport(model, context, session, current) }) {
                    RecordingSnapshot(model.backend.call("VisualEpisode", session, current).obj())
                }
                error != null -> FailureView("Could not load recordings", error!!) { reload++ }
                !listed -> Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) { LoadingRow("Loading recordings…") }
                else -> EmptyMessage("No recordings", "Campaign recordings will appear here.", Icons.Outlined.VideoLibrary)
            }
        }
    }
}

/** Loads one recording with [load] and plays it. [key] identifies the recording; [export] adds a share button. */
@Composable
fun EpisodeSnapshotViewer(model: WendyModel, key: Any? = Unit, export: (suspend () -> Unit)? = null, load: suspend () -> RecordingSnapshot) {
    val context = LocalContext.current
    var data by remember { mutableStateOf<RecordingSnapshot?>(null) }
    var loading by remember { mutableStateOf(true) }
    var error by remember { mutableStateOf<String?>(null) }
    var reload by remember { mutableIntStateOf(0) }
    LaunchedEffect(key, reload) {
        loading = true; error = null
        try { data = withContext(Dispatchers.Default) { load() } } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { loading = false }
    }
    // Downloaded media belongs to the loaded snapshot and is deleted with it.
    val current = data
    DisposableEffect(current) { onDispose { current?.mediaPaths?.forEach { deleteMedia(context.cacheDir, it) } } }
    when {
        loading -> Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) { LoadingRow("Loading recording…") }
        error != null -> FailureView("Could not load recording", error!!) { reload++ }
        current == null -> EmptyMessage("No recordings", "Campaign recordings will appear here.", Icons.Outlined.VideoLibrary)
        else -> SnapshotPlayer(model, current, export)
    }
}

@Composable
private fun SnapshotPlayer(model: WendyModel, current: RecordingSnapshot, export: (suspend () -> Unit)?) {
    val scope = rememberCoroutineScope()
    var track by remember(current) { mutableStateOf(current.videos.firstOrNull()?.track ?: current.tracks.firstOrNull()?.id ?: "") }
    var cursor by remember(current) { mutableDoubleStateOf(current.start) }
    var playing by remember(current) { mutableStateOf(false) }
    var viewport by remember(current) { mutableStateOf(TimelineViewport.fitted(current.start, current.end)) }
    var exporting by remember { mutableStateOf(false) }
    LaunchedEffect(current, playing) {
        if (!playing) return@LaunchedEffect
        val started = System.nanoTime(); val origin = cursor
        val bounds = TimelineViewport.fitted(current.start, current.end)
        while (playing) {
            cursor = min(current.end, origin + (System.nanoTime() - started) / 1e9)
            viewport = viewport.revealing(cursor, bounds)
            if (cursor >= current.end) { playing = false; break }
            delay(33)
        }
    }
    val visible = LocalPageVisible.current
    LaunchedEffect(visible) { if (!visible) playing = false }
    Column(Modifier.fillMaxSize()) {
        Column(Modifier.weight(1f).verticalScroll(rememberScrollState()).padding(horizontal = 20.dp, vertical = 8.dp), verticalArrangement = Arrangement.spacedBy(14.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Box(Modifier.weight(1f)) { ChoiceField("Channel", current.tracks.map { it.id to it.name }, track) { track = it } }
                if (export != null) IconButton(onClick = {
                    if (exporting) return@IconButton
                    exporting = true
                    scope.launch { try { export() } catch (e: CancellationException) { throw e } catch (e: Exception) { model.error = e.message } finally { exporting = false } }
                }, enabled = !exporting) { if (exporting) CircularProgressIndicator(Modifier.size(18.dp), strokeWidth = 2.dp) else Icon(Icons.Outlined.IosShare, "Export recording") }
            }
            val selectedTrack = current.tracks.firstOrNull { it.id == track }
            val video = current.videos.firstOrNull { it.track == track }
            val audio = current.audio.firstOrNull { it.track == track }
            val sample = current.latest(track, cursor)
            when {
                video != null -> if (video.frame(cursor) != null) RecordedVideo(video, cursor, playing) else EmptyMessage("No frame at this time", "Move the cursor to a time with video.", Icons.Outlined.Videocam)
                audio != null -> RecordedAudio(audio, cursor, playing)
                sample != null && sample.points.isNotEmpty() -> PointCloudView(sample.points, Modifier.fillMaxWidth().aspectRatio(4f / 3f))
                sample != null && sample.image.isNotEmpty() -> {
                    val image = remember(sample) { runCatching { Base64.decode(sample.image, Base64.DEFAULT).let { BitmapFactory.decodeByteArray(it, 0, it.size) } }.getOrNull() }
                    image?.let { Image(it.asImageBitmap(), selectedTrack?.name, Modifier.fillMaxWidth().clip(RoundedCornerShape(12.dp))) }
                }
            }
            val numeric = remember(current, track) { current.numeric(track) }
            if (numeric.size > 1) NumericChart(numeric, viewport, cursor)
            if (sample != null) {
                Text(sample.label.ifEmpty { selectedTrack?.name ?: "Sample" }, style = MaterialTheme.typography.titleMedium)
                if (sample.value != null) Text("${sample.value} ${sample.unit}".trim(), style = MaterialTheme.typography.bodyLarge)
                if (sample.points.isNotEmpty()) SecondaryText("${sample.points.size / 3} points · Drag to rotate · Pinch to zoom · Double-tap to reset")
                SecondaryText("Recorded at %+.3f s".format(sample.seconds))
                if (sample.detail.isNotEmpty()) SelectionContainer { Text(sample.detail, fontFamily = FontFamily.Monospace, style = MaterialTheme.typography.bodySmall) }
            } else if (audio == null) SecondaryText("No sample before the cursor")
            SecondaryText(current.warnings)
        }
        HorizontalDivider(color = MaterialTheme.colorScheme.outlineVariant)
        RecordingTimeline(current, cursor, { cursor = it; playing = false }, playing, { if (cursor >= current.end) cursor = current.start; playing = !playing }, viewport) { viewport = it }
    }
}

private fun deleteMedia(cache: File, path: String) { val file = File(path); if (file.parentFile?.canonicalFile == cache.canonicalFile) file.delete() }

private suspend fun shareExport(model: WendyModel, context: android.content.Context, session: Long, episode: String) {
    val path = model.backend.call("ExportVisualEpisode", session, episode) as String
    val destination = withContext(Dispatchers.IO) {
        val directory = context.cacheDir.resolve("exports").apply { mkdirs() }
        val output = File.createTempFile("wendy-episode-", ".zip", directory)
        File(path).copyTo(output, overwrite = true); File(path).delete(); output
    }
    val uri = FileProvider.getUriForFile(context, "${context.packageName}.files", destination)
    context.startActivity(Intent.createChooser(Intent(Intent.ACTION_SEND).apply { type = "application/zip"; putExtra(Intent.EXTRA_STREAM, uri); addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION) }, "Export recording"))
}

@Composable
internal fun RecordingTimeline(data: RecordingSnapshot, cursor: Double, seek: (Double) -> Unit, playing: Boolean, toggle: () -> Unit, viewport: TimelineViewport, setViewport: (TimelineViewport) -> Unit) {
    val bounds = TimelineViewport.fitted(data.start, data.end)
    val currentViewport by rememberUpdatedState(viewport)
    val purple = MaterialTheme.colorScheme.secondary
    val cursorColor = Color(0xFFE0912B)
    val track = MaterialTheme.colorScheme.surfaceVariant
    Column(Modifier.fillMaxWidth().padding(12.dp)
        // Two-finger pinch zooms around the gesture's center, as on iOS.
        .pointerInput(bounds) {
            pinchTimeline({ currentViewport }) { origin, zoom, anchor -> setViewport(origin.zoomed(zoom.toDouble(), anchor.toDouble(), bounds)) }
        }, verticalArrangement = Arrangement.spacedBy(8.dp)) {
        Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(4.dp)) {
            IconButton(onClick = toggle) { Icon(if (playing) Icons.Outlined.Pause else Icons.Outlined.PlayArrow, if (playing) "Pause recording" else "Play recording") }
            Text("%+.3f s".format(cursor), fontFamily = FontFamily.Monospace, style = MaterialTheme.typography.bodyMedium)
            Spacer(Modifier.weight(1f))
            IconButton(onClick = { setViewport(viewport.zoomed(0.5, 0.5, bounds)) }) { Icon(Icons.Outlined.ZoomOut, "Zoom out") }
            IconButton(onClick = { setViewport(viewport.zoomed(2.0, 0.5, bounds)) }) { Icon(Icons.Outlined.ZoomIn, "Zoom in") }
            TextButton(onClick = { setViewport(bounds) }) { Text("Fit") }
        }
        Row(Modifier.fillMaxWidth().height(28.dp).pointerInput(bounds) {
            var origin: TimelineViewport? = null
            var moved = 0f
            detectDragGestures(onDragStart = { origin = currentViewport; moved = 0f }, onDragEnd = { origin = null }) { change, amount ->
                change.consume()
                moved += amount.x
                val start = origin ?: return@detectDragGestures
                setViewport(start.panned(-moved.toDouble() / max(size.width, 1) * start.span, bounds))
            }
        }, verticalAlignment = Alignment.CenterVertically) {
            Text("%+.2f s".format(viewport.start), style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            Spacer(Modifier.weight(1f)); Text("Drag ruler to pan", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant); Spacer(Modifier.weight(1f))
            Text("%+.2f s".format(viewport.end), style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        Column(Modifier.heightIn(max = 150.dp).verticalScroll(rememberScrollState()), verticalArrangement = Arrangement.spacedBy(6.dp)) {
            data.tracks.forEach { t ->
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Text(t.name, Modifier.width(110.dp), style = MaterialTheme.typography.labelSmall, maxLines = 1)
                    Canvas(Modifier.weight(1f).height(24.dp).clip(RoundedCornerShape(4.dp)).background(track).semantics { contentDescription = "${t.name} timeline" }
                        .pointerInput(bounds) {
                            detectTapGestures { seek(currentViewport.time(it.x / size.width.toDouble())) }
                        }.pointerInput(bounds) {
                            detectDragGestures { change, _ -> change.consume(); seek(currentViewport.time(change.position.x / size.width.toDouble())) }
                        }) {
                        val values = data.timestamps[t.id] ?: DoubleArray(0)
                        val from = upperBound(values, viewport.start - 0.000_001)
                        val to = upperBound(values, viewport.end)
                        var lastPixel = -2
                        for (i in from until to) {
                            val x = ((values[i] - viewport.start) / viewport.span * size.width).toFloat()
                            if (x.toInt() == lastPixel) continue
                            lastPixel = x.toInt()
                            drawRect(purple, Offset(x, 3f), Size(1.5f * density, size.height - 6f))
                        }
                        if (cursor in viewport.start..viewport.end) drawRect(cursorColor, Offset(((cursor - viewport.start) / viewport.span * size.width).toFloat(), 0f), Size(2f * density, size.height))
                    }
                }
            }
        }
        Text("${data.tracks.size} channels · Drag tracks to seek · Pinch to zoom", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

/**
 * Two-finger pinch zoom. Zoom is measured from the window and finger distance when
 * the pinch began, so a burst of touch events cannot compound or drop steps.
 * One-finger drags are left to the ruler and tracks.
 */
private suspend fun androidx.compose.ui.input.pointer.PointerInputScope.pinchTimeline(current: () -> TimelineViewport, onZoom: (TimelineViewport, Float, Float) -> Unit) {
    awaitPointerEventScope {
        var origin: TimelineViewport? = null
        var startDistance = 0f
        var anchor = 0.5f
        while (true) {
            val event = awaitPointerEvent(androidx.compose.ui.input.pointer.PointerEventPass.Initial)
            val pressed = event.changes.filter { it.pressed }
            if (pressed.size < 2) { origin = null; continue }
            val (a, b) = pressed[0] to pressed[1]
            val distance = (a.position - b.position).getDistance()
            val start = origin
            if (start == null) {
                origin = current(); startDistance = distance
                anchor = ((a.position.x + b.position.x) / 2 / max(size.width, 1)).coerceIn(0f, 1f)
            } else if (startDistance > 0f && distance > 0f) {
                onZoom(start, distance / startDistance, anchor)
                event.changes.forEach { it.consume() }
            }
        }
    }
}

@Composable
private fun NumericChart(samples: List<RecordingSample>, viewport: TimelineViewport, cursor: Double) {
    val line = MaterialTheme.colorScheme.primary
    val cursorColor = Color(0xFFE0912B)
    Canvas(Modifier.fillMaxWidth().height(180.dp)) {
        val visible = samples.filter { it.seconds in viewport.start..viewport.end }.ifEmpty { samples }
        val low = visible.minOf { it.value!! }; val high = visible.maxOf { it.value!! }
        val range = (high - low).takeIf { it > 0 } ?: 1.0
        fun point(s: RecordingSample) = Offset(((s.seconds - viewport.start) / viewport.span * size.width).toFloat(), (size.height * (1 - (s.value!! - low) / range)).toFloat())
        samples.zipWithNext().forEach { (a, b) -> if (b.seconds >= viewport.start && a.seconds <= viewport.end) drawLine(line, point(a), point(b), 2.dp.toPx()) }
        if (cursor in viewport.start..viewport.end) { val x = ((cursor - viewport.start) / viewport.span * size.width).toFloat(); drawLine(cursorColor, Offset(x, 0f), Offset(x, size.height), 2.dp.toPx()) }
    }
}

/** Plays the recorded video in step with the cursor; while paused it shows the frame at the cursor. */
@Composable
private fun RecordedVideo(video: RecordingVideo, cursor: Double, playing: Boolean) {
    val first = video.times.firstOrNull() ?: 0.0
    var player by remember { mutableStateOf<VideoView?>(null) }
    var ready by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    LaunchedEffect(cursor, ready, playing) {
        val view = player ?: return@LaunchedEffect
        if (!ready) return@LaunchedEffect
        val target = (((video.frame(cursor) ?: first) - first) * 1000).toInt().coerceAtLeast(0)
        if (playing) { if (!view.isPlaying) { view.seekTo(target); view.start() } else if (abs(view.currentPosition - target) > 250) view.seekTo(target) }
        else { if (view.isPlaying) view.pause(); if (abs(view.currentPosition - target) > 40) view.seekTo(target) }
    }
    DisposableEffect(video.path) { onDispose { player?.stopPlayback() } }
    Box(Modifier.fillMaxWidth().aspectRatio(4f / 3f).clip(RoundedCornerShape(12.dp)).background(Color.Black)) {
        AndroidView(factory = { context -> VideoView(context).apply {
            player = this; setVideoPath(video.path)
            setOnPreparedListener { it.setVolume(0f, 0f); ready = true; seekTo(1) }
            setOnErrorListener { _, what, extra -> error = "Video playback failed ($what/$extra)"; true }
        } }, modifier = Modifier.fillMaxSize())
    }
    error?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodySmall) }
}

/** Recorded audio follows the timeline: it plays while the recording plays and seeks with the cursor. */
@Composable
private fun RecordedAudio(audio: RecordingAudio, cursor: Double, playing: Boolean) {
    var player by remember { mutableStateOf<MediaPlayer?>(null) }
    var error by remember { mutableStateOf<String?>(null) }
    var muted by remember { mutableStateOf(false) }
    DisposableEffect(audio.path) {
        val media = runCatching { MediaPlayer().apply { setDataSource(audio.path); prepare() } }.onFailure { error = "Audio playback failed: ${it.message}" }.getOrNull()
        player = media
        onDispose { media?.release(); player = null }
    }
    val inRange = cursor >= audio.start && cursor <= audio.end
    LaunchedEffect(cursor, playing, player, muted) {
        val media = player ?: return@LaunchedEffect
        val target = ((cursor - audio.start) * 1000).toInt().coerceIn(0, max(media.duration, 0))
        media.setVolume(if (muted) 0f else 1f, if (muted) 0f else 1f)
        if (playing && inRange) { if (!media.isPlaying) { media.seekTo(target); media.start() } else if (abs(media.currentPosition - target) > 250) media.seekTo(target) }
        else { if (media.isPlaying) media.pause(); if (abs(media.currentPosition - target) > 40) media.seekTo(target) }
    }
    FormSection {
        Row(Modifier.padding(vertical = 8.dp), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
            Icon(Icons.Outlined.GraphicEq, null, tint = MaterialTheme.colorScheme.primary)
            Column(Modifier.weight(1f)) {
                Text("Recorded audio", style = MaterialTheme.typography.titleSmall)
                SecondaryText(if (!inRange) "No audio at this time" else if (playing) "Playing with the recording" else "Press play to listen from the cursor")
                SecondaryText("%+.2f s to %+.2f s".format(audio.start, audio.end))
            }
            IconButton(onClick = { muted = !muted }) { Icon(if (muted) Icons.AutoMirrored.Outlined.VolumeOff else Icons.AutoMirrored.Outlined.VolumeUp, if (muted) "Unmute" else "Mute") }
        }
        error?.let { FailureNote(it) }
    }
}

/**
 * A rotatable view of a point cloud in the sensor frame, in meters. Points are
 * colored by height. Drag to orbit, pinch to zoom, double-tap to reset.
 */
@Composable
fun PointCloudView(points: FloatArray, modifier: Modifier = Modifier) {
    var yaw by remember { mutableFloatStateOf(-0.6f) }
    var pitch by remember { mutableFloatStateOf(0.45f) }
    var zoom by remember { mutableFloatStateOf(1f) }
    val count = points.size / 3
    val bounds = remember(points) {
        var minZ = Float.MAX_VALUE; var maxZ = -Float.MAX_VALUE; var cx = 0.0; var cy = 0.0; var cz = 0.0
        for (i in 0 until count) { val z = points[i * 3 + 2]; minZ = min(minZ, z); maxZ = max(maxZ, z); cx += points[i * 3]; cy += points[i * 3 + 1]; cz += z }
        var radius = 0.001f
        val center = floatArrayOf((cx / max(count, 1)).toFloat(), (cy / max(count, 1)).toFloat(), (cz / max(count, 1)).toFloat())
        for (i in 0 until count) radius = max(radius, sqrt((points[i * 3] - center[0]).pow(2) + (points[i * 3 + 1] - center[1]).pow(2) + (points[i * 3 + 2] - center[2]).pow(2)))
        Triple(center, radius, minZ to maxZ)
    }
    val low = MaterialTheme.colorScheme.primary
    val high = Color(0xFFE0912B)
    Canvas(modifier.clip(RoundedCornerShape(12.dp)).background(Color(0xFF0B1713))
        .pointerInput(Unit) { detectTapGestures(onDoubleTap = { yaw = -0.6f; pitch = 0.45f; zoom = 1f }) }
        .pointerInput(Unit) {
            detectTransformGestures { _, pan, gestureZoom, _ ->
                yaw += pan.x / size.width * 4f
                pitch = (pitch + pan.y / size.height * 3f).coerceIn(-1.5f, 1.5f)
                zoom = (zoom * gestureZoom).coerceIn(0.2f, 20f)
            }
        }) {
        val (center, radius, heights) = bounds
        val (minZ, maxZ) = heights
        val scale = min(size.width, size.height) / (2.4f * radius) * zoom
        val cosY = cos(yaw); val sinY = sin(yaw); val cosP = cos(pitch); val sinP = sin(pitch)
        val distance = 3f * radius
        val step = max(1, count / 20_000)
        val dot = Size(2.2f, 2.2f)
        var i = 0
        while (i < count) {
            val x = points[i * 3] - center[0]; val y = points[i * 3 + 1] - center[1]; val z = points[i * 3 + 2] - center[2]
            // ROS sensor frame: x forward, y left, z up. Orbit around z, then tilt.
            val rx = x * cosY - y * sinY; val ry = x * sinY + y * cosY
            val depth = rx * cosP + z * sinP
            val up = -rx * sinP + z * cosP
            val perspective = distance / max(distance + depth, 0.05f * radius)
            val sx = size.width / 2 - ry * scale * perspective
            val sy = size.height / 2 - up * scale * perspective
            if (sx in 0f..size.width && sy in 0f..size.height) {
                val t = if (maxZ > minZ) ((points[i * 3 + 2] - minZ) / (maxZ - minZ)).coerceIn(0f, 1f) else 0.5f
                drawRect(androidx.compose.ui.graphics.lerp(low, high, t), Offset(sx - 1.1f, sy - 1.1f), dot)
            }
            i += step
        }
        // Sensor origin.
        val ox = -center[0]; val oy = -center[1]; val oz = -center[2]
        val orx = ox * cosY - oy * sinY; val ory = ox * sinY + oy * cosY
        val od = orx * cosP + oz * sinP; val ou = -orx * sinP + oz * cosP
        val op = distance / max(distance + od, 0.05f * radius)
        drawCircle(Color.White, 4.dp.toPx(), Offset(size.width / 2 - ory * scale * op, size.height / 2 - ou * scale * op))
    }
}
