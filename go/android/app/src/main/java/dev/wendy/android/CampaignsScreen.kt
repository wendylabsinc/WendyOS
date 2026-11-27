package dev.wendy.android

import android.content.Intent
import android.net.Uri
import android.text.format.DateUtils
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.material.icons.automirrored.outlined.*
import androidx.compose.material3.*
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import org.json.JSONArray
import org.json.JSONObject
import java.text.DateFormat
import java.util.Date

data class CampaignInfo(val name: String, val state: String, val summary: String, val inferenceState: String, val problem: String, val warnings: String) {
    companion object {
        fun parse(value: JSONObject) = CampaignInfo(value.text("Name"), value.text("State"), value.text("Summary"), value.text("InferenceState"), value.text("Problem"),
            value.optJSONArray("Warnings")?.let { a -> (0 until a.length()).joinToString("\n") { a.optString(it) } } ?: "")
    }
}

data class CampaignClip(val id: String, val campaign: String, val state: String, val reason: String, val startedNanos: Long, val bytes: Long, val duration: Double) {
    val started get() = startedNanos / 1_000_000
    /** Minutes and seconds, or a dash while recording or when unknown (iOS durationLabel). */
    val durationLabel: String get() = if (duration < 0) "—" else (duration.toLong()).let { "%d:%02d".format(it / 60, it % 60) }
    companion object {
        fun parse(value: JSONObject) = CampaignClip(value.text("ID"), value.text("Campaign"), value.text("State"), value.text("Reason"), value.number("Started"), value.number("Size"),
            if (value.has("Duration")) value.optDouble("Duration", -1.0) else -1.0)
        fun parseAll(value: JSONObject) = value.list("Items").map(::parse).associateBy { it.id }.values
            .sortedWith(compareByDescending<CampaignClip> { it.startedNanos }.thenBy { it.id })
    }
}

/** A new campaign, with the iOS defaults and validity rules (iOS CampaignDraft). */
data class CampaignDraft(
    val name: String = "human-detected", val camera: String = "*", val audio: Set<String> = emptySet(), val ros2: Set<String> = emptySet(), val telemetry: Boolean = false,
    val trigger: String = "detection", val model: String = "facebook/detr-resnet-50", val modelRevision: String = "", val labels: Set<String> = setOf("person"),
    val notify: Boolean = true, val event: String = "model_matched", val threshold: Double = 0.9, val cooldown: Int = 30, val before: Int = 10, val after: Int = 10, val upload: Boolean = false,
) {
    val valid: Boolean get() = name.isNotBlank() && (camera.isNotEmpty() || (trigger == "event" && (audio.isNotEmpty() || ros2.isNotEmpty() || telemetry))) &&
        event.isNotBlank() && (trigger == "event" || (model.isNotEmpty() && modelRevision.length == 40 && labels.isNotEmpty() && labels.size <= 32))
    /** Arguments for the shared DesktopCampaignInput. */
    fun input(): JSONObject = json("Name" to name.trim(), "Camera" to camera, "Event" to event.trim(), "RunInference" to (trigger != "event"), "Classification" to (trigger == "classification"),
        "Model" to model, "ModelRevision" to modelRevision, "Labels" to JSONArray(labels.sorted()), "Audio" to JSONArray(audio.sorted()), "ROS2" to JSONArray(ros2.sorted()),
        "Telemetry" to telemetry, "Notify" to notify, "Upload" to upload, "Threshold" to threshold, "Cooldown" to cooldown, "Before" to before, "After" to after)
    /** Switching trigger resets the model to the default for that task, as on iOS. */
    fun withTrigger(value: String) = when (value) {
        "classification" -> copy(trigger = value, model = "google/mobilenet_v2_1.0_224", labels = emptySet(), modelRevision = "")
        "detection" -> copy(trigger = value, model = "facebook/detr-resnet-50", labels = setOf("person"), modelRevision = "")
        else -> copy(trigger = value, modelRevision = "", camera = camera)
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun CampaignsScreen(model: WendyModel) {
    val nav = LocalNavigator.current
    val session = model.session
    val scope = rememberCoroutineScope()
    val name = model.selected?.title ?: ""
    var campaigns by remember { mutableStateOf(emptyList<CampaignInfo>()) }
    var clips by remember { mutableStateOf(emptyList<CampaignClip>()) }
    var clipWarning by remember { mutableStateOf("") }
    var clipError by remember { mutableStateOf<String?>(null) }
    var loading by remember { mutableStateOf(false) }
    var loadingClips by remember { mutableStateOf(true) }
    var error by remember { mutableStateOf<String?>(null) }
    var refreshing by remember { mutableStateOf(false) }
    suspend fun refresh() {
        loading = true; error = null; loadingClips = true; clipError = null
        try { campaigns = model.backend.call("Campaigns", session).items().map(CampaignInfo::parse) } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message }
        try { val value = model.backend.call("CampaignClips", session).obj(); clips = CampaignClip.parseAll(value); clipWarning = value.text("Warning") }
        catch (e: CancellationException) { throw e } catch (e: Exception) { clipError = e.message } finally { loadingClips = false }
        loading = false
    }
    fun clipCount(campaign: String): String {
        if (clipError != null) return "Clips unavailable"
        val count = clips.count { it.campaign == campaign }
        return "$count${if (clipWarning.isEmpty()) "" else "+"} ${if (count == 1 && clipWarning.isEmpty()) "clip" else "clips"}"
    }
    val reload = { scope.launch { refresh() }; Unit }
    LaunchedEffect(session) { refresh() }
    PullToRefreshBox(refreshing, onRefresh = { scope.launch { refreshing = true; refresh(); refreshing = false } }) {
        PageColumn {
            item {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    PageTitle("Campaigns", "Record device data when an event occurs")
                    Spacer(Modifier.weight(1f))
                    IconButton(onClick = { nav.push("New campaign") { CampaignEditorPage(model, name) { reload() } } }) { Icon(Icons.Outlined.Add, "Create campaign") }
                }
            }
            formSection {
                campaigns.forEach { campaign ->
                    Row(Modifier.fillMaxWidth().clickable { nav.push(campaign.name, { CampaignSettingsButton(model, campaign.name, reload) }) { CampaignDetailPage(model, campaign) } }.padding(vertical = 10.dp),
                        verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(5.dp)) {
                            Text(campaign.name, style = MaterialTheme.typography.titleMedium)
                            SecondaryText(campaign.summary)
                            Row(horizontalArrangement = Arrangement.spacedBy(6.dp), verticalAlignment = Alignment.CenterVertically) {
                                Box(Modifier.size(6.dp).background(if (campaign.state == "armed") MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurfaceVariant, CircleShape))
                                Text(campaign.state.replaceFirstChar { it.uppercase() } + if (!loadingClips) " · ${clipCount(campaign.name)}" else "", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                            }
                            if (campaign.problem.isNotEmpty()) Text(campaign.problem, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error, maxLines = 2)
                        }
                        Icon(Icons.Outlined.ChevronRight, null, Modifier.size(20.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                }
                if (campaigns.isEmpty() && !loading) {
                    EmptyMessage("No campaigns", "Create a campaign to record when an event happens.", Icons.Outlined.Bolt)
                    ActionRow("Create campaign", Icons.Outlined.Add) { nav.push("New campaign") { CampaignEditorPage(model, name) { reload() } } }
                }
                if (loading && campaigns.isEmpty()) LoadingRow("Loading campaigns…")
                error?.let { FailureNote(it) }
            }
            formSection {
                NavRow("Event log", Icons.Outlined.Notifications) { nav.push("Event log") { CampaignEventLogPage(model, rememberCampaignEvents(model)) } }
                NavRow("All recordings", Icons.Outlined.VideoLibrary) { nav.push("All recordings") { RecordingsScreen(model) } }
            }
            item { SecondaryText("Rules run on this device. Notification delivery also requires an enabled grant for the campaign in Wendy Cloud Apps.") }
        }
    }
}

@Composable
private fun CampaignSettingsButton(model: WendyModel, name: String, saved: () -> Unit) {
    val nav = LocalNavigator.current
    IconButton(onClick = { nav.push("Campaign settings") { CampaignSettingsPage(model, name, saved) } }) { Icon(Icons.Outlined.Settings, "Campaign settings") }
}

/** One campaign and its recordings (iOS CampaignRecordingsView). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun CampaignDetailPage(model: WendyModel, initial: CampaignInfo) {
    val nav = LocalNavigator.current
    val session = model.session
    val scope = rememberCoroutineScope()
    var campaign by remember { mutableStateOf(initial) }
    var clips by remember { mutableStateOf(emptyList<CampaignClip>()) }
    var warning by remember { mutableStateOf("") }
    var error by remember { mutableStateOf<String?>(null) }
    var loading by remember { mutableStateOf(true) }
    var refreshing by remember { mutableStateOf(false) }
    suspend fun refresh() {
        loading = true; error = null
        try {
            val value = model.backend.call("CampaignClips", session).obj()
            clips = CampaignClip.parseAll(value).filter { it.campaign == campaign.name }; warning = value.text("Warning")
            model.backend.call("Campaigns", session).items().map(CampaignInfo::parse).firstOrNull { it.name == campaign.name }?.let { campaign = it }
        } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { loading = false }
    }
    LaunchedEffect(session) { refresh() }
    PullToRefreshBox(refreshing, onRefresh = { scope.launch { refreshing = true; refresh(); refreshing = false } }) {
        PageColumn {
            formSection {
                LabeledRow("Status", campaign.state.replaceFirstChar { it.uppercase() })
                SecondaryText(campaign.summary)
                if (campaign.inferenceState.isNotEmpty()) SecondaryText("Detection: ${campaign.inferenceState}")
                if (campaign.problem.isNotEmpty()) FailureNote(campaign.problem)
                if (campaign.warnings.isNotEmpty()) Text(campaign.warnings, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error)
            }
            formSection("Recordings") {
                if (loading && clips.isEmpty()) LoadingRow("Loading recordings…")
                error?.let { FailureNote(it) }
                SecondaryText(warning)
                clips.forEach { clip -> ClipRow(clip) { nav.push("Recording") { EpisodeScreen(model, clip.id) } } }
                if (!loading && clips.isEmpty() && error == null) EmptyMessage("No recordings yet", "Recordings appear here when this campaign triggers.", Icons.Outlined.VideoLibrary)
            }
        }
    }
}

@Composable
private fun ClipRow(clip: CampaignClip, unread: Boolean = false, showCampaign: Boolean = false, open: () -> Unit) {
    Row(Modifier.fillMaxWidth().clickable(onClick = open).padding(vertical = 8.dp), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
        if (showCampaign) Icon(Icons.Outlined.VideoCameraBack, null, tint = MaterialTheme.colorScheme.primary)
        Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(3.dp)) {
            Row(horizontalArrangement = Arrangement.spacedBy(6.dp), verticalAlignment = Alignment.CenterVertically) {
                Text(if (showCampaign) clip.campaign else DateFormat.getDateTimeInstance(DateFormat.MEDIUM, DateFormat.SHORT).format(Date(clip.started)), style = MaterialTheme.typography.titleSmall, maxLines = 1)
                if (unread) Box(Modifier.size(6.dp).background(MaterialTheme.colorScheme.error, CircleShape))
            }
            if (clip.reason.isNotEmpty()) Text(clip.reason, style = MaterialTheme.typography.bodySmall, maxLines = 2)
            if (showCampaign) SecondaryText(DateFormat.getDateTimeInstance(DateFormat.MEDIUM, DateFormat.MEDIUM).format(Date(clip.started)))
            SecondaryText("${clip.durationLabel} · ${formatBytes(clip.bytes)} · ${clip.state.replaceFirstChar { it.uppercase() }}")
        }
        Icon(Icons.Outlined.PlayCircleOutline, "Open recording", tint = MaterialTheme.colorScheme.primary)
    }
}

/** Saved clip IDs a person has seen, per device; read receipts never depend on a connection. */
class CampaignEventStore(private val model: WendyModel, deviceID: String) {
    private val preferences = model.getApplication<WendyApplication>().getSharedPreferences("campaign-events", 0)
    private val key = "read.$deviceID"
    var clips by mutableStateOf(emptyList<CampaignClip>()); private set
    var readIDs by mutableStateOf(preferences.getStringSet(key, emptySet())!!.toSet()); private set
    var loading by mutableStateOf(false); private set
    var error by mutableStateOf<String?>(null); private set
    var warning by mutableStateOf(""); private set
    private var lastEpisodes: List<String>? = null
    val hasUnread get() = clips.any { it.id !in readIDs }
    fun markRead(ids: Collection<String>) { readIDs = readIDs + ids; preferences.edit().putStringSet(key, readIDs).apply() }
    /** Polls the episode index first and fetches clip manifests only when recordings change. */
    suspend fun refresh(session: Long, force: Boolean = true) {
        if (loading) return
        loading = true
        try {
            var episodes: List<String>? = null
            if (!force) {
                episodes = model.backend.call("VisualEpisodes", session).items().map { "${it.text("id")}/${it.text("state")}/${it.number("size_bytes")}" }.sorted()
                if (episodes == lastEpisodes && error == null && warning.isEmpty()) return
            }
            val value = model.backend.call("CampaignClips", session).obj()
            clips = CampaignClip.parseAll(value); warning = value.text("Warning"); error = null
            lastEpisodes = episodes
        } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { loading = false }
    }
}

@Composable fun rememberCampaignEvents(model: WendyModel): CampaignEventStore {
    val id = model.selected?.id ?: ""
    return remember(id) { CampaignEventStore(model, id) }
}

/** The toolbar bell: unread marker, recent events and the full event log (iOS CampaignEventButton). */
@Composable fun CampaignEventButton(model: WendyModel) {
    val nav = LocalNavigator.current
    val session = model.session
    val events = rememberCampaignEvents(model)
    var recent by remember { mutableStateOf(false) }
    LaunchedEffect(session, model.foregroundGeneration) { while (true) { events.refresh(session, force = false); delay(5000) } }
    LaunchedEffect(recent, events.clips) { if (recent) events.markRead(events.clips.map { it.id }) }
    Box {
        IconButton(onClick = { recent = true }) {
            BadgedBox(badge = { if (events.hasUnread) Badge() }) { Icon(Icons.Outlined.Notifications, if (events.hasUnread) "Event log, new activity" else "Event log") }
        }
        DropdownMenu(recent, onDismissRequest = { recent = false }, modifier = Modifier.widthIn(min = 280.dp, max = 360.dp)) {
            Row(Modifier.padding(horizontal = 16.dp, vertical = 8.dp), verticalAlignment = Alignment.CenterVertically) {
                Text("Recent activity", Modifier.weight(1f), style = MaterialTheme.typography.titleSmall)
                if (events.loading) CircularProgressIndicator(Modifier.size(16.dp), strokeWidth = 2.dp)
            }
            events.error?.let { Text("Could not refresh events: $it", Modifier.padding(horizontal = 16.dp), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error) }
            if (events.clips.isEmpty() && !events.loading) Text("No events yet. Campaign recordings will appear here.", Modifier.padding(16.dp), style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            events.clips.take(3).forEach { clip ->
                HorizontalDivider()
                Box(Modifier.padding(horizontal = 16.dp)) { ClipRow(clip, showCampaign = true) { recent = false; nav.push("Recording") { EpisodeScreen(model, clip.id) } } }
            }
            HorizontalDivider()
            DropdownMenuItem(text = { Text("More…") }, onClick = { recent = false; nav.push("Event log") { CampaignEventLogPage(model, events) } })
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun CampaignEventLogPage(model: WendyModel, events: CampaignEventStore) {
    val nav = LocalNavigator.current
    val session = model.session
    val scope = rememberCoroutineScope()
    var search by remember { mutableStateOf("") }
    var refreshing by remember { mutableStateOf(false) }
    // Opening the log marks what is already listed as read; new arrivals keep their marker until opened.
    val seen = remember { events.clips.map { it.id }.toSet() + events.readIDs }
    LaunchedEffect(Unit) { events.markRead(events.clips.map { it.id }); events.refresh(session) }
    val filtered = events.clips.filter { search.isEmpty() || "${it.campaign} ${it.reason}".contains(search, true) }
    PullToRefreshBox(refreshing, onRefresh = { scope.launch { refreshing = true; events.refresh(session); refreshing = false } }) {
        PageColumn {
            events.error?.let { item { FormSection { FailureNote("Could not refresh events: $it") } } }
            if (events.warning.isNotEmpty()) item { SecondaryText(events.warning) }
            if (events.clips.isEmpty() && !events.loading) { item { EmptyMessage("No events yet", "Campaign recordings on ${model.selected?.title ?: "this device"} will appear here.", Icons.Outlined.Notifications) }; return@PageColumn }
            item { OutlinedTextField(search, { search = it }, Modifier.fillMaxWidth(), singleLine = true, placeholder = { Text("Search campaigns and events") }, leadingIcon = { Icon(Icons.Outlined.Search, null) }) }
            formSection {
                filtered.forEach { clip -> ClipRow(clip, unread = clip.id !in seen && clip.id !in events.readIDs, showCampaign = true) { events.markRead(listOf(clip.id)); nav.push("Recording") { EpisodeScreen(model, clip.id) } } }
                if (filtered.isEmpty()) SecondaryText("No matching events")
            }
        }
    }
}

// MARK: Settings

@Composable
private fun CampaignSettingsPage(model: WendyModel, name: String, saved: () -> Unit) {
    val nav = LocalNavigator.current
    val session = model.session
    val scope = rememberCoroutineScope()
    var original by remember { mutableStateOf<JSONObject?>(null) }
    var before by remember { mutableStateOf("") }
    var after by remember { mutableStateOf("") }
    var upload by remember { mutableStateOf("") }
    var cooldown by remember { mutableStateOf("") }
    var threshold by remember { mutableDoubleStateOf(0.9) }
    var error by remember { mutableStateOf<String?>(null) }
    var saving by remember { mutableStateOf(false) }
    LaunchedEffect(session, name) {
        try {
            val value = model.backend.call("CampaignSettings", session, name).obj()
            original = value; before = value.text("Before"); after = value.text("After"); upload = value.text("Upload"); cooldown = value.text("Cooldown"); threshold = value.optDouble("Threshold", 0.9)
        } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message }
    }
    val base = original
    val inference = base?.flag("Inference") == true
    val changed = base != null && (before != base.text("Before") || after != base.text("After") || upload != base.text("Upload") || cooldown != base.text("Cooldown") || threshold != base.optDouble("Threshold", 0.9))
    PageColumn {
        if (base == null) { item { if (error == null) LoadingRow("Loading settings…") else FormSection { FailureNote(error!!) } }; return@PageColumn }
        formSection("Campaign") { Text(name, Modifier.padding(vertical = 8.dp)) }
        formSection("Recording", "How much video to keep before and after the event. Enter a duration such as 10s or 1m.") {
            DurationField(if (inference) "Before detection" else "Before trigger", before, !saving) { before = it }
            DurationField(if (inference) "After detection" else "After trigger", after, !saving) { after = it }
        }
        formSection("Upload recordings") { ChoiceField("Upload", listOf("manual" to "Manually", "wifi" to "On Wi-Fi", "always" to "Always"), upload, !saving) { upload = it } }
        if (inference) formSection("Detection") {
            LabeledRow("Confidence", "%.0f%%".format(threshold * 100))
            Slider(threshold.toFloat(), { threshold = Math.round(it * 100) / 100.0 }, valueRange = 0.01f..1f, enabled = !saving)
            DurationField("Time between alerts", cooldown, !saving) { cooldown = it }
        }
        error?.let { formSection { FailureNote(it) } }
        item {
            Button(onClick = {
                saving = true; error = null
                scope.launch {
                    try {
                        val updated = JSONObject(base.toString()).put("Before", before).put("After", after).put("Upload", upload).put("Cooldown", cooldown).put("Threshold", threshold)
                        model.backend.call("UpdateCampaignSettings", session, updated); saved(); nav.pop()
                    } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { saving = false }
                }
            }, Modifier.fillMaxWidth(), enabled = changed && !saving) { Text(if (saving) "Saving…" else "Save") }
        }
    }
}

@Composable
private fun DurationField(title: String, value: String, enabled: Boolean, onChange: (String) -> Unit) {
    Row(Modifier.fillMaxWidth().padding(vertical = 4.dp), verticalAlignment = Alignment.CenterVertically) {
        Text(title, Modifier.weight(1f), style = MaterialTheme.typography.bodyLarge)
        OutlinedTextField(value, onChange, Modifier.width(120.dp), singleLine = true, enabled = enabled, placeholder = { Text("10s") },
            keyboardOptions = KeyboardOptions(autoCorrectEnabled = false, keyboardType = KeyboardType.Ascii))
    }
}

// MARK: New campaign

private val detectionModels = listOf("People, animals & everyday objects · Standard" to "facebook/detr-resnet-50", "People, animals & everyday objects · Compact" to "hustvl/yolos-tiny",
    "People, animals & everyday objects · Large" to "facebook/detr-resnet-101")
private val classificationModels = listOf("Animals, plants & everyday objects · Compact" to "google/mobilenet_v2_1.0_224", "Animals, plants & everyday objects · Standard" to "google/vit-base-patch16-224")
private fun shortModelName(repo: String) = when (repo) {
    "facebook/detr-resnet-50" -> "DETR · Standard"; "facebook/detr-resnet-101" -> "DETR · Large"; "hustvl/yolos-tiny" -> "YOLOS · Compact"
    "google/mobilenet_v2_1.0_224" -> "MobileNet V2"; "google/vit-base-patch16-224" -> "Vision Transformer"; else -> repo.substringAfterLast('/')
}

@Composable
private fun CampaignEditorPage(model: WendyModel, deviceName: String, deployed: () -> Unit) {
    val nav = LocalNavigator.current
    val session = model.session
    val scope = rememberCoroutineScope()
    var draft by remember { mutableStateOf(CampaignDraft()) }
    var sources by remember { mutableStateOf(emptyList<JSONObject>()) }
    var sourceError by remember { mutableStateOf<String?>(null) }
    var loading by remember { mutableStateOf(true) }
    var saving by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    var vision by remember { mutableStateOf<JSONObject?>(null) }
    var visionLoading by remember { mutableStateOf(false) }
    var visionError by remember { mutableStateOf<String?>(null) }
    var openSources by remember { mutableStateOf(setOf<String>()) }
    LaunchedEffect(session) {
        try { sources = model.backend.call("CampaignSources", session).items() } catch (e: CancellationException) { throw e } catch (e: Exception) { sourceError = e.message } finally { loading = false }
    }
    // Resolves the model's exact revision and categories; a campaign pins that revision.
    LaunchedEffect(draft.model, draft.trigger) {
        if (draft.trigger == "event") return@LaunchedEffect
        val repo = draft.model; val trigger = draft.trigger
        visionLoading = true; visionError = null; vision = null; draft = draft.copy(modelRevision = "")
        try {
            delay(300)
            val value = model.backend.call("VisionModel", repo, trigger == "classification").obj()
            if (repo == draft.model && trigger == draft.trigger) {
                vision = value
                val labels = value.optJSONArray("Labels")?.let { a -> (0 until a.length()).map(a::optString).toSet() } ?: emptySet()
                draft = draft.copy(modelRevision = value.text("Revision"), labels = draft.labels.intersect(labels))
            }
        } catch (e: CancellationException) { throw e } catch (e: Exception) { visionError = e.message } finally { visionLoading = false }
    }
    val editable = !saving
    PageColumn {
        formSection { OutlinedTextField(draft.name, { draft = draft.copy(name = it) }, Modifier.fillMaxWidth(), label = { Text("Campaign name") }, singleLine = true, enabled = editable, keyboardOptions = KeyboardOptions(autoCorrectEnabled = false)) }
        formSection("Trigger") {
            ChoiceField("When", listOf("event" to "App event", "classification" to "Image classification", "detection" to "Object detection"), draft.trigger, editable) { draft = draft.withTrigger(it) }
            if (draft.trigger == "event") OutlinedTextField(draft.event, { draft = draft.copy(event = it) }, Modifier.fillMaxWidth(), label = { Text("Event name") }, singleLine = true, enabled = editable, keyboardOptions = KeyboardOptions(autoCorrectEnabled = false))
            else {
                NavRow("Model", value = shortModelName(draft.model), enabled = editable) {
                    nav.push("Choose a model") { ModelChoicePage(if (draft.trigger == "classification") classificationModels else detectionModels, draft.model) { draft = draft.copy(model = it) } }
                }
                if (visionLoading) LoadingRow("Loading categories…")
                vision?.let { info ->
                    val labels = info.optJSONArray("Labels")?.let { a -> (0 until a.length()).map(a::optString) } ?: emptyList()
                    NavRow("Detect", value = if (draft.labels.isEmpty()) "Choose categories" else draft.labels.sorted().joinToString(", ") { it.replaceFirstChar { c -> c.uppercase() } }, enabled = editable) {
                        nav.push("Detect categories") { LabelChoicePage(labels, { draft.labels }) { draft = draft.copy(labels = it) } }
                    }
                    NavRow("Model details") { nav.push("Model details") { ModelDetailsPage(draft.model, info, labels.size) } }
                    SecondaryText(if (draft.trigger == "classification") "Recognizes ${labels.size} categories across the whole image. Only your selected categories trigger an event."
                        else "Finds ${labels.size} kinds of people, animals and objects in camera frames. Only your selected categories trigger an event.")
                }
                visionError?.let { FailureNote(it) }
            }
        }
        formSection("Camera") {
            val cameras = listOf("*" to "Any camera") + (if (draft.trigger == "event") listOf("" to "No camera") else emptyList()) +
                sources.filter { it.text("kind") == "camera" && it.flag("healthy") }.map { it.text("id") to it.text("name").ifEmpty { it.text("id") } }
            ChoiceField("Camera", cameras, draft.camera, editable) { draft = draft.copy(camera = it) }
            if (loading) LoadingRow("Finding sources…")
            sourceError?.let { FailureNote(it) }
        }
        formSection("Other recording sources", "Audio and ROS 2 recording start at the trigger. Select a ROS 2 domain to record all its topics, or select individual topics. Application records are included automatically.") {
            listOf("audio" to "Audio inputs", "ros2" to "ROS 2 topics and domains").forEach { (kind, title) ->
                val open = kind in openSources
                ActionRow(title, if (open) Icons.Outlined.ExpandLess else Icons.Outlined.ExpandMore) { openSources = if (open) openSources - kind else openSources + kind }
                if (open) {
                    val choices = sources.filter { it.text("kind") == kind }
                    choices.forEach { source ->
                        val id = source.text("id")
                        val selected = if (kind == "audio") id in draft.audio else id in draft.ros2
                        ToggleRow(source.text("name").ifEmpty { id }, selected, enabled = editable && source.flag("healthy"), subtitle = source.text("unusable_reason")) { on ->
                            draft = if (kind == "audio") draft.copy(audio = if (on) draft.audio + id else draft.audio - id) else draft.copy(ros2 = if (on) draft.ros2 + id else draft.ros2 - id)
                        }
                    }
                    if (choices.isEmpty()) SecondaryText("No sources discovered")
                }
            }
            ToggleRow("Device telemetry", draft.telemetry, editable) { draft = draft.copy(telemetry = it) }
        }
        formSection("Action") {
            ChoiceField("Action", listOf("notify" to "Record & notify", "record" to "Record"), if (draft.notify) "notify" else "record", editable) { draft = draft.copy(notify = it == "notify") }
            ToggleRow("Upload to Wendy Cloud", draft.upload, editable) { draft = draft.copy(upload = it) }
        }
        formSection("Recording") {
            StepperRow("Before trigger: ${draft.before}s", draft.before, 0..300, 5, editable) { draft = draft.copy(before = it) }
            StepperRow("After trigger: ${draft.after}s", draft.after, 5..300, 5, editable) { draft = draft.copy(after = it) }
        }
        if (draft.trigger != "event") formSection("Detection") {
            LabeledRow("Confidence", "%.0f%%".format(draft.threshold * 100))
            Slider(draft.threshold.toFloat(), { draft = draft.copy(threshold = Math.round(it * 100) / 100.0) }, valueRange = 0.5f..0.99f, enabled = editable)
            StepperRow("Cooldown: ${draft.cooldown}s", draft.cooldown, 5..3600, 5, editable) { draft = draft.copy(cooldown = it) }
        }
        error?.let { formSection { FailureNote(it) } }
        item { SecondaryText("The campaign starts on $deviceName when you create it.") }
        item {
            Button(onClick = {
                saving = true; error = null
                scope.launch {
                    try { model.backend.call("DeployCampaign", session, draft.input()); model.record("Created campaign ${draft.name.trim()}"); deployed(); nav.pop() }
                    catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { saving = false }
                }
            }, Modifier.fillMaxWidth(), enabled = draft.valid && !saving) { Text(if (saving) "Creating…" else "Create") }
        }
    }
}

@Composable
fun StepperRow(title: String, value: Int, range: IntRange, step: Int, enabled: Boolean = true, onChange: (Int) -> Unit) {
    Row(Modifier.fillMaxWidth().padding(vertical = 4.dp), verticalAlignment = Alignment.CenterVertically) {
        Text(title, Modifier.weight(1f), style = MaterialTheme.typography.bodyLarge)
        IconButton(onClick = { onChange((value - step).coerceIn(range)) }, enabled = enabled && value > range.first) { Icon(Icons.Outlined.Remove, "Decrease") }
        IconButton(onClick = { onChange((value + step).coerceIn(range)) }, enabled = enabled && value < range.last) { Icon(Icons.Outlined.Add, "Increase") }
    }
}

@Composable
private fun ModelChoicePage(choices: List<Pair<String, String>>, selected: String, choose: (String) -> Unit) {
    val nav = LocalNavigator.current
    var repository by remember { mutableStateOf("") }
    PageColumn {
        formSection("Models") {
            choices.forEach { (title, repo) ->
                Row(Modifier.fillMaxWidth().clickable { choose(repo); nav.pop() }.padding(vertical = 8.dp), verticalAlignment = Alignment.CenterVertically) {
                    Column(Modifier.weight(1f)) { Text(title, style = MaterialTheme.typography.bodyLarge); SecondaryText(repo) }
                    if (repo == selected) Icon(Icons.Outlined.Check, "Selected", tint = MaterialTheme.colorScheme.primary)
                }
            }
        }
        formSection("Other model") {
            OutlinedTextField(repository, { repository = it }, Modifier.fillMaxWidth(), label = { Text("Hugging Face owner/repository") }, singleLine = true, keyboardOptions = KeyboardOptions(autoCorrectEnabled = false))
            ActionRow("Use model", enabled = '/' in repository) { choose(repository.trim()); nav.pop() }
        }
    }
}

@Composable
private fun LabelChoicePage(labels: List<String>, selected: () -> Set<String>, update: (Set<String>) -> Unit) {
    var search by remember { mutableStateOf("") }
    var current by remember { mutableStateOf(selected()) }
    Column(Modifier.fillMaxSize()) {
        OutlinedTextField(search, { search = it }, Modifier.fillMaxWidth().padding(16.dp), singleLine = true, placeholder = { Text("Find a category") }, leadingIcon = { Icon(Icons.Outlined.Search, null) })
        androidx.compose.foundation.lazy.LazyColumn(Modifier.weight(1f)) {
            items(labels.filter { search.isEmpty() || it.contains(search, true) }) { label ->
                Row(Modifier.fillMaxWidth().clickable {
                    current = if (label in current) current - label else if (current.size < 32) current + label else current
                    update(current)
                }.padding(horizontal = 20.dp, vertical = 12.dp), verticalAlignment = Alignment.CenterVertically) {
                    Text(label.replaceFirstChar { it.uppercase() }, Modifier.weight(1f))
                    if (label in current) Icon(Icons.Outlined.Check, "Selected", tint = MaterialTheme.colorScheme.primary)
                }
            }
        }
        Text("${current.size} of 32 selected", Modifier.fillMaxWidth().padding(12.dp), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

@Composable
private fun ModelDetailsPage(repo: String, info: JSONObject, categories: Int) {
    val context = LocalContext.current
    PageColumn {
        formSection { Text(shortModelName(repo), Modifier.padding(top = 8.dp), style = MaterialTheme.typography.titleMedium); SecondaryText(repo) }
        formSection {
            LabeledRow("Download size", formatBytes(info.number("WeightsBytes")))
            LabeledRow("License", info.text("License"))
            LabeledRow("Categories", categories.toString())
            ActionRow("Model card", Icons.AutoMirrored.Outlined.OpenInNew) { context.startActivity(Intent(Intent.ACTION_VIEW, Uri.parse("https://huggingface.co/$repo"))) }
        }
    }
}
