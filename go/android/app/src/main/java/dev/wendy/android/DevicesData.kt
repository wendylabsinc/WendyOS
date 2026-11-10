package dev.wendy.android

import android.content.Intent
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.material3.*
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.core.content.FileProvider
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.NonCancellable
import kotlinx.coroutines.async
import kotlinx.coroutines.awaitAll
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import org.json.JSONArray
import org.json.JSONObject
import java.io.File
import java.text.DateFormat
import java.util.Calendar
import java.util.Date

/** Recording kinds, as iOS MobileRecordingKind names the agent's episode kinds. */
fun recordingKindTitle(kind: String) = when (kind) { "mixed" -> "Mixed"; "video" -> "Video"; "audio" -> "Audio"; "rosbag" -> "ROS bag"; "images" -> "Images"; "dataset" -> "Dataset"; else -> "Recording" }
private val recordingKinds = listOf("video", "audio", "rosbag", "images", "dataset", "mixed")

/** The trigger label colors iOS uses for recording pills (RecordingAppearance.dot). */
fun labelColor(label: String?): Color = when (label?.lowercase()) {
    "person" -> Color(0xFFFF3B30); "door opened" -> Color(0xFFFF9500); "motion" -> Color(0xFFFFD60A); "schedule" -> Color(0xFF64D2FF)
    "manual" -> Color(0xFF9FE2BF); "loud sound" -> Color(0xFFAF52DE); else -> Color(0xFF8E8E93)
}

data class RecordingItem(val id: String, val title: String, val trigger: String, val kind: String, val campaign: String, val startedNanos: Long, val bytes: Long, val duration: Double) {
    val started get() = startedNanos / 1_000_000
    companion object {
        fun parse(value: JSONObject) = RecordingItem(value.text("ID"), value.text("Title").ifEmpty { "Recording" }, value.text("Trigger"), value.text("Kind"), value.text("Campaign"),
            value.number("Started"), value.number("Bytes"), if (value.has("Duration")) value.optDouble("Duration", -1.0) else -1.0)
    }
}

class RecordingLibraryRead(val device: Device, val episodes: List<RecordingItem>, val model: String, val warning: String, val problem: String?)

/** One device's recordings read without changing the connected device (iOS GoClient.readRecordings). */
suspend fun <T> WendyModel.readRecordings(device: Device, block: suspend (Long) -> T): T {
    require(!device.lite) { "Wendy Lite does not store recordings." }
    val id = (backend.call("BeginRecordingRead") as? Number)?.toLong() ?: 0L
    check(id != 0L) { "Recording access is paused. Refresh to try again." }
    try { return block(id) } finally { withContext(NonCancellable) { try { backend.call("EndRecordingRead", id) } catch (_: Exception) {} } }
}

/** Recording targets: nearby devices plus Cloud devices that are online (iOS RecordingSources). */
private fun recordingTargets(model: WendyModel): Pair<List<Device>, Int> {
    val routes = linkedMapOf<String, Device>()
    model.nearbyDevices.filter { !it.lite && !it.selector.startsWith("vm:") }.forEach { routes.putIfAbsent(widgetDeviceID(it), it) }
    val fresh = model.presence != null && model.presenceError.isEmpty() && model.cloudError == null && System.currentTimeMillis() - model.presenceAt < 60_000
    var unavailable = 0
    model.presence?.forEach { row ->
        val device = row.device
        if (fresh && row.online) routes.putIfAbsent(widgetDeviceID(device), device) else if (widgetDeviceID(device) !in routes) unavailable++
    }
    model.selected?.takeIf { model.session != 0L && !model.lite && !it.selector.startsWith("cloud://") }?.let { routes.putIfAbsent(widgetDeviceID(it), it) }
    return routes.values.toList() to unavailable
}

/** Data: every device with saved recordings, filterable by kind (iOS MobileLensData). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun DevicesDataTab(model: WendyModel, openDevice: (Device) -> Unit) {
    val nav = LocalNavigator.current
    val scope = rememberCoroutineScope()
    var reads by remember(model.account, model.organization) { mutableStateOf<List<RecordingLibraryRead>>(emptyList()) }
    var loading by remember { mutableStateOf(false) }
    var kind by remember { mutableStateOf<String?>(null) }
    var refresh by remember { mutableIntStateOf(0) }
    var refreshing by remember { mutableStateOf(false) }
    var showProblems by remember { mutableStateOf(false) }
    val (targets, unavailable) = recordingTargets(model)
    val visible = LocalPageVisible.current
    LaunchedEffect(targets.map { it.selector }, refresh, visible) {
        if (!visible) return@LaunchedEffect
        loading = true
        try {
            reads = coroutineScope {
                targets.map { device ->
                    async {
                        try {
                            val library = model.readRecordings(device) { id -> model.backend.call("RecordingLibrary", id, device.selector, device.requiresMTLS).obj() }
                            RecordingLibraryRead(device, library.list("Episodes").map(RecordingItem::parse).sortedByDescending { it.startedNanos }, library.text("Model"), library.text("Warning"), null)
                        } catch (e: CancellationException) { throw e } catch (e: Exception) { RecordingLibraryRead(device, emptyList(), "", "", e.message ?: "Could not read recordings") }
                    }
                }.awaitAll()
            }
        } finally { loading = false }
    }
    val libraries = reads.map { read -> read to read.episodes.filter { kind == null || it.kind == kind } }.filter { it.second.isNotEmpty() }
        .sortedByDescending { it.second.first().startedNanos }
    val empty = reads.count { it.problem == null && it.episodes.isEmpty() }
    val problems = reads.mapNotNull { r -> (r.problem ?: r.warning.ifEmpty { null })?.let { "${r.device.title}: ${PresentedFailure(it).message}" } } +
        listOfNotNull(model.nearbyError, model.cloudError, model.presenceError.ifEmpty { null }).map { PresentedFailure(it).message }
    PullToRefreshBox(refreshing, onRefresh = { scope.launch { refreshing = true; model.refresh(); refresh++; refreshing = false } }) {
        Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(horizontal = 16.dp, vertical = 8.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            Row(Modifier.horizontalScroll(rememberScrollState()), horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                FilterChip(kind == null, { kind = null }, label = { Text("All") })
                recordingKinds.forEach { value -> FilterChip(kind == value, { kind = if (kind == value) null else value }, label = { Text(recordingKindTitle(value)) }) }
            }
            libraries.forEach { (read, episodes) ->
                RecordingDeviceCard(model, read, episodes) { label -> nav.push(read.device.title) { EpisodePicker(model, read, label, kind) } }
            }
            when {
                loading && libraries.isEmpty() -> Surface(shape = RoundedCornerShape(22.dp), color = MaterialTheme.colorScheme.surface) { LensLoading("Loading recordings…") }
                loading -> LensLoading("Updating recordings…")
                libraries.isEmpty() -> EmptyMessage(if (problems.isNotEmpty() || unavailable > 0 || kind != null) "No recordings to show" else "No saved recordings",
                    if (kind != null) "Try another kind." else if (problems.isNotEmpty()) "Some devices could not be checked." else "Devices appear here when they have saved recordings.", Icons.Outlined.Storage)
            }
            if (empty > 0) Row(horizontalArrangement = Arrangement.spacedBy(6.dp), verticalAlignment = Alignment.CenterVertically) {
                Icon(Icons.Outlined.RemoveCircleOutline, null, Modifier.size(16.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant); SecondaryText(if (empty == 1) "1 device has no saved data" else "$empty devices have no saved data")
            }
            if (unavailable > 0) SecondaryText("$unavailable ${if (unavailable == 1) "device is" else "devices are"} unavailable")
            if (problems.isNotEmpty()) {
                TextButton(onClick = { showProblems = !showProblems }) { Text("Some recording details are unavailable"); Icon(if (showProblems) Icons.Outlined.ExpandLess else Icons.Outlined.ExpandMore, null) }
                if (showProblems) { problems.forEach { SecondaryText(it) }; TextButton(onClick = { refresh++ }) { Text("Try again") } }
            }
            Spacer(Modifier.height(16.dp))
        }
    }
}

/** A device with saved recordings: count, latest time, size and top trigger labels (iOS RecordingDeviceCard). */
@Composable
private fun RecordingDeviceCard(model: WendyModel, read: RecordingLibraryRead, episodes: List<RecordingItem>, open: (String?) -> Unit) {
    val latest = episodes.first()
    val labels = episodes.filter { it.trigger.isNotEmpty() }.groupingBy { it.trigger }.eachCount().entries.sortedByDescending { it.value }.take(3)
    Surface(onClick = { open(null) }, shape = RoundedCornerShape(22.dp), color = MaterialTheme.colorScheme.surface) {
        Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Row(horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
                RecordingPoster(latest, 60.dp)
                Column(Modifier.weight(1f)) {
                    Text(read.device.title, style = MaterialTheme.typography.titleMedium)
                    SecondaryText(listOf(read.model.ifEmpty { null }, "latest ${DateFormat.getTimeInstance(DateFormat.SHORT).format(Date(latest.started))}", formatBytes(episodes.sumOf { it.bytes })).filterNotNull().joinToString(" · "))
                }
                Column(horizontalAlignment = Alignment.End) { Text("${episodes.size}", fontSize = 26.sp, fontWeight = FontWeight.Bold); Text("episodes", fontSize = 11.sp, color = MaterialTheme.colorScheme.onSurfaceVariant) }
            }
            if (labels.isNotEmpty()) Row(Modifier.padding(start = 72.dp).horizontalScroll(rememberScrollState()), horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                labels.forEach { (label, count) -> LabelPill(label, count) { open(label) } }
            }
        }
    }
}

@Composable
private fun LabelPill(label: String, count: Int?, selected: Boolean = false, onClick: () -> Unit) {
    Row(Modifier.height(24.dp).background(if (selected) Color(0xFF171C23) else MaterialTheme.colorScheme.surfaceVariant, CircleShape).clickable(onClick = onClick).padding(horizontal = 9.dp),
        horizontalArrangement = Arrangement.spacedBy(5.dp), verticalAlignment = Alignment.CenterVertically) {
        Box(Modifier.size(7.dp).background(labelColor(label), CircleShape))
        Text(label, style = MaterialTheme.typography.labelMedium, color = if (selected) Color.White else MaterialTheme.colorScheme.onSurface, maxLines = 1)
        if (count != null) Text("$count", style = MaterialTheme.typography.labelMedium, color = if (selected) Color.White.copy(alpha = .7f) else Color(0xFF9A978F))
    }
}

/** A simple type poster: a dark frame for video, bars for audio, lanes for ROS bags (iOS RecordingThumbnail placeholders). */
@Composable
private fun RecordingPoster(item: RecordingItem, size: androidx.compose.ui.unit.Dp) {
    Box(Modifier.size(size).background(Color(0xFF242B35), RoundedCornerShape(14.dp)), contentAlignment = Alignment.Center) {
        Icon(when (item.kind) { "audio" -> Icons.Outlined.GraphicEq; "rosbag" -> Icons.Outlined.Hub; "images" -> Icons.Outlined.Collections; "dataset" -> Icons.Outlined.GridView; else -> Icons.Outlined.Videocam },
            recordingKindTitle(item.kind), Modifier.size(size * 0.4f), tint = Color.White.copy(alpha = .85f))
    }
}

/** Episodes for one device grouped by day, with label filters, play and export (iOS RecordingEpisodePicker). */
@Composable
private fun EpisodePicker(model: WendyModel, read: RecordingLibraryRead, initialLabel: String?, kind: String?) {
    val nav = LocalNavigator.current
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    var label by remember { mutableStateOf(initialLabel) }
    var picked by remember { mutableStateOf<String?>(null) }
    var exporting by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    val all = read.episodes.filter { kind == null || it.kind == kind }
    val episodes = all.filter { label == null || it.trigger == label }
    val labels = all.filter { it.trigger.isNotEmpty() }.groupingBy { it.trigger }.eachCount()
    val today = Calendar.getInstance()
    fun day(item: RecordingItem): String {
        val c = Calendar.getInstance().apply { timeInMillis = item.started }
        val days = ((today.timeInMillis - c.timeInMillis) / 86_400_000L).toInt()
        return if (c.get(Calendar.DAY_OF_YEAR) == today.get(Calendar.DAY_OF_YEAR) && c.get(Calendar.YEAR) == today.get(Calendar.YEAR)) "TODAY" else if (days <= 1) "YESTERDAY" else DateFormat.getDateInstance(DateFormat.MEDIUM).format(Date(item.started)).uppercase()
    }
    Column(Modifier.fillMaxSize()) {
        Column(Modifier.weight(1f).verticalScroll(rememberScrollState()).padding(16.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            SecondaryText("${all.size} episodes · ${formatBytes(all.sumOf { it.bytes })}")
            Row(Modifier.horizontalScroll(rememberScrollState()), horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                LabelPill("All", all.size, selected = label == null) { label = null }
                labels.entries.sortedByDescending { it.value }.forEach { (value, count) -> LabelPill(value, count, selected = label == value) { label = value } }
            }
            episodes.groupBy(::day).forEach { (title, group) ->
                LensSection(title) {
                    group.forEachIndexed { index, item ->
                        if (index > 0) LensDivider()
                        Row(Modifier.fillMaxWidth().background(if (picked == item.id) LensStyle.green.copy(alpha = .08f) else Color.Transparent).clickable { picked = if (picked == item.id) null else item.id }
                            .padding(horizontal = 12.dp, vertical = 8.dp), horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
                            Box(Modifier.size(84.dp, 54.dp)) { RecordingPoster(item, 54.dp) }
                            Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(2.dp)) {
                                Text(item.title, style = MaterialTheme.typography.titleSmall, maxLines = 1, overflow = TextOverflow.Ellipsis)
                                SecondaryText(listOf(DateFormat.getTimeInstance(DateFormat.SHORT).format(Date(item.started)), if (item.duration >= 0) "%d:%02d".format(item.duration.toLong() / 60, item.duration.toLong() % 60) else null, recordingKindTitle(item.kind)).filterNotNull().joinToString(" · "))
                                if (item.trigger.isNotEmpty()) Row(horizontalArrangement = Arrangement.spacedBy(5.dp), verticalAlignment = Alignment.CenterVertically) { Box(Modifier.size(7.dp).background(labelColor(item.trigger), CircleShape)); Text(item.trigger, style = MaterialTheme.typography.labelMedium) }
                            }
                            if (picked == item.id) Icon(Icons.Outlined.CheckCircle, "Selected", tint = LensStyle.accent)
                        }
                    }
                }
            }
            if (episodes.isEmpty()) EmptyMessage("No episodes", "No recordings match this label.", Icons.Outlined.Storage)
            error?.let { FailureNote(it) }
        }
        Row(Modifier.fillMaxWidth().padding(16.dp), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            LensButton(if (exporting) "Exporting…" else "Export", enabled = !exporting && episodes.isNotEmpty(), modifier = Modifier.weight(1f)) {
                exporting = true; error = null
                scope.launch {
                    try {
                        val ids = picked?.let { listOf(it) } ?: episodes.map { it.id }
                        val paths = model.readRecordings(read.device) { id -> model.backend.call("ExportRecordings", id, read.device.selector, read.device.requiresMTLS, JSONArray(ids)) }
                        val files = (paths as? JSONArray)?.let { a -> (0 until a.length()).map { File(a.getString(it)) } }.orEmpty()
                        shareFiles(context, files)
                    } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { exporting = false }
                }
            }
            LensButton(if (picked == null) "Pick an episode" else "Play episode", primary = true, enabled = picked != null, modifier = Modifier.weight(1f)) {
                val id = picked ?: return@LensButton
                nav.push(episodes.firstOrNull { it.id == id }?.title ?: "Recording") { RecordingPlayback(model, read.device, id) }
            }
        }
    }
}

private suspend fun shareFiles(context: android.content.Context, files: List<File>) {
    if (files.isEmpty()) return
    val shared = withContext(Dispatchers.IO) {
        val directory = context.cacheDir.resolve("exports").apply { mkdirs() }
        files.map { source -> File(directory, source.name.ifEmpty { "wendy-recording.zip" }).also { source.copyTo(it, overwrite = true); source.delete() } }
    }
    val uris = ArrayList(shared.map { FileProvider.getUriForFile(context, "${context.packageName}.files", it) })
    val intent = if (uris.size == 1) Intent(Intent.ACTION_SEND).putExtra(Intent.EXTRA_STREAM, uris[0]) else Intent(Intent.ACTION_SEND_MULTIPLE).putParcelableArrayListExtra(Intent.EXTRA_STREAM, uris)
    context.startActivity(Intent.createChooser(intent.setType("application/zip").addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION), "Export recordings"))
}

/** Plays an episode read directly from its device, without changing the connected device. */
@Composable
private fun RecordingPlayback(model: WendyModel, device: Device, episode: String) {
    EpisodeSnapshotViewer(model) { RecordingSnapshot(model.readRecordings(device) { id -> model.backend.call("PlayRecording", id, device.selector, device.requiresMTLS, episode).obj() }) }
}
