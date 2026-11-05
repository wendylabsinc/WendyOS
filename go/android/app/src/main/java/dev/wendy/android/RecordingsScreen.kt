package dev.wendy.android

import android.content.Intent
import android.graphics.BitmapFactory
import android.util.Base64
import android.widget.VideoView
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.Image
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.core.content.FileProvider
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.withContext
import org.json.JSONArray
import org.json.JSONObject
import java.io.File
import kotlin.math.abs

@Composable
fun RecordingsScreen(model: WendyModel) {
    val episodes = rememberRemote(model, "VisualEpisodes", model.session)
    val active = rememberRemote(model, "VisualRecordingStatus", model.session)
    var selected by remember { mutableStateOf<String?>(null) }
    var recording by remember { mutableStateOf(false) }
    if (selected != null) { EpisodeScreen(model, selected!!) { selected = null }; return }
    if (recording) { RecordingEditor(model) { recording = false; active.refresh(); episodes.refresh() }; return }
    LazyColumn(contentPadding = PaddingValues(20.dp), verticalArrangement = Arrangement.spacedBy(14.dp)) {
        item { PageTitle("Recordings"); RemoteStatus(episodes); RemoteStatus(active)
            if (active.value.obj().text("id").isNotBlank()) SectionCard("Recording in progress") { OptionalMessage(active.value.obj().text("name")); Text("Capturing selected sources", style = MaterialTheme.typography.bodyMedium); Button(onClick = { model.task { model.backend.call("StopVisualRecording", model.session); active.refresh(); episodes.refresh() } }) { Text("Stop recording") } }
            else Button(onClick = { recording = true }) { Text("Start recording") }
            TextButton(onClick = { episodes.refresh(); active.refresh() }) { Text("Refresh") }
        }
        items(episodes.value.items()) { item -> Card(onClick = { selected = item.text("id") }, modifier = Modifier.fillMaxWidth()) { Column(Modifier.padding(18.dp)) { Text(item.text("name").ifBlank { "Recording" }, style = MaterialTheme.typography.titleMedium); Text(item.text("state").replaceFirstChar { it.uppercase() }); Text(formatBytes(item.number("size_bytes")), style = MaterialTheme.typography.bodySmall) } } }
    }
}
@Composable private fun RecordingEditor(model: WendyModel, done: () -> Unit) {
    val sources = rememberRemote(model, "CampaignSources", model.session)
    var name by remember { mutableStateOf("") }
    val selected = remember { mutableStateMapOf<String, Boolean>() }
    Column(Modifier.verticalScroll(rememberScrollState()).padding(20.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
        TextButton(onClick = done) { Text("Back to recordings") }; PageTitle("Start recording"); RemoteStatus(sources)
        OutlinedTextField(name, { name = it }, label = { Text("Recording name") })
        sources.value.items().forEach { source -> Row { Text(source.text("name", "id"), Modifier.weight(1f)); Checkbox(selected[source.text("id")] == true, { selected[source.text("id")] = it }) } }
        Button(enabled = selected.values.any { it }, onClick = { model.task { model.backend.call("StartVisualRecording", model.session, name, JSONArray(selected.filterValues { it }.keys.toList())); model.record("Started recording $name"); done() } }) { Text("Record") }
    }
}
@Composable
fun EpisodeScreen(model: WendyModel, episode: String, back: () -> Unit) {
    val state = rememberRemote(model, "VisualEpisode", model.session, episode)
    val data = state.value.obj()
    val samples = remember(state.value) { data.list("Samples") }
    val videos = remember(state.value) { data.list("Videos") }
    val start = data.number("Start"); val end = data.number("End")
    var progress by remember { mutableFloatStateOf(0f) }
    var playing by remember { mutableStateOf(false) }
    val time = start + ((end - start) * progress.toDouble()).toLong()
    val context = LocalContext.current
    LaunchedEffect(playing, state.value) { while (playing && end > start) { delay(33); progress = (progress + 33_000_000.0 / (end-start)).toFloat().coerceAtMost(1f); if (progress >= 1f) playing = false } }
    DisposableEffect(state.value) {
        val paths = videos.map { it.text("Path") } + data.list("Audio").map { it.text("Path") }
        onDispose { paths.forEach { path -> val file = File(path); if (file.parentFile?.canonicalFile == context.cacheDir.canonicalFile) file.delete() } }
    }
    Column(Modifier.fillMaxSize().padding(16.dp), verticalArrangement = Arrangement.spacedBy(10.dp)) {
        Row { TextButton(onClick = back) { Text("Back to recordings") }; Spacer(Modifier.weight(1f)); TextButton(onClick = { model.task {
            val path = model.backend.call("ExportVisualEpisode", model.session, episode) as String
            val destination = withContext(Dispatchers.IO) { val directory = context.cacheDir.resolve("exports").apply { mkdirs() }; val output = File.createTempFile("wendy-episode-", ".zip", directory); File(path).copyTo(output, overwrite = true); File(path).delete(); output }
            val uri = FileProvider.getUriForFile(context, "${context.packageName}.files", destination)
            context.startActivity(Intent.createChooser(Intent(Intent.ACTION_SEND).apply { type = "application/zip"; putExtra(Intent.EXTRA_STREAM, uri); addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION) }, "Export recording"))
        } }) { Text("Export") } }
        PageTitle("Recorded data"); RemoteStatus(state)
        Row { Button(onClick = { if (progress >= 1f) progress = 0f; playing = !playing }, enabled = end > start) { Text(if (playing) "Pause" else "Play") }; Spacer(Modifier.width(16.dp)); Text("%.1f / %.1f s".format((time - start) / 1e9, (end - start) / 1e9)) }
        Slider(progress, { progress = it; playing = false }, enabled = end > start)
        LazyColumn(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            videos.forEach { video -> item(key = video.text("Path")) { RecordedVideo(video, time) } }
            data.list("Tracks").forEach { track ->
                val points = samples.filter { it.text("Track") == track.text("ID") }
                val nearby = points.filter { it.number("Time") <= time }.lastOrNull()
                item { SectionCard(track.text("Name", "ID")) {
                    val numeric = points.filter { it.flag("HasValue") }
                    if (numeric.size > 1) {
                        val color = MaterialTheme.colorScheme.primary
                        Canvas(Modifier.fillMaxWidth().height(100.dp)) {
                            val low = numeric.minOf { it.optDouble("Value") }; val high = numeric.maxOf { it.optDouble("Value") }; val range = (high-low).takeIf { it > 0 } ?: 1.0
                            fun location(p: JSONObject) = Offset(((p.number("Time") - start).toDouble() / (end-start).coerceAtLeast(1) * size.width).toFloat(), (size.height * (1 - (p.optDouble("Value")-low)/range)).toFloat())
                            numeric.zipWithNext().forEach { (a,b) -> drawLine(color, location(a), location(b), 2f) }
                            val x = size.width * progress; drawLine(color.copy(alpha = .4f), Offset(x,0f), Offset(x,size.height), 2f)
                        }
                    }
                    nearby?.let { value ->
                        Text(value.text("Label")); Text(value.text("Detail"), style = MaterialTheme.typography.bodySmall)
                        if (value.flag("HasValue")) Text("${value.text("Value")} ${value.text("Unit")}")
                        val image = remember(value) { value.text("Image").takeIf { it.isNotEmpty() }?.let { encoded -> runCatching { val bytes = Base64.decode(encoded, Base64.DEFAULT); BitmapFactory.decodeByteArray(bytes, 0, bytes.size) }.getOrNull() } }
                        image?.let { Image(it.asImageBitmap(), track.text("Name"), Modifier.fillMaxWidth().heightIn(max = 250.dp)) }
                    }
                } }
            }
            item { data.optJSONArray("Warnings")?.let { warnings -> for (index in 0 until warnings.length()) OptionalMessage(warnings.optString(index)) } }
        }
    }
}
@Composable private fun RecordedVideo(video: JSONObject, cursor: Long) {
    val times = video.optJSONArray("Times")
    val start = times?.optLong(0) ?: 0L
    var player by remember { mutableStateOf<VideoView?>(null) }
    var ready by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    LaunchedEffect(cursor, ready) { if (ready) { val position = ((cursor - start).coerceAtLeast(0) / 1_000_000).coerceAtMost(Int.MAX_VALUE.toLong()).toInt(); if (abs((player?.currentPosition ?: 0) - position) > 60) player?.seekTo(position) } }
    DisposableEffect(Unit) { onDispose { player?.stopPlayback() } }
    SectionCard(video.text("Track")) {
        AndroidView(factory = { context -> VideoView(context).apply { player = this; setVideoPath(video.text("Path")); setOnPreparedListener { it.setVolume(0f,0f); ready = true; seekTo(1) }; setOnErrorListener { _,what,extra -> error = "Video playback failed ($what/$extra)"; true } } }, modifier = Modifier.fillMaxWidth().aspectRatio(16f/9f))
        error?.let { Text(it, color = MaterialTheme.colorScheme.error) }
    }
}
