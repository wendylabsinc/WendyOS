package dev.wendy.android

import android.content.Context
import android.text.format.DateUtils
import androidx.compose.foundation.ExperimentalFoundationApi
import androidx.compose.foundation.background
import androidx.compose.foundation.combinedClickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
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
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.launch
import org.json.JSONArray
import org.json.JSONObject
import java.text.DateFormat
import java.util.Calendar
import java.util.Date

enum class ActivitySegment(val title: String) { Alerts("Alerts"), Events("Events"), Archived("Archived") }

/** What an archived alert still shows after its source is gone. */
data class AlertSummary(val id: String, val title: String, val deviceName: String, val date: Long?)
data class ArchivedAlert(val summary: AlertSummary, val archivedAt: Long)

/** Read and archived alerts for one account and organization, kept on this phone (iOS ActivityReceipts). */
data class ActivityReceipts(val read: Set<String> = emptySet(), val archived: Map<String, ArchivedAlert> = emptyMap(), val expired: Set<String> = emptySet()) {
    /** Archived alerts expire after 30 days and are never shown again. */
    fun expire(now: Long): ActivityReceipts {
        val cutoff = now - 30L * 24 * 3600 * 1000
        val old = archived.filterValues { it.archivedAt <= cutoff }.keys
        return if (old.isEmpty()) this else copy(archived = archived - old, expired = expired + old)
    }
    fun json(): String = JSONObject()
        .put("read", JSONArray(read.sorted()))
        .put("expired", JSONArray(expired.sorted()))
        .put("archived", JSONObject().also { o -> archived.forEach { (id, a) ->
            o.put(id, JSONObject().put("archivedAt", a.archivedAt).put("summary", JSONObject().put("id", a.summary.id).put("title", a.summary.title).put("deviceName", a.summary.deviceName).also { s -> a.summary.date?.let { s.put("date", it) } }))
        } }).toString()
    companion object {
        fun parse(text: String?): ActivityReceipts = runCatching {
            val value = JSONObject(text ?: return ActivityReceipts())
            fun strings(name: String) = value.optJSONArray(name)?.let { a -> (0 until a.length()).map { a.optString(it) }.toSet() }.orEmpty()
            val archived = value.optJSONObject("archived")?.let { o -> o.keys().asSequence().mapNotNull { id ->
                val entry = o.optJSONObject(id) ?: return@mapNotNull null
                val summary = entry.optJSONObject("summary") ?: return@mapNotNull null
                id to ArchivedAlert(AlertSummary(summary.optString("id", id), summary.optString("title"), summary.optString("deviceName"), if (summary.has("date")) summary.optLong("date") else null), entry.optLong("archivedAt"))
            }.toMap() }.orEmpty()
            ActivityReceipts(strings("read"), archived, strings("expired"))
        }.getOrDefault(ActivityReceipts())
        fun key(account: String, organization: String) = "wendy.activity.receipts.$account|$organization"
    }
}

/** An alert's identity includes its owning device, so the same clip ID on two devices stays separate. */
internal fun alertID(device: Device, id: String): String { val owner = recordingDeviceID(device); return "${owner.toByteArray().size}:$owner:$id" }

private class ActivityAlert(val device: Device, id: String, title: String, date: Long?, val episode: RecordingEpisode? = null, val appName: String? = null, val updateAvailable: Boolean = false) {
    val summary = AlertSummary(alertID(device, id), title, device.title, date)
    val id get() = summary.id
}

/**
 * Activity (iOS MobileDeviceActivity): alerts from campaign recordings, agent updates and crash loops,
 * the device events Wendy observed, and alerts archived on this phone.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun DeviceActivityScreen(model: WendyModel, segment: ActivitySegment, setSegment: (ActivitySegment) -> Unit, select: (Device) -> Unit) {
    val catalog = LocalRecordingCatalog.current ?: return
    val nav = LocalNavigator.current
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val preferences = remember { context.getSharedPreferences("activity", Context.MODE_PRIVATE) }
    val storageKey = ActivityReceipts.key(model.account, model.organization)
    var receipts by remember(storageKey) { mutableStateOf(ActivityReceipts.parse(preferences.getString(storageKey, null))) }
    fun update(edit: (ActivityReceipts) -> ActivityReceipts) {
        val next = edit(receipts)
        if (next != receipts) { receipts = next; preferences.edit().putString(storageKey, next.json()).apply() }
    }
    var refresh by remember { mutableIntStateOf(0) }
    var refreshing by remember { mutableStateOf(false) }
    var updateStatus by remember { mutableStateOf<UpdateSnapshot?>(null) }
    var updateOwner by remember { mutableLongStateOf(0L) }
    val visible = LocalPageVisible.current && model.inForeground
    val sources = RecordingSources.of(model)
    val active = visible && model.initialized && segment == ActivitySegment.Alerts
    LaunchedEffect(active, sources.targets, refresh) {
        if (visible) update { it.expire(System.currentTimeMillis()) }
        if (active) catalog.load(sources.targets, refresh, null, consumer = "activity")
    }
    val softwareSession = if (active && model.session != 0L && !model.lite) model.session else 0L
    LaunchedEffect(softwareSession) {
        updateStatus = null; updateOwner = 0L
        if (softwareSession == 0L) return@LaunchedEffect
        val status = try { UpdateSnapshot.parse(model.backend.call("OnlineUpdate", softwareSession, 0, "").obj()) } catch (e: CancellationException) { throw e } catch (_: Exception) { null }
        if (model.session == softwareSession) { updateStatus = status; updateOwner = softwareSession }
    }
    val alerts = buildList {
        catalog.reads.values.mapNotNull { it.library }.forEach { library ->
            library.episodes.forEach { episode -> episode.campaign?.let { campaign -> add(ActivityAlert(library.device, "clip." + episode.id, episode.trigger ?: campaign, episode.started, episode)) } }
        }
        val device = model.selected
        if (model.initialized && model.session != 0L && device != null) {
            val status = updateStatus
            if (updateOwner == model.session && status != null && status.available) add(ActivityAlert(device, "update." + status.latest, "Agent update available", null, updateAvailable = true))
            model.apps.filter { it.state == AppState.CrashLooping }.forEach { add(ActivityAlert(device, "crash." + it.name, "${it.name} keeps restarting", null, appName = it.name)) }
        }
    }.sortedWith(compareByDescending<ActivityAlert> { it.summary.date ?: Long.MAX_VALUE }.thenBy { it.id })
    val shown = alerts.filter { receipts.archived[it.id] == null && it.id !in receipts.expired }
    val unread = shown.filter { it.id !in receipts.read }
    SideEffect { catalog.unreadAlerts = unread.size }
    fun archive(alert: ActivityAlert) = update { it.copy(read = it.read + alert.id, archived = it.archived + (alert.id to ArchivedAlert(alert.summary, System.currentTimeMillis()))) }
    fun open(alert: ActivityAlert) { update { it.copy(read = it.read + alert.id) }; select(alert.device) }
    CompositionLocalProvider(LocalRecordingPosters provides if (visible) LocalRecordingPosters.current else null) {
        PullToRefreshBox(refreshing, onRefresh = { scope.launch { refreshing = true; refresh++; refreshing = false } }) {
            LazyColumn(Modifier.fillMaxSize(), contentPadding = PaddingValues(horizontal = 16.dp, vertical = 8.dp), verticalArrangement = Arrangement.spacedBy(10.dp)) {
                item(key = "segments") { ActivitySegments(segment, receipts.archived.size, setSegment) }
                when (segment) {
                    ActivitySegment.Alerts -> {
                        item(key = "header") {
                            Row(verticalAlignment = Alignment.CenterVertically) {
                                Text("ALERTS · ${unread.size} UNREAD", Modifier.weight(1f), style = MaterialTheme.typography.labelMedium, fontWeight = FontWeight.SemiBold, color = MaterialTheme.colorScheme.onSurfaceVariant)
                                TextButton(onClick = { update { it.copy(read = it.read + shown.map { a -> a.id }) } }, enabled = unread.isNotEmpty()) { Text("Mark all read", fontWeight = FontWeight.SemiBold) }
                            }
                        }
                        items(shown, key = { it.id }) { alert ->
                            SwipeToArchive(onArchive = { archive(alert) }) {
                                AlertCard(model, alert, alert.id !in receipts.read, updateOwner == model.session, onOpen = { open(alert) }, onArchive = { archive(alert) },
                                    onPlay = { episode ->
                                        update { it.copy(read = it.read + alert.id) }
                                        nav.push(alert.device.title) { EpisodePickerPage(model, recordingDeviceID(alert.device), alert.device, null, null, episode.id) }
                                    },
                                    onInspect = { app -> update { it.copy(read = it.read + alert.id) }; nav.push(app) { AppLensPage(model, alert.device, app) } },
                                    onReview = {
                                        update { it.copy(read = it.read + alert.id) }
                                        val session = model.session
                                        nav.push("Device update") { if (model.session == session && model.session != 0L) DeviceInfoPage(model, alert.device.title) else EmptyMessage("Device changed", "Open the device again to review its update.", Icons.Outlined.SyncProblem) }
                                    })
                            }
                        }
                        val partial = catalog.reads.values.any { it.library?.labelCoverage != null }
                        val incomplete = sources.unavailable > 0 || catalog.detailProblems.isNotEmpty() || catalog.reads.values.any { it.problem != null || it.library?.warning != null } || model.cloudError != null || model.nearbyError != null
                        val checking = catalog.loading || (model.organization.isNotEmpty() && !model.cloudLoaded && model.cloudError == null) || (!model.nearbyLoaded && model.nearbyError == null) || (partial && catalog.detailProblems.isEmpty())
                        item(key = "status") {
                            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                                when {
                                    checking && shown.isEmpty() -> Surface(shape = RoundedCornerShape(22.dp), color = MaterialTheme.colorScheme.surface) { LensLoading("Checking for alerts…") }
                                    checking -> LensLoading("Checking for alerts…")
                                    shown.isEmpty() -> EmptyMessage(if (incomplete) "No alerts from available devices" else "No alerts", "", Icons.Outlined.Notifications)
                                }
                                if (incomplete) Row(verticalAlignment = Alignment.CenterVertically) {
                                    Text("Some devices or recordings could not be checked.", Modifier.weight(1f), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                                    TextButton(onClick = { refresh++ }) { Text("Retry", fontWeight = FontWeight.Bold) }
                                }
                            }
                        }
                    }
                    ActivitySegment.Events -> {
                        val devices = model.nearbyDevices + model.cloudDevices + listOfNotNull(model.selected)
                        val days = model.experienceEvents.groupBy { startOfEventDay(it.time) }.entries.sortedByDescending { it.key }
                        if (days.isEmpty()) item(key = "noevents") { EmptyMessage("No device events yet", "", Icons.Outlined.Schedule) }
                        days.forEach { (day, events) ->
                            item(key = "day:$day") {
                                LensSection(eventDayTitle(day)) {
                                    events.sortedByDescending { it.time }.forEachIndexed { index, event ->
                                        if (index > 0) LensDivider()
                                        val device = devices.firstOrNull { it.id == event.deviceID }
                                        LensRow(event.title, event.deviceName, eventIcon(event.kind), trailing = DateFormat.getTimeInstance(DateFormat.SHORT).format(Date(event.time)),
                                            navigates = false, enabled = device != null) { device?.let(select) }
                                    }
                                }
                            }
                        }
                    }
                    ActivitySegment.Archived -> {
                        val entries = receipts.archived.values.sortedByDescending { it.archivedAt }
                        if (entries.isEmpty()) item(key = "noarchive") { EmptyMessage("Nothing archived", "Swipe left on an alert to archive it.", Icons.Outlined.Archive) }
                        else item(key = "note") { SecondaryText("Archived alerts stay on this phone for 30 days. Their recordings stay in Data.") }
                        items(entries, key = { "archived:" + it.summary.id }) { entry ->
                            Surface(shape = RoundedCornerShape(18.dp), color = MaterialTheme.colorScheme.surface) {
                                Row(Modifier.padding(14.dp), verticalAlignment = Alignment.CenterVertically) {
                                    Column(Modifier.weight(1f)) {
                                        Text(entry.summary.title, style = MaterialTheme.typography.titleMedium, maxLines = 3, overflow = TextOverflow.Ellipsis)
                                        Text(entry.summary.deviceName, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 2, overflow = TextOverflow.Ellipsis)
                                    }
                                    TextButton(onClick = { update { it.copy(archived = it.archived - entry.summary.id) } }) { Text("Restore") }
                                }
                            }
                        }
                    }
                }
                item(key = "end") { Spacer(Modifier.height(16.dp)) }
            }
        }
    }
}

@Composable
private fun ActivitySegments(segment: ActivitySegment, archived: Int, select: (ActivitySegment) -> Unit) {
    Row(Modifier.fillMaxWidth().background(MaterialTheme.colorScheme.onSurface.copy(alpha = .08f), CircleShape).padding(2.dp)) {
        ActivitySegment.entries.forEach { tab ->
            val selected = tab == segment
            Surface(onClick = { select(tab) }, Modifier.weight(1f).heightIn(min = 34.dp), shape = CircleShape, color = if (selected) LensStyle.green else Color.Transparent) {
                Box(contentAlignment = Alignment.Center) {
                    Text(if (tab == ActivitySegment.Archived && archived > 0) "Archived $archived" else tab.title, style = MaterialTheme.typography.labelLarge,
                        fontWeight = if (selected) FontWeight.SemiBold else FontWeight.Medium, color = if (selected) Color.White else MaterialTheme.colorScheme.onSurface)
                }
            }
        }
    }
}

/** Swiping an alert to the left archives it (iOS swipe action). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun SwipeToArchive(onArchive: () -> Unit, content: @Composable () -> Unit) {
    val state = rememberSwipeToDismissBoxState(confirmValueChange = { value -> if (value == SwipeToDismissBoxValue.EndToStart) { onArchive(); true } else false })
    SwipeToDismissBox(state, enableDismissFromStartToEnd = false, backgroundContent = {
        Box(Modifier.fillMaxSize().clip(RoundedCornerShape(22.dp)).background(LensStyle.offline).padding(horizontal = 20.dp), contentAlignment = Alignment.CenterEnd) {
            Row(horizontalArrangement = Arrangement.spacedBy(6.dp), verticalAlignment = Alignment.CenterVertically) { Icon(Icons.Outlined.Archive, null, tint = Color.White); Text("Archive", color = Color.White) }
        }
    }) { content() }
}

@OptIn(ExperimentalFoundationApi::class)
@Composable
private fun AlertCard(model: WendyModel, alert: ActivityAlert, unread: Boolean, updateReady: Boolean, onOpen: () -> Unit, onArchive: () -> Unit,
                      onPlay: (RecordingEpisode) -> Unit, onInspect: (String) -> Unit, onReview: () -> Unit) {
    var menu by remember { mutableStateOf(false) }
    val episode = alert.episode
    Surface(shape = RoundedCornerShape(22.dp), color = MaterialTheme.colorScheme.surface) {
        Column(Modifier.padding(14.dp)) {
            Box {
                Row(Modifier.fillMaxWidth().combinedClickable(onClick = onOpen, onLongClick = { menu = true }), horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
                    LensIcon(if (episode == null) Icons.Outlined.WarningAmber else if (episode.kind == "audio") Icons.Outlined.GraphicEq else Icons.Outlined.PhotoCamera,
                        if (episode == null) LensStyle.warning else LensStyle.green)
                    Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(3.dp)) {
                        Text(alert.summary.title, style = MaterialTheme.typography.titleMedium, maxLines = 2, overflow = TextOverflow.Ellipsis)
                        Text(listOfNotNull(alert.device.title, episode?.campaign).joinToString(" · "), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 2, overflow = TextOverflow.Ellipsis)
                    }
                    Column(horizontalAlignment = Alignment.End, verticalArrangement = Arrangement.spacedBy(4.dp)) {
                        alert.summary.date?.let { Text(recordingTime(it), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant) }
                        if (unread) Box(Modifier.size(9.dp).background(LensStyle.green, CircleShape))
                    }
                }
                DropdownMenu(menu, onDismissRequest = { menu = false }) {
                    DropdownMenuItem(text = { Text("Archive") }, leadingIcon = { Icon(Icons.Outlined.Archive, null) }, onClick = { menu = false; onArchive() })
                }
            }
            if (episode != null) {
                Box(Modifier.padding(top = 10.dp).fillMaxWidth().height(if (episode.kind == "audio") 96.dp else 180.dp).clip(RoundedCornerShape(16.dp))
                    .combinedClickable(onClick = { onPlay(episode) }, onLongClick = { menu = true }, onClickLabel = "Play ${episode.title}")) {
                    RecordingThumbnail(alert.device, episode, Modifier.fillMaxSize())
                    Box(Modifier.align(Alignment.Center).size(50.dp).background(Color.White.copy(alpha = .9f), CircleShape), contentAlignment = Alignment.Center) {
                        Icon(Icons.Outlined.PlayArrow, null, tint = Color(0xFF171C23))
                    }
                    episode.trigger?.let { Badge(it, Modifier.align(Alignment.TopStart)) }
                    episode.duration?.let { Badge(minuteSecond(it), Modifier.align(Alignment.BottomEnd)) }
                    episode.synced?.let { synced ->
                        Box(Modifier.align(Alignment.TopEnd).padding(8.dp).background(Color.Black.copy(alpha = .65f), CircleShape).padding(5.dp)) {
                            Icon(if (synced) Icons.Outlined.CloudDone else Icons.Outlined.SdStorage, if (synced) "In Wendy Cloud" else "On the device", Modifier.size(14.dp), tint = Color.White)
                        }
                    }
                }
            } else {
                Row(Modifier.padding(top = 8.dp), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    OutlinedButton(onClick = onOpen) { Text("Open device") }
                    alert.appName?.let { app -> OutlinedButton(onClick = { onInspect(app) }) { Text("Inspect app") } }
                    if (alert.updateAvailable && updateReady) OutlinedButton(onClick = onReview) { Text("Review update") }
                }
            }
        }
    }
}

@Composable
private fun Badge(value: String, modifier: Modifier) {
    Text(value, modifier.padding(8.dp).background(Color.Black.copy(alpha = .7f), CircleShape).padding(horizontal = 7.dp, vertical = 4.dp),
        style = MaterialTheme.typography.labelSmall, fontWeight = FontWeight.Bold, color = Color.White, maxLines = 1)
}

private fun eventIcon(kind: String): ImageVector = when (kind) { "connected" -> Icons.Outlined.CheckCircle; "app" -> Icons.Outlined.Layers; else -> Icons.Outlined.Bolt }

private fun startOfEventDay(millis: Long): Long = Calendar.getInstance().apply { timeInMillis = millis; set(Calendar.HOUR_OF_DAY, 0); set(Calendar.MINUTE, 0); set(Calendar.SECOND, 0); set(Calendar.MILLISECOND, 0) }.timeInMillis

private fun eventDayTitle(day: Long): String = when {
    DateUtils.isToday(day) -> "Today"
    DateUtils.isToday(day + DateUtils.DAY_IN_MILLIS) -> "Yesterday"
    else -> DateFormat.getDateInstance(DateFormat.MEDIUM).format(Date(day))
}
