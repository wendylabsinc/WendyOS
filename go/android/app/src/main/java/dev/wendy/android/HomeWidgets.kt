package dev.wendy.android

import android.content.Context
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.combinedClickable
import androidx.compose.foundation.ExperimentalFoundationApi
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import org.json.JSONArray
import org.json.JSONObject
import java.security.MessageDigest
import java.util.UUID

/** Home widget kinds; identifiers match the iOS HomeWidgetKind raw values. */
enum class HomeWidgetKind(val raw: String, val title: String, val icon: ImageVector, val color: Color, val summary: String) {
    Favorites("devices.favorites", "Favorite devices", Icons.Outlined.StarOutline, LensStyle.green, "Devices you open most"),
    Camera("devices.camera", "Camera feed", Icons.Outlined.Videocam, Color(0xFF30B0C7), "Watch a camera from Home"),
    Campaign("devices.campaign", "Campaign recording", Icons.Outlined.FiberManualRecord, Color(0xFFFF3B30), "Record a campaign clip"),
    Gallery("data.gallery", "Data gallery", Icons.Outlined.GridView, Color(0xFF5B5A56), "Latest recordings from one or all devices"),
    Portal("devices.portal", "App web portal", Icons.Outlined.Public, LensStyle.green, "Open an app’s web page"),
    Attention("home.attention", "Needs you", Icons.Outlined.WarningAmber, Color(0xFFFF9500), "Device issues that need attention"),
    Activity("home.activity", "Happening now", Icons.Outlined.ShowChart, Color(0xFF5856D6), "Recent activity and current connections"),
    Actions("home.actions", "Quick actions", Icons.Outlined.Bolt, Color(0xFF1C1C1E), "Shortcuts to setup, devices, activity and data");
    val builtIn get() = this == Attention || this == Activity || this == Actions
    val sizes get() = if (this == Activity || this == Actions) listOf(HomeWidgetSize.Small, HomeWidgetSize.Medium) else HomeWidgetSize.entries
    companion object { fun of(raw: String) = entries.firstOrNull { it.raw == raw } }
}

/** Small is half width; Medium and Large are full width, Large twice as tall (iOS HomeWidgetSize). */
enum class HomeWidgetSize(val title: String) { Small("Small"), Medium("Medium"), Large("Large");
    companion object { fun of(raw: String) = when (raw) { "Compact", "Small", "S" -> Small; "Large", "L" -> Large; else -> Medium } }
}

data class HomeWidgetDevice(val id: String, val title: String)

data class HomeWidget(
    val id: String = UUID.randomUUID().toString().uppercase(), val kind: HomeWidgetKind, val size: HomeWidgetSize = HomeWidgetSize.Medium, val title: String = "",
    val devices: List<HomeWidgetDevice> = emptyList(), val resource: String = "", val resourceTitle: String = "", val dataKind: String? = null, val label: String = "",
) {
    val displayTitle: String get() {
        if (title.isNotBlank()) return title
        return when (kind) {
            HomeWidgetKind.Camera -> resourceTitle.trim().ifEmpty { null }?.let { if (it.lowercase().endsWith("camera")) it else "$it camera" } ?: kind.title
            HomeWidgetKind.Campaign -> if (resourceTitle.isEmpty()) kind.title else "Record · $resourceTitle"
            HomeWidgetKind.Gallery -> devices.firstOrNull()?.let { "Data · ${it.title}" } ?: "Data gallery"
            HomeWidgetKind.Portal -> resourceTitle.ifEmpty { kind.title }
            else -> kind.title
        }
    }
    /** The same rules as iOS: devices are unique, at most six, and single-device widgets name their resource. */
    val valid: Boolean get() {
        if (kind.builtIn) return size in kind.sizes
        if (kind == HomeWidgetKind.Gallery && devices.isEmpty()) return true
        if (devices.isEmpty() || devices.size > 6 || devices.map { it.id }.toSet().size != devices.size) return false
        if (kind != HomeWidgetKind.Favorites && devices.size != 1) return false
        return kind == HomeWidgetKind.Favorites || kind == HomeWidgetKind.Gallery || resource.isNotEmpty()
    }
    fun json(): JSONObject = JSONObject().put("id", id).put("kind", kind.raw).put("size", size.title).put("title", title)
        .put("devices", JSONArray(devices.map { JSONObject().put("id", it.id).put("title", it.title) })).put("resource", resource).put("resourceTitle", resourceTitle)
        .put("label", label).also { if (dataKind != null) it.put("dataKind", dataKind) }
    companion object {
        fun parse(value: JSONObject): HomeWidget? {
            val kind = HomeWidgetKind.of(value.optString("kind")) ?: return null
            return HomeWidget(value.optString("id").ifEmpty { UUID.randomUUID().toString() }, kind, HomeWidgetSize.of(value.optString("size")), value.optString("title"),
                value.optJSONArray("devices")?.let { a -> (0 until a.length()).mapNotNull { a.optJSONObject(it)?.let { d -> HomeWidgetDevice(d.optString("id"), d.optString("title")) } } } ?: emptyList(),
                value.optString("resource"), value.optString("resourceTitle"), value.optString("dataKind").ifEmpty { null }, value.optString("label"))
        }
    }
}

/** Stable widget identity for a device across Nearby and Cloud (iOS recordingDeviceID). */
fun widgetDeviceID(device: Device) = device.id.ifEmpty { device.selector }

/** Saved Home layouts, one per account and organization (iOS HomeWidgetStore). */
class HomeWidgetStore(context: Context) {
    private val preferences = context.getSharedPreferences("home", 0)
    var items by mutableStateOf(emptyList<HomeWidget>()); private set
    var error by mutableStateOf<String?>(null); private set
    private var key: String? = null
    fun load(account: String, organization: String) {
        val next = storageKey(account, organization)
        if (key == next) return
        key = next; items = emptyList(); error = null
        val saved = preferences.getString(next, null) ?: return
        try { val array = JSONArray(saved); items = (0 until array.length()).mapNotNull { HomeWidget.parse(array.getJSONObject(it)) } }
        catch (_: Exception) { error = "The saved Home layout could not be read. Your saved layout has not been replaced." }
    }
    fun save(widget: HomeWidget) {
        if (!widget.valid || error != null) return
        items = if (items.any { it.id == widget.id }) items.map { if (it.id == widget.id) widget else it } else items + widget
        persist()
    }
    fun remove(id: String) { items = items.filter { it.id != id }; persist() }
    fun move(id: String, offset: Int) {
        val from = items.indexOfFirst { it.id == id }
        val to = from + offset
        if (from < 0 || to !in items.indices) return
        items = items.toMutableList().apply { add(to, removeAt(from)) }
        persist()
    }
    fun resize(id: String, size: HomeWidgetSize) { items = items.map { if (it.id == id && size in it.kind.sizes) it.copy(size = size) else it }; persist() }
    private fun persist() { val k = key ?: return; if (error == null) preferences.edit().putString(k, JSONArray(items.map { it.json() }).toString()).apply() }
    companion object {
        fun storageKey(account: String, organization: String): String {
            val digest = MessageDigest.getInstance("SHA-256").digest(JSONArray(listOf(account, organization)).toString().toByteArray())
            return "wendy.home.widgets.v1." + digest.joinToString("") { "%02x".format(it) }
        }
    }
}

private fun WendyModel.widgetDevices(): List<Device> = devices.distinctBy { widgetDeviceID(it) }.sortedBy { it.title.lowercase() }
private fun WendyModel.resolve(reference: HomeWidgetDevice?): Device? = reference?.let { r -> widgetDevices().firstOrNull { widgetDeviceID(it) == r.id } }
private fun WendyModel.status(device: Device): String = when {
    session != 0L && selected?.selector == device.selector -> "Connected"
    !device.selector.startsWith("cloud://") -> "Nearby"
    cloudError != null || presenceError.isNotEmpty() || System.currentTimeMillis() - presenceAt > 60_000 -> "Status unknown"
    else -> presence?.firstOrNull { it.selector == device.selector }?.let { if (it.online) "Online" else "Offline" } ?: "Status unknown"
}

/** Home's configurable widgets (iOS HomeWidgetDeck). Small widgets pair up in a row. */
@OptIn(ExperimentalFoundationApi::class)
@Composable
fun HomeWidgetDeck(model: WendyModel, store: HomeWidgetStore, editing: Boolean, setEditing: (Boolean) -> Unit, setup: () -> Unit, recordings: () -> Unit,
                   notifications: () -> Unit, devices: () -> Unit, openDevice: (Device) -> Unit) {
    val nav = LocalNavigator.current
    fun configure(widget: HomeWidget) = nav.push(widget.kind.title) { HomeWidgetEditor(model, widget) { store.save(it); nav.pop() } }
    Column(verticalArrangement = Arrangement.spacedBy(12.dp)) {
        store.error?.let { SecondaryText(it) }
        if (store.items.isEmpty()) Surface(onClick = { setEditing(true); nav.push("Add widget") { HomeWidgetManager(model, store) } },
            Modifier.fillMaxWidth().dashedBorder(MaterialTheme.colorScheme.outline.copy(alpha = .4f), 22.dp), shape = RoundedCornerShape(22.dp), color = Color.Transparent) {
            Column(Modifier.padding(24.dp), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(10.dp)) {
                Box(Modifier.size(52.dp).background(LensStyle.green.copy(alpha = .12f), RoundedCornerShape(16.dp)), contentAlignment = Alignment.Center) { Icon(Icons.Outlined.GridView, null, tint = LensStyle.accent) }
                Text("Add widgets", style = MaterialTheme.typography.titleMedium, color = LensStyle.accent)
                Text("Add widgets for the devices, cameras and data you check most.", style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant, textAlign = androidx.compose.ui.text.style.TextAlign.Center)
            }
        }
        // Dense packing: a small widget fills the half row next to a preceding small widget.
        val rows = mutableListOf<List<HomeWidget>>()
        var pending: HomeWidget? = null
        store.items.forEach { widget ->
            if (widget.size == HomeWidgetSize.Small) { if (pending != null) { rows.add(listOf(pending!!, widget)); pending = null } else pending = widget }
            else rows.add(listOf(widget))
        }
        pending?.let { rows.add(listOf(it)) }
        rows.forEach { row ->
            Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                row.forEach { widget ->
                    var menu by remember(widget.id) { mutableStateOf(false) }
                    Box(Modifier.weight(1f).height(if (widget.size == HomeWidgetSize.Large) 332.dp else 160.dp)) {
                        Surface(Modifier.fillMaxSize().let { if (editing) it.dashedBorder(MaterialTheme.colorScheme.outline.copy(alpha = .5f), 22.dp) else it }
                            .combinedClickable(enabled = editing || true, onClick = { if (editing) configure(widget) }, onLongClick = { menu = true }),
                            shape = RoundedCornerShape(22.dp), color = MaterialTheme.colorScheme.surface) {
                            Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                                Row(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
                                    Icon(widget.kind.icon, null, Modifier.size(16.dp), tint = LensStyle.visible(widget.kind.color))
                                    Text(widget.displayTitle, Modifier.weight(1f), style = MaterialTheme.typography.labelMedium, fontWeight = FontWeight.SemiBold, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis)
                                    if (editing) {
                                        IconButton(onClick = { store.move(widget.id, -1) }, Modifier.size(24.dp)) { Icon(Icons.Outlined.KeyboardArrowUp, "Move up", Modifier.size(18.dp)) }
                                        IconButton(onClick = { store.move(widget.id, 1) }, Modifier.size(24.dp)) { Icon(Icons.Outlined.KeyboardArrowDown, "Move down", Modifier.size(18.dp)) }
                                    }
                                }
                                Box(Modifier.weight(1f).fillMaxWidth()) {
                                    if (!editing) WidgetContent(model, widget, setup, recordings, notifications, devices, openDevice)
                                    else Row(Modifier.align(Alignment.BottomEnd), horizontalArrangement = Arrangement.spacedBy(4.dp)) {
                                        widget.kind.sizes.forEach { size ->
                                            FilterChip(widget.size == size, { store.resize(widget.id, size) }, label = { Text(size.title.take(1)) })
                                        }
                                    }
                                }
                            }
                        }
                        DropdownMenu(menu, onDismissRequest = { menu = false }) {
                            DropdownMenuItem(text = { Text("Edit") }, leadingIcon = { Icon(Icons.Outlined.Edit, null) }, onClick = { menu = false; setEditing(true) })
                            DropdownMenuItem(text = { Text("Configure") }, leadingIcon = { Icon(Icons.Outlined.Tune, null) }, onClick = { menu = false; configure(widget) })
                            DropdownMenuItem(text = { Text("Delete widget", color = MaterialTheme.colorScheme.error) }, leadingIcon = { Icon(Icons.Outlined.Delete, null, tint = MaterialTheme.colorScheme.error) }, onClick = { menu = false; store.remove(widget.id) })
                        }
                    }
                }
                if (row.size == 1 && row[0].size == HomeWidgetSize.Small) Spacer(Modifier.weight(1f))
            }
        }
        if (editing && store.items.isNotEmpty()) SecondaryText("Use the arrows to reorder · S, M and L resize · tap to configure")
    }
}

@Composable
private fun WidgetContent(model: WendyModel, widget: HomeWidget, setup: () -> Unit, recordings: () -> Unit, notifications: () -> Unit, devices: () -> Unit, openDevice: (Device) -> Unit) {
    val nav = LocalNavigator.current
    when (widget.kind) {
        HomeWidgetKind.Favorites -> Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
            widget.devices.take(if (widget.size == HomeWidgetSize.Large) 6 else if (widget.size == HomeWidgetSize.Small) 2 else 3).forEach { reference ->
                val device = model.resolve(reference)
                if (device == null) Text("${reference.title} · Unavailable", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 2)
                else Row(Modifier.fillMaxWidth().clickable { openDevice(device) }.padding(vertical = 2.dp), horizontalArrangement = Arrangement.spacedBy(10.dp), verticalAlignment = Alignment.CenterVertically) {
                    val status = model.status(device)
                    Box {
                        DeviceAvatar(model, device, 32.dp, if (status == "Offline") LensStyle.offline else LensStyle.green)
                        Box(Modifier.align(Alignment.BottomEnd).size(9.dp).background(if (status in setOf("Online", "Connected", "Nearby")) LensStyle.online else LensStyle.offline, CircleShape))
                    }
                    Column(Modifier.weight(1f)) {
                        Text(device.title, style = MaterialTheme.typography.titleSmall, maxLines = 1, overflow = TextOverflow.Ellipsis)
                        if (widget.size != HomeWidgetSize.Small) Text("$status · ${if (device.selector.startsWith("cloud://")) "Cloud" else "Local"}", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                }
            }
        }
        HomeWidgetKind.Attention -> {
            val issues = listOfNotNull(model.nearbyError, model.cloudError, model.detailError)
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                if (issues.isEmpty()) { Icon(Icons.Outlined.CheckCircle, null, Modifier.size(32.dp), tint = LensStyle.green); Text("No issues reported", style = MaterialTheme.typography.bodyMedium) }
                else {
                    if (widget.size == HomeWidgetSize.Small) Text("${issues.size}", fontSize = 34.sp, fontWeight = FontWeight.Bold, color = LensStyle.warning)
                    issues.take(if (widget.size == HomeWidgetSize.Small) 1 else if (widget.size == HomeWidgetSize.Large) 3 else 2).forEach { issue ->
                        Text(PresentedFailure(issue).message, Modifier.clickable(onClick = devices), style = MaterialTheme.typography.bodySmall, maxLines = 2)
                    }
                }
            }
        }
        HomeWidgetKind.Activity -> Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
            model.experienceEvents.take(if (widget.size == HomeWidgetSize.Small) 1 else 2).forEach { event ->
                Column(Modifier.clickable(onClick = notifications)) {
                    Text(event.title, style = MaterialTheme.typography.titleSmall, maxLines = 2); Text(event.deviceName, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1)
                }
            }
            if (model.experienceEvents.isEmpty()) Text("No recent activity", style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        HomeWidgetKind.Actions -> {
            val actions = listOf(Triple("Set up", Icons.Outlined.Add, setup), Triple("Devices", Icons.Outlined.Memory, devices), Triple("Activity", Icons.Outlined.Schedule, notifications), Triple("Data", Icons.Outlined.Storage, recordings))
            Column(verticalArrangement = Arrangement.spacedBy(6.dp)) {
                actions.chunked(if (widget.size == HomeWidgetSize.Small) 2 else 4).forEach { row ->
                    Row(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                        row.forEach { (title, icon, action) ->
                            // Two rows must fit the small widget's 160dp height with their labels.
                            val small = widget.size == HomeWidgetSize.Small
                            Column(Modifier.weight(1f).background(LensStyle.green.copy(alpha = .08f), RoundedCornerShape(12.dp)).clickable(onClick = action).padding(vertical = if (small) 5.dp else 8.dp),
                                horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(if (small) 2.dp else 4.dp)) {
                                Icon(icon, null, Modifier.size(if (small) 18.dp else 20.dp), tint = LensStyle.accent); Text(title, fontSize = 10.sp, lineHeight = 12.sp, color = LensStyle.accent, maxLines = 1)
                            }
                        }
                    }
                }
            }
        }
        HomeWidgetKind.Gallery -> Column(verticalArrangement = Arrangement.spacedBy(6.dp)) {
            Text(widget.devices.firstOrNull()?.title ?: "All devices", style = MaterialTheme.typography.titleSmall)
            SecondaryText(listOfNotNull(widget.label.ifEmpty { null }, widget.dataKind).joinToString(" · ").ifEmpty { "Latest recordings" })
            Spacer(Modifier.weight(1f))
            WidgetButton("Open gallery") { recordings() }
        }
        else -> {
            val device = model.resolve(widget.devices.firstOrNull())
            val offline = device == null || model.status(device) == "Offline"
            val connected = device != null && model.session != 0L && model.selected?.selector == device.selector
            Column(verticalArrangement = Arrangement.spacedBy(6.dp)) {
                if (widget.kind == HomeWidgetKind.Camera && connected) {
                    val camera = model.cameras.firstOrNull { it.text("stable_id") == widget.resource }
                    if (camera != null && widget.size != HomeWidgetSize.Small) Box(Modifier.weight(1f)) { CameraTile(model, camera, compact = true) }
                    else { Text(widget.resourceTitle, style = MaterialTheme.typography.titleSmall, maxLines = 2); Spacer(Modifier.weight(1f)) }
                } else {
                    Text(widget.resourceTitle.ifEmpty { widget.kind.title }, style = MaterialTheme.typography.titleSmall, maxLines = 2)
                    Text(device?.title ?: widget.devices.firstOrNull()?.title ?: "Device unavailable", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1)
                    Spacer(Modifier.weight(1f))
                }
                val label = when {
                    offline -> "Offline"
                    widget.kind == HomeWidgetKind.Camera -> if (connected) "Open cameras" else "Watch camera"
                    widget.kind == HomeWidgetKind.Campaign -> if (widget.size == HomeWidgetSize.Small) "Record" else "Record a clip"
                    else -> "Open"
                }
                WidgetButton(label, enabled = !offline, danger = widget.kind == HomeWidgetKind.Campaign) {
                    val target = device ?: return@WidgetButton
                    when (widget.kind) {
                        HomeWidgetKind.Portal -> { model.connect(target); model.openAppWhenReady(widget.resource) }
                        HomeWidgetKind.Campaign -> { openDevice(target); nav.push("Campaigns") { CampaignsScreen(model) } }
                        else -> if (connected) { openDevice(target) } else model.connect(target)
                    }
                }
            }
        }
    }
}

@Composable
private fun WidgetButton(title: String, enabled: Boolean = true, danger: Boolean = false, onClick: () -> Unit) {
    Surface(onClick = onClick, enabled = enabled, modifier = Modifier.fillMaxWidth().heightIn(min = 32.dp), shape = CircleShape,
        color = if (!enabled) MaterialTheme.colorScheme.surfaceVariant else if (danger) LensStyle.danger.copy(alpha = .12f) else LensStyle.green) {
        Box(contentAlignment = Alignment.Center) { Text(title, Modifier.padding(vertical = 7.dp), style = MaterialTheme.typography.labelMedium, fontWeight = FontWeight.SemiBold,
            color = if (!enabled) MaterialTheme.colorScheme.onSurfaceVariant else if (danger) LensStyle.danger else Color.White) }
    }
}

/** Choose a widget to add (iOS HomeWidgetManager). */
@Composable
fun HomeWidgetManager(model: WendyModel, store: HomeWidgetStore) {
    val nav = LocalNavigator.current
    PageColumn {
        item { SecondaryText("Built-in widgets show your account’s activity. Device widgets open the devices, cameras and data you choose.") }
        item {
            LensSection("Widgets") {
                HomeWidgetKind.entries.forEachIndexed { index, kind ->
                    if (index > 0) LensDivider()
                    val present = kind.builtIn && store.items.any { it.kind == kind }
                    LensRow(kind.title, if (present) "Already on Home" else kind.summary, kind.icon, kind.color, enabled = !present) {
                        val draft = HomeWidget(kind = kind, size = if (kind.builtIn) HomeWidgetSize.Small else HomeWidgetSize.Medium)
                        if (kind.builtIn) { store.save(draft); nav.pop() }
                        else nav.push(kind.title) { HomeWidgetEditor(model, draft) { store.save(it); nav.pop(); nav.pop() } }
                    }
                }
            }
        }
    }
}

/** Configure a widget's title, size, devices and resource (iOS HomeWidgetEditor). */
@Composable
fun HomeWidgetEditor(model: WendyModel, initial: HomeWidget, save: (HomeWidget) -> Unit) {
    var widget by remember { mutableStateOf(initial) }
    val all = model.widgetDevices()
    val single = widget.kind != HomeWidgetKind.Favorites && widget.kind != HomeWidgetKind.Gallery
    val chosen = model.resolve(widget.devices.firstOrNull())
    val connected = chosen != null && model.session != 0L && model.selected?.selector == chosen.selector
    var campaigns by remember { mutableStateOf(emptyList<String>()) }
    LaunchedEffect(connected, widget.kind) {
        if (connected && widget.kind == HomeWidgetKind.Campaign) campaigns = runCatching { model.backend.call("Campaigns", model.session).items().map { it.text("Name") } }.getOrDefault(emptyList())
    }
    PageColumn {
        formSection {
            OutlinedTextField(widget.title, { widget = widget.copy(title = it) }, Modifier.fillMaxWidth(), label = { Text("Title") }, placeholder = { Text(widget.displayTitle) }, singleLine = true)
            ChoiceField("Size", widget.kind.sizes.map { it.name to it.title }, widget.size.name) { widget = widget.copy(size = HomeWidgetSize.valueOf(it)) }
        }
        if (!widget.kind.builtIn) formSection(if (single) "Device" else "Devices", if (widget.kind == HomeWidgetKind.Favorites) "Choose up to six devices." else if (widget.kind == HomeWidgetKind.Gallery) "Leave empty to show every device." else null) {
            if (all.isEmpty()) SecondaryText("No devices found yet. Devices appear once Nearby or Cloud discovery has run.")
            all.forEach { device ->
                val reference = HomeWidgetDevice(widgetDeviceID(device), device.title)
                val selected = widget.devices.any { it.id == reference.id }
                Row(Modifier.fillMaxWidth().clickable {
                    widget = if (single) widget.copy(devices = listOf(reference), resource = "", resourceTitle = "")
                    else widget.copy(devices = if (selected) widget.devices.filter { it.id != reference.id } else (widget.devices + reference).take(6))
                }.padding(vertical = 8.dp), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                    DeviceAvatar(model, device, 32.dp)
                    Text(device.title, Modifier.weight(1f))
                    if (selected) Icon(Icons.Outlined.Check, "Selected", tint = LensStyle.accent)
                }
            }
        }
        if (widget.kind == HomeWidgetKind.Camera || widget.kind == HomeWidgetKind.Portal || widget.kind == HomeWidgetKind.Campaign) formSection(when (widget.kind) { HomeWidgetKind.Camera -> "Camera"; HomeWidgetKind.Portal -> "App"; else -> "Campaign" }) {
            when {
                chosen == null -> SecondaryText("Choose a device first.")
                !connected -> { SecondaryText("Connect to ${chosen.title} to list its ${if (widget.kind == HomeWidgetKind.Camera) "cameras" else if (widget.kind == HomeWidgetKind.Portal) "apps" else "campaigns"}."); ActionRow("Connect") { model.connect(chosen) } }
                else -> {
                    val choices = when (widget.kind) {
                        HomeWidgetKind.Camera -> model.cameras.map { it.text("stable_id").ifEmpty { it.text("id") } to it.text("name") }
                        HomeWidgetKind.Portal -> model.apps.filter { it.hasWebpage }.map { it.name to it.name }
                        else -> campaigns.map { it to it }
                    }
                    if (choices.isEmpty()) SecondaryText("Nothing to choose on ${chosen.title}.")
                    choices.forEach { (id, title) ->
                        Row(Modifier.fillMaxWidth().clickable { widget = widget.copy(resource = id, resourceTitle = title) }.padding(vertical = 8.dp), verticalAlignment = Alignment.CenterVertically) {
                            Text(title, Modifier.weight(1f)); if (widget.resource == id) Icon(Icons.Outlined.Check, "Selected", tint = LensStyle.accent)
                        }
                    }
                }
            }
        }
        if (widget.kind == HomeWidgetKind.Gallery) formSection("Filter") {
            OutlinedTextField(widget.label, { widget = widget.copy(label = it.trim()) }, Modifier.fillMaxWidth(), label = { Text("Trigger label") }, placeholder = { Text("Any") }, singleLine = true)
        }
        item { LensButton("Save", primary = true, enabled = widget.valid, modifier = Modifier.fillMaxWidth()) { save(widget) } }
    }
}
