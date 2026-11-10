package dev.wendy.android

import android.content.Intent
import android.text.format.DateUtils
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.material3.*
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.*
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.clipToBounds
import androidx.compose.ui.draw.rotate
import androidx.compose.ui.geometry.CornerRadius
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.drawscope.DrawScope
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.core.content.FileProvider
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import org.json.JSONArray
import java.io.File
import java.text.DateFormat
import java.util.Calendar
import java.util.Date

/** The trigger label colors iOS uses for recording pills (RecordingAppearance.dot). */
fun labelColor(label: String?): Color = when (label?.lowercase()) {
    "person" -> Color(0xFFFF3B30); "door opened" -> Color(0xFFFF9500); "motion" -> Color(0xFFFFD60A); "schedule" -> Color(0xFF64D2FF)
    "manual" -> Color(0xFF9FE2BF); "loud sound" -> Color(0xFFAF52DE); else -> Color(0xFF8E8E93)
}

/** Today shows the time, yesterday says so, older days show the date (iOS RecordingAppearance.time). */
fun recordingTime(millis: Long): String = when {
    DateUtils.isToday(millis) -> DateFormat.getTimeInstance(DateFormat.SHORT).format(Date(millis))
    DateUtils.isToday(millis + DateUtils.DAY_IN_MILLIS) -> "yesterday"
    else -> DateFormat.getDateInstance(DateFormat.MEDIUM).format(Date(millis))
}

private val recordingInk = Color(0xFF171C23)
private val recordingMint = Color(0xFF9FE2BF)

/** Data: every reachable device with saved recordings, filterable by kind (iOS MobileLensData). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun DevicesDataTab(model: WendyModel, openDevice: (Device) -> Unit) {
    val catalog = LocalRecordingCatalog.current ?: return
    val nav = LocalNavigator.current
    val scope = rememberCoroutineScope()
    var kind by rememberSaveable { mutableStateOf<String?>(null) }
    var refresh by rememberSaveable { mutableIntStateOf(0) }
    var refreshing by remember { mutableStateOf(false) }
    var showProblems by remember { mutableStateOf(false) }
    val sources = RecordingSources.of(model)
    val active = LocalPageVisible.current && model.inForeground
    LaunchedEffect(active, sources.targets, refresh) { if (active) catalog.load(sources.targets, refresh, null, retaining = sources.inventory) }
    // Returning from the background reloads, as iOS does on its scene phase change.
    var generation by remember { mutableIntStateOf(model.foregroundGeneration) }
    LaunchedEffect(model.foregroundGeneration) { if (model.foregroundGeneration != generation) { generation = model.foregroundGeneration; refresh++ } }
    val libraries = catalog.reads.values.mapNotNull { it.library }.map { it.copy(episodes = it.episodes(null, kind)) }.filter { it.episodes.isNotEmpty() }
        .sortedWith(compareByDescending<RecordingLibrary> { it.episodes.first().startedNanos }.thenBy { recordingDeviceID(it.device) })
    val emptyCount = catalog.reads.values.count { it.library?.episodes?.isEmpty() == true }
    val problems = (listOfNotNull(model.nearbyError, model.cloudError, model.presenceError.ifEmpty { null }).map { PresentedFailure(it).details } +
        catalog.reads.values.mapNotNull { read -> (read.problem ?: read.library?.warning)?.let { p -> read.target.routes.firstOrNull()?.let { "${it.title}: ${PresentedFailure(p).details}" } } } +
        catalog.detailProblems.filterKeys { it in catalog.reads }.values).sorted()
    val waiting = catalog.loading || !model.initialized || (!model.nearbyLoaded && model.nearbyError == null) ||
        (model.organization.isNotEmpty() && !model.cloudLoaded && model.cloudError == null)
    CompositionLocalProvider(LocalRecordingPosters provides if (active) LocalRecordingPosters.current else null) {
        PullToRefreshBox(refreshing, onRefresh = { scope.launch { refreshing = true; model.refresh(); refresh++; refreshing = false } }) {
            Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(horizontal = 16.dp, vertical = 8.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
                RecordingKindFilter(kind) { kind = it }
                if (kind != null && catalog.reads.values.any { it.library?.labelCoverage != null }) SecondaryText("Results are partial while episode details load.")
                libraries.forEach { library ->
                    RecordingDeviceCard(library) { label -> nav.push(library.device.title) { EpisodePickerPage(model, recordingDeviceID(library.device), library.device, label, kind) } }
                }
                when {
                    waiting && libraries.isEmpty() -> Surface(shape = RoundedCornerShape(22.dp), color = MaterialTheme.colorScheme.surface) { LensLoading("Loading recordings…") }
                    waiting -> LensLoading("Updating recordings…")
                    libraries.isEmpty() -> {
                        val incomplete = problems.isNotEmpty() || sources.unavailable > 0 || kind != null
                        EmptyMessage(if (incomplete) "No recordings to show" else "No saved recordings",
                            if (!incomplete) "Devices appear here after they save their first episode." else if (kind != null) "Try another kind, or wait for episode details." else "Some devices could not be checked.", Icons.Outlined.Storage)
                    }
                }
                if (emptyCount > 0) Row(Modifier.padding(horizontal = 6.dp), horizontalArrangement = Arrangement.spacedBy(6.dp), verticalAlignment = Alignment.CenterVertically) {
                    Icon(Icons.Outlined.RemoveCircleOutline, null, Modifier.size(16.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
                    SecondaryText(if (emptyCount == 1) "1 device has no saved data" else "$emptyCount devices have no saved data")
                }
                if (sources.unavailable > 0) SecondaryText("${sources.unavailable} ${if (sources.unavailable == 1) "device is" else "devices are"} unavailable", Modifier.padding(horizontal = 6.dp))
                if (problems.isNotEmpty()) {
                    TextButton(onClick = { showProblems = !showProblems }) { Text("Some recording details are unavailable"); Icon(if (showProblems) Icons.Outlined.ExpandLess else Icons.Outlined.ExpandMore, null) }
                    if (showProblems) { problems.forEach { SecondaryText(it, Modifier.padding(horizontal = 6.dp)) }; TextButton(onClick = { refresh++ }) { Text("Try again") } }
                }
                Spacer(Modifier.height(16.dp))
            }
        }
    }
}

/** The kind menu shared by Data and the episode picker (iOS RecordingKindFilter). */
@Composable
fun RecordingKindFilter(selection: String?, select: (String?) -> Unit) {
    var open by remember { mutableStateOf(false) }
    Box {
        TextButton(onClick = { open = true }) {
            Text(selection?.let { if (it == "unknown") "Unknown" else recordingKindTitle(it) } ?: "All kinds", color = LensStyle.accent)
            Icon(Icons.Outlined.UnfoldMore, null, Modifier.size(16.dp), tint = LensStyle.accent)
        }
        DropdownMenu(open, onDismissRequest = { open = false }) {
            (listOf<String?>(null) + recordingKinds).forEach { kind ->
                DropdownMenuItem(text = { Text(kind?.let { if (it == "unknown") "Unknown" else recordingKindTitle(it) } ?: "All kinds") }, onClick = { open = false; select(kind) },
                    trailingIcon = { if (kind == selection) Icon(Icons.Outlined.Check, "Selected") })
            }
        }
    }
}

/** A device with saved recordings: latest poster, count, time and size, and its top labels (iOS RecordingDeviceCard). */
@OptIn(ExperimentalLayoutApi::class)
@Composable
fun RecordingDeviceCard(library: RecordingLibrary, open: (String?) -> Unit) {
    val latest = library.episodes.firstOrNull()
    Surface(shape = RoundedCornerShape(22.dp), color = MaterialTheme.colorScheme.surface) {
        Column(Modifier.padding(12.dp)) {
            Row(Modifier.fillMaxWidth().clickable { open(null) }, horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
                if (latest != null) RecordingThumbnail(library.device, latest, Modifier.size(60.dp).clip(RoundedCornerShape(14.dp)))
                Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(3.dp)) {
                    Text(library.device.title, style = MaterialTheme.typography.titleSmall, fontWeight = FontWeight.SemiBold, maxLines = 2, overflow = TextOverflow.Ellipsis)
                    Text(listOfNotNull(library.model, latest?.started?.let { "latest " + recordingTime(it) }, library.sizeText).joinToString(" · "),
                        style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 2, overflow = TextOverflow.Ellipsis)
                }
                Column(horizontalAlignment = Alignment.End) {
                    Text("${library.episodes.size}", fontSize = 26.sp, fontWeight = FontWeight.Bold)
                    Text(if (library.episodes.size == 1) "episode" else "episodes", fontSize = 11.sp, color = MaterialTheme.colorScheme.onSurfaceVariant)
                }
            }
            library.labelCoverage?.let { Text(it, Modifier.padding(start = 72.dp, top = 6.dp), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant) }
            if (library.labels.isNotEmpty()) FlowRow(Modifier.padding(start = 72.dp), horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                library.labels.take(3).forEach { (name, count) ->
                    Box(Modifier.heightIn(min = 44.dp).clickable { open(name) }, contentAlignment = Alignment.Center) {
                        Row(Modifier.background(MaterialTheme.colorScheme.background, CircleShape).padding(horizontal = 9.dp, vertical = 4.dp), horizontalArrangement = Arrangement.spacedBy(5.dp), verticalAlignment = Alignment.CenterVertically) {
                            Box(Modifier.size(6.dp).background(labelColor(name), CircleShape))
                            Text(name, style = MaterialTheme.typography.labelMedium, fontWeight = FontWeight.SemiBold, maxLines = 1, overflow = TextOverflow.Ellipsis)
                            Text("$count", style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1)
                        }
                    }
                }
            }
        }
    }
}

/** A real thumbnail when the device has a small preview, otherwise artwork for the recording's kind (iOS RecordingThumbnail). */
@Composable
fun RecordingThumbnail(device: Device, episode: RecordingEpisode, modifier: Modifier = Modifier) {
    val posters = LocalRecordingPosters.current
    val poster = posters?.poster(device, episode.id)
    val file = episode.previewFile
    if (posters != null && file != null && poster == null) LaunchedEffect(device.selector, episode.id, file) { posters.load(device, episode.id, file) }
    Box(modifier.clipToBounds(), contentAlignment = Alignment.Center) {
        if (poster is RecordingPoster.Still) { Image(poster.image, null, Modifier.fillMaxSize(), contentScale = ContentScale.Crop); return@Box }
        when (episode.kind) {
            "video" -> Canvas(Modifier.fillMaxSize().background(Color(0xFF242B35))) {
                var x = -size.height
                while (x < size.width) { drawLine(Color.White.copy(alpha = .035f), Offset(x, size.height), Offset(x + size.height, 0f), 8.dp.toPx()); x += 20.dp.toPx() }
            }
            "audio" -> Box(Modifier.fillMaxSize().background(Color(0xFFE8F6EE)), contentAlignment = Alignment.Center) {
                if (poster is RecordingPoster.Waveform) Canvas(Modifier.fillMaxSize()) { waveform(poster.peaks) }
                else Icon(Icons.Outlined.VolumeUp, null, Modifier.size(22.dp), tint = LensStyle.green)
            }
            "rosbag" -> Canvas(Modifier.fillMaxSize().background(recordingInk)) { topicBars(episode.previewCounts) }
            "images" -> Box(Modifier.fillMaxSize().background(Color(0xFFE6E2D8)), contentAlignment = Alignment.Center) {
                Box(Modifier.fillMaxWidth(.46f).fillMaxHeight(.52f)) {
                    Photo(Color(0xFFC9C4B8), Modifier.rotate(6f).offset(4.dp, (-3).dp))
                    Photo(Color(0xFFB8B2A5), Modifier.rotate(-4f).offset((-3).dp, 0.dp))
                    Photo(Color(0xFF9A978F), Modifier.offset(0.dp, 3.dp))
                }
            }
            "mixed" -> Box(Modifier.fillMaxSize().background(recordingInk), contentAlignment = Alignment.Center) { Icon(Icons.Outlined.Layers, null, Modifier.size(22.dp), tint = Color.White) }
            "dataset" -> Canvas(Modifier.fillMaxSize().background(Color(0xFFF9F9F9))) { datasetGrid(episode.previewCounts) }
            else -> Box(Modifier.fillMaxSize().background(MaterialTheme.colorScheme.onSurface.copy(alpha = .12f)), contentAlignment = Alignment.Center) {
                Icon(Icons.Outlined.Storage, null, Modifier.size(22.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
            }
        }
    }
}

@Composable
private fun Photo(color: Color, modifier: Modifier) {
    Box(modifier.fillMaxSize().background(color, RoundedCornerShape(5.dp)).border(2.dp, Color.White, RoundedCornerShape(5.dp)))
}

private fun DrawScope.waveform(peaks: List<Int>) {
    val step = (size.width - 12.dp.toPx()) / maxOf(1, peaks.size)
    peaks.forEachIndexed { i, peak ->
        val h = maxOf(2.dp.toPx(), peak / 255f * (size.height - 10.dp.toPx()))
        drawRoundRect(LensStyle.green, Offset(6.dp.toPx() + i * step, (size.height - h) / 2), Size(maxOf(1f, step - 1.dp.toPx()), h), CornerRadius(2.dp.toPx()))
    }
}

/** One meter per recorded topic; width is its message count relative to the busiest topic. */
private fun DrawScope.topicBars(counts: List<Long>) {
    val shown = counts.take(3)
    val peak = maxOf(1L, shown.maxOrNull() ?: 1L).toDouble()
    val colors = listOf(LensStyle.green, recordingMint, Color(0xFFFF9500))
    val inset = 6.dp.toPx(); val bar = 6.dp.toPx()
    shown.forEachIndexed { i, count ->
        val y = if (shown.size == 1) (size.height - bar) / 2 else 8.dp.toPx() + i * (size.height - 22.dp.toPx()) / maxOf(1, shown.size - 1)
        val width = size.width - 2 * inset
        drawRoundRect(Color.White.copy(alpha = .10f), Offset(inset, y), Size(width, bar), CornerRadius(2.dp.toPx()))
        drawRoundRect(colors[i], Offset(inset, y), Size(maxOf(3.dp.toPx(), (width * count / peak).toFloat()), bar), CornerRadius(2.dp.toPx()))
    }
}

private fun DrawScope.datasetGrid(counts: List<Long>) {
    val shown = counts.take(12)
    val peak = shown.maxOrNull() ?: 0L
    val gap = 3.dp.toPx(); val inset = 8.dp.toPx()
    val w = (size.width - 2 * inset - 5 * gap) / 6; val h = (size.height - 2 * inset - gap) / 2
    for (i in 0 until 12) {
        val count = shown.getOrElse(i) { 0L }
        val color = if (count == 0L) Color(0xFFE6E2D8) else if (count == peak) LensStyle.green else recordingMint
        drawRoundRect(color, Offset(inset + (i % 6) * (w + gap), inset + (i / 6) * (h + gap)), Size(w, h), CornerRadius(3.dp.toPx()))
    }
}

/**
 * One device's episodes grouped by day, with kind and label filters, export and play (iOS RecordingEpisodePicker).
 * It reads the shared catalog live, so details appear as they load.
 */
@Composable
fun EpisodePickerPage(model: WendyModel, targetID: String, fallback: Device, initialTrigger: String?, initialKind: String?, selectedID: String? = null) {
    val catalog = LocalRecordingCatalog.current ?: return
    val nav = LocalNavigator.current
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    var refresh by remember { mutableIntStateOf(0) }
    val sources = RecordingSources.of(model)
    val active = LocalPageVisible.current && model.inForeground
    LaunchedEffect(active, sources.targets, refresh) { if (active) catalog.load(sources.targets, refresh, targetID, consumer = "picker", retaining = sources.inventory) }
    val read = catalog.reads[targetID]
    val library = read?.library
    if (library == null) {
        Box(Modifier.fillMaxSize().padding(20.dp)) {
            when {
                read?.problem != null -> Column(verticalArrangement = Arrangement.spacedBy(8.dp)) { Text("Could not load recordings", style = MaterialTheme.typography.titleMedium); FailureNote(read.problem); TextButton(onClick = { refresh++ }) { Text("Try again") } }
                sources.targets.any { it.id == targetID } -> LensLoading("Loading recordings…")
                else -> EmptyMessage("Device unavailable", fallback.title, Icons.Outlined.WifiOff)
            }
        }
        return
    }
    var trigger by remember { mutableStateOf(initialTrigger) }
    var kind by remember { mutableStateOf(initialKind) }
    var selection by remember { mutableStateOf(selectedID) }
    var exporting by remember { mutableStateOf(false) }
    var proposed by remember { mutableStateOf<List<RecordingEpisode>?>(null) }
    var exportError by remember { mutableStateOf<String?>(null) }
    val filtered = library.episodes(trigger, kind)
    val selected = filtered.firstOrNull { it.id == selection }
    val incompleteFilter = selected == null && (trigger != null || kind != null) && library.labelCoverage != null
    val groups = filtered.groupBy { it.started?.let(::startOfDay) }.entries.sortedByDescending { it.key ?: Long.MIN_VALUE }
    val list = rememberLazyListState()
    LaunchedEffect(Unit) { selectedID?.let { id -> var index = 0; for (g in groups) { val at = g.value.indexOfFirst { it.id == id }; if (at >= 0) { list.scrollToItem(index + 1 + at); break }; index += 1 + g.value.size } } }
    fun export(items: List<RecordingEpisode>) {
        exporting = true
        scope.launch {
            try {
                val paths = model.readRecordings(library.device) { id -> model.backend.call("ExportRecordings", id, library.device.selector, library.device.requiresMTLS, JSONArray(items.map { it.id })) }
                val files = (paths as? JSONArray)?.let { a -> (0 until a.length()).map { File(a.getString(it)) } }.orEmpty()
                check(files.isNotEmpty() && files.size == items.size) { "The device returned an incomplete export." }
                shareRecordings(context, files, items)
            } catch (e: CancellationException) { throw e } catch (e: Exception) { exportError = PresentedFailure(e.message ?: "").details } finally { exporting = false }
        }
    }
    Column(Modifier.fillMaxSize()) {
        Column(Modifier.padding(horizontal = 18.dp).padding(top = 8.dp)) {
            Text("${library.episodes.size} ${if (library.episodes.size == 1) "episode" else "episodes"} · ${library.sizeText}", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        Box(Modifier.padding(horizontal = 6.dp)) { RecordingKindFilter(kind) { kind = it; if (selected == null) selection = null } }
        Row(Modifier.horizontalScroll(rememberScrollState()).padding(horizontal = 16.dp), horizontalArrangement = Arrangement.spacedBy(6.dp)) {
            FilterPill(null, library.episodes(null, kind).size, trigger == null) { trigger = null; if (selected == null) selection = null }
            library.labels.forEach { (name, _) -> FilterPill(name, library.episodes(name, kind).size, trigger == name) { trigger = name; if (selected == null) selection = null } }
        }
        LazyColumn(Modifier.weight(1f), state = list, contentPadding = PaddingValues(horizontal = 16.dp, vertical = 8.dp), verticalArrangement = Arrangement.spacedBy(0.dp)) {
            groups.forEach { (day, episodes) ->
                item(key = "day:$day") { Text(dayTitle(day), Modifier.padding(start = 6.dp, top = 14.dp, bottom = 6.dp), style = MaterialTheme.typography.labelMedium, fontWeight = FontWeight.SemiBold, color = MaterialTheme.colorScheme.onSurfaceVariant) }
                items(episodes, key = { it.id }) { episode ->
                    val first = episode.id == episodes.first().id; val last = episode.id == episodes.last().id
                    val shape = RoundedCornerShape(topStart = if (first) 20.dp else 0.dp, topEnd = if (first) 20.dp else 0.dp, bottomStart = if (last) 20.dp else 0.dp, bottomEnd = if (last) 20.dp else 0.dp)
                    Surface(shape = shape, color = MaterialTheme.colorScheme.surface) {
                        Column {
                            EpisodeRow(library.device, episode, selection == episode.id) { selection = episode.id }
                            if (!last) HorizontalDivider(Modifier.padding(start = 108.dp), color = MaterialTheme.colorScheme.outlineVariant)
                        }
                    }
                }
            }
            if (filtered.isEmpty()) item { EmptyMessage("No matching episodes", if (library.labelCoverage != null) "Some details are still missing. Wait or change the filters." else "Try a different kind or label.", Icons.Outlined.FilterList) }
            (catalog.detailProblems[targetID] ?: library.warning)?.let { warning ->
                item { SecondaryText(PresentedFailure(warning).details); TextButton(onClick = { refresh++ }) { Text("Try again") } }
            }
        }
        HorizontalDivider()
        when {
            exporting -> TextButton(onClick = {}, enabled = false, modifier = Modifier.padding(horizontal = 8.dp)) { Text("Exporting…") }
            library.labelCoverage != null -> SecondaryText(if (incompleteFilter) "${library.labelCoverage}. Load all details to export this filter." else library.labelCoverage!!, Modifier.padding(horizontal = 16.dp).padding(top = 4.dp))
        }
        Row(Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 10.dp), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            LensButton(if (exporting) "Exporting…" else "Export", enabled = filtered.isNotEmpty() && !incompleteFilter && !exporting, modifier = Modifier.weight(1f)) {
                val items = library.exportTargets(trigger, selection, kind)
                if (items.size > 1) proposed = items else if (items.isNotEmpty()) export(items)
            }
            LensButton(if (selected == null) "Pick an episode" else "Play episode", primary = true, enabled = selected != null && !exporting, modifier = Modifier.weight(1f)) {
                val episode = selected ?: return@LensButton
                nav.push(episode.title) { RecordingPlayback(model, library.device, episode.id) }
            }
        }
    }
    proposed?.let { items ->
        ConfirmAction("Export recordings?", "The selected filter includes ${RecordingLibrary.sizeText(items)} of saved recordings.", "Export ${items.size} episodes", destructive = false,
            onDismiss = { proposed = null }) { proposed = null; export(items) }
    }
    exportError?.let { message -> AlertDialog(onDismissRequest = { exportError = null }, title = { Text("Could not export recordings") }, text = { Text(message) }, confirmButton = { TextButton(onClick = { exportError = null }) { Text("OK") } }) }
}

@Composable
private fun FilterPill(name: String?, count: Int, active: Boolean, onClick: () -> Unit) {
    Box(Modifier.heightIn(min = 44.dp).clickable(onClick = onClick), contentAlignment = Alignment.Center) {
        Row(Modifier.background(if (active) recordingInk else MaterialTheme.colorScheme.surface, CircleShape).padding(horizontal = 12.dp, vertical = 7.dp),
            horizontalArrangement = Arrangement.spacedBy(6.dp), verticalAlignment = Alignment.CenterVertically) {
            Box(Modifier.size(7.dp).background(if (name == null && active) Color.White else labelColor(name), CircleShape))
            val color = if (active) Color.White else MaterialTheme.colorScheme.onSurface
            Text(name ?: "All", style = MaterialTheme.typography.labelLarge, fontWeight = FontWeight.SemiBold, color = color, maxLines = 1, overflow = TextOverflow.Ellipsis)
            Text("$count", style = MaterialTheme.typography.labelLarge, fontWeight = FontWeight.SemiBold, color = color.copy(alpha = .7f), maxLines = 1)
        }
    }
}

@Composable
private fun EpisodeRow(device: Device, episode: RecordingEpisode, picked: Boolean, onClick: () -> Unit) {
    Row(Modifier.fillMaxWidth().background(if (picked) LensStyle.green.copy(alpha = .09f) else Color.Transparent).clickable(onClick = onClick).padding(horizontal = 12.dp, vertical = 10.dp),
        horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
        RecordingThumbnail(device, episode, Modifier.size(84.dp, 54.dp).clip(RoundedCornerShape(10.dp)))
        Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(3.dp)) {
            Text(episode.title, style = MaterialTheme.typography.titleSmall, fontWeight = FontWeight.SemiBold, maxLines = 1, overflow = TextOverflow.Ellipsis)
            Text(listOfNotNull(episode.started?.let { DateFormat.getTimeInstance(DateFormat.SHORT).format(Date(it)) }, episode.duration?.let(::minuteSecond), recordingKindTitle(episode.kind)).joinToString(" · "),
                style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            episode.trigger?.let { label ->
                Row(horizontalArrangement = Arrangement.spacedBy(4.dp), verticalAlignment = Alignment.CenterVertically) {
                    Box(Modifier.size(6.dp).background(labelColor(label), CircleShape)); Text(label, style = MaterialTheme.typography.labelSmall, fontWeight = FontWeight.SemiBold, maxLines = 1, overflow = TextOverflow.Ellipsis)
                }
            }
        }
        if (picked) Icon(Icons.Outlined.Check, "Selected", tint = LensStyle.accent)
    }
}

fun minuteSecond(seconds: Double): String { val total = seconds.toLong(); return "%d:%02d".format(total / 60, total % 60) }

private fun startOfDay(millis: Long): Long = Calendar.getInstance().apply { timeInMillis = millis; set(Calendar.HOUR_OF_DAY, 0); set(Calendar.MINUTE, 0); set(Calendar.SECOND, 0); set(Calendar.MILLISECOND, 0) }.timeInMillis

private fun dayTitle(day: Long?): String = when {
    day == null -> "DATE UNKNOWN"
    DateUtils.isToday(day) -> "TODAY"
    DateUtils.isToday(day + DateUtils.DAY_IN_MILLIS) -> "YESTERDAY"
    else -> DateFormat.getDateInstance(DateFormat.LONG).format(Date(day)).uppercase()
}

/** Names each staged archive "001-Title.zip" and opens the share sheet (iOS MobileRecordingExport). */
private suspend fun shareRecordings(context: android.content.Context, files: List<File>, episodes: List<RecordingEpisode>) {
    val shared = withContext(Dispatchers.IO) {
        val directory = context.cacheDir.resolve("exports").apply { deleteRecursively(); mkdirs() }
        files.zip(episodes).mapIndexed { index, (source, episode) ->
            File(directory, "%03d-%s.zip".format(index + 1, exportTitle(episode.title))).also { source.copyTo(it, overwrite = true); source.delete() }
        }
    }
    val uris = ArrayList(shared.map { FileProvider.getUriForFile(context, "${context.packageName}.files", it) })
    val intent = if (uris.size == 1) Intent(Intent.ACTION_SEND).putExtra(Intent.EXTRA_STREAM, uris[0]) else Intent(Intent.ACTION_SEND_MULTIPLE).putParcelableArrayListExtra(Intent.EXTRA_STREAM, uris)
    context.startActivity(Intent.createChooser(intent.setType("application/zip").addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION), "Export recordings"))
}

/** Letters, digits, dash, underscore and space, up to 100 bytes; anything else becomes a dash. */
internal fun exportTitle(value: String): String {
    val safe = StringBuilder()
    for (ch in value) {
        val piece = if (ch.isLetterOrDigit() || ch in "-_ ") ch.toString() else "-"
        if (safe.toString().toByteArray().size + piece.toByteArray().size > 100) break
        safe.append(piece)
    }
    return safe.toString().ifEmpty { "Recording" }
}

/** Plays an episode read directly from its device, without changing the connected device. */
@Composable
fun RecordingPlayback(model: WendyModel, device: Device, episode: String) {
    EpisodeSnapshotViewer(model) { RecordingSnapshot(model.readRecordings(device) { id -> model.backend.call("PlayRecording", id, device.selector, device.requiresMTLS, episode).obj() }) }
}

