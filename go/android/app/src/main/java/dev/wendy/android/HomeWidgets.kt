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
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.launch
import org.json.JSONArray
import org.json.JSONObject
import java.security.MessageDigest
import java.text.DateFormat
import java.util.Date
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

/** Stable widget identity for a device across Nearby and Cloud (iOS HomeWidgetDevice uses recordingDeviceID). */
fun widgetDeviceID(device: Device) = recordingDeviceID(device).ifEmpty { device.selector }

/** Where one widget sits on the four-column grid; Small is 2×2, Medium 4×2, Large 4×4 (iOS HomeWidgetGridPlacement). */
data class HomeWidgetPlacement(val column: Int, val row: Int, val columns: Int, val rows: Int)

/** Deterministic dense packing: a later small widget fills a preceding half row. */
fun packWidgets(sizes: List<HomeWidgetSize>): List<HomeWidgetPlacement> {
    val occupied = mutableSetOf<Int>()
    return sizes.map { size ->
        val width = if (size == HomeWidgetSize.Small) 2 else 4
        val height = if (size == HomeWidgetSize.Large) 4 else 2
        generateSequence(0) { it + 1 }.firstNotNullOf { row ->
            (0..(4 - width)).firstNotNullOfOrNull { column ->
                val cells = (row until row + height).flatMap { r -> (column until column + width).map { r * 4 + it } }
                if (cells.none { it in occupied }) { occupied += cells; HomeWidgetPlacement(column, row, width, height) } else null
            }
        }
    }
}

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

private fun WendyModel.widgetDevices(): List<Device> = if (!initialized) emptyList() else (nearbyDevices + cloudDevices).distinctBy { widgetDeviceID(it) }.sortedBy { it.title.lowercase() }
private fun WendyModel.resolve(reference: HomeWidgetDevice?): Device? = reference?.let { r -> widgetDevices().firstOrNull { widgetDeviceID(it) == r.id } }
private fun WendyModel.status(device: Device): String = when {
    session != 0L && selected?.selector == device.selector -> "Connected"
    !device.isCloud -> "Nearby"
    cloudError != null || presenceError.isEmpty().not() || System.currentTimeMillis() - presenceAt > 60_000 -> "Status unknown"
    else -> presence?.firstOrNull { it.selector == device.selector }?.let { if (it.online) "Online" else "Offline" } ?: "Status unknown"
}
private fun HomeWidgetKind.tint(dark: Boolean): Color = if (this == HomeWidgetKind.Gallery) headerColor(dark) else color
private fun headerColor(dark: Boolean) = if (dark) Color(0xFFAAAAAA) else Color(0xFF5B5A56)

/** Home's configurable widgets (iOS HomeWidgetDeck): a dense four-column grid of Small, Medium and Large cards. */
@OptIn(ExperimentalFoundationApi::class)
@Composable
fun HomeWidgetDeck(model: WendyModel, store: HomeWidgetStore, editing: Boolean, setEditing: (Boolean) -> Unit, setup: () -> Unit, recordings: () -> Unit,
                   notifications: () -> Unit, devices: () -> Unit, openDevice: (Device) -> Unit) {
    val nav = LocalNavigator.current
    val catalog = LocalRecordingCatalog.current
    var activeCamera by remember { mutableStateOf<String?>(null) }
    var session by remember { mutableStateOf<HomeWidget?>(null) }
    var expanded by remember { mutableStateOf<HomeWidget?>(null) }
    var refresh by remember { mutableIntStateOf(0) }
    fun configure(widget: HomeWidget) = nav.push(widget.kind.title) { HomeWidgetEditor(model, widget) { store.save(it); nav.pop() } }
    LaunchedEffect(editing) { if (editing) activeCamera = null }
    // Only gallery widgets read recordings, and only for the devices they show.
    val sources = RecordingSources.of(model)
    val wanted = store.items.filter { it.kind == HomeWidgetKind.Gallery }.flatMap { w -> w.devices.map { it.id } }.toSet()
    val all = store.items.any { it.kind == HomeWidgetKind.Gallery && it.devices.isEmpty() }
    val targets = sources.targets.filter { all || it.id in wanted }
    val active = LocalPageVisible.current && model.inForeground
    if (catalog != null) LaunchedEffect(targets, sources.inventory, active, refresh) { if (active) catalog.load(targets, refresh, null, consumer = "home.widgets", retaining = sources.inventory) }
    CompositionLocalProvider(LocalRecordingPosters provides if (active) LocalRecordingPosters.current else null) {
        Column(verticalArrangement = Arrangement.spacedBy(12.dp)) {
            store.error?.let { SecondaryText(it) }
            if (store.items.isEmpty()) Surface(onClick = { setEditing(true); nav.push("Add widget") { HomeWidgetManager(model, store) } },
                Modifier.fillMaxWidth().dashedBorder(MaterialTheme.colorScheme.outline.copy(alpha = .4f), 22.dp), shape = RoundedCornerShape(22.dp), color = Color.Transparent) {
                Column(Modifier.padding(24.dp), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(10.dp)) {
                    Box(Modifier.size(52.dp).background(LensStyle.green.copy(alpha = .12f), RoundedCornerShape(16.dp)), contentAlignment = Alignment.Center) { Icon(Icons.Outlined.GridView, null, tint = LensStyle.accent) }
                    Text("Add widgets", style = MaterialTheme.typography.titleMedium, color = LensStyle.accent)
                    Text("Add widgets for the devices, cameras and data you check most.", style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant, textAlign = TextAlign.Center)
                }
            }
            val placements = packWidgets(store.items.map { it.size })
            val bands = store.items.zip(placements).groupBy { it.second.row }.toSortedMap()
            bands.values.forEach { band ->
                Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                    band.sortedBy { it.second.column }.forEach { (widget, placement) ->
                        Box(Modifier.weight(placement.columns.toFloat()).height(if (placement.rows == 4) 332.dp else 160.dp)) {
                            WidgetCard(model, store, widget, editing, setEditing, ::configure, activeCamera, { activeCamera = it }, { session = it }, { activeCamera = null; expanded = it },
                                setup, recordings, notifications, devices, openDevice, { refresh++ })
                        }
                    }
                    if (band.size == 1 && band[0].second.columns == 2) Spacer(Modifier.weight(2f))
                }
            }
            if (editing && store.items.isNotEmpty()) SecondaryText("Use the arrows to reorder · S, M and L resize · tap to configure")
        }
    }
    session?.let { widget -> HomeWidgetSession(model, widget) { session = null } }
    expanded?.let { widget -> HomeWidgetFullScreen(model, widget) { expanded = null } }
}

@OptIn(ExperimentalFoundationApi::class)
@Composable
private fun WidgetCard(model: WendyModel, store: HomeWidgetStore, widget: HomeWidget, editing: Boolean, setEditing: (Boolean) -> Unit, configure: (HomeWidget) -> Unit,
                       activeCamera: String?, setActiveCamera: (String?) -> Unit, openSession: (HomeWidget) -> Unit, expand: (HomeWidget) -> Unit,
                       setup: () -> Unit, recordings: () -> Unit, notifications: () -> Unit, devices: () -> Unit, openDevice: (Device) -> Unit, retry: () -> Unit) {
    var menu by remember(widget.id) { mutableStateOf(false) }
    Surface(Modifier.fillMaxSize().let { if (editing) it.dashedBorder(MaterialTheme.colorScheme.outline.copy(alpha = .5f), 22.dp) else it }
        .combinedClickable(onClick = { if (editing) configure(widget) }, onLongClick = { menu = true }, onClickLabel = if (editing) "Configure" else null),
        shape = RoundedCornerShape(22.dp), color = MaterialTheme.colorScheme.surface) {
        Box {
            Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Row(Modifier.heightIn(min = 24.dp), horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
                    Icon(widget.kind.icon, null, Modifier.size(16.dp), tint = LensStyle.visible(widget.kind.tint(LensStyle.isDark)))
                    Text(widget.displayTitle, Modifier.weight(1f), fontSize = 12.sp, fontWeight = FontWeight.SemiBold, color = headerColor(LensStyle.isDark), maxLines = 1, overflow = TextOverflow.Ellipsis)
                    if (editing) {
                        IconButton(onClick = { store.move(widget.id, -1) }, Modifier.size(24.dp)) { Icon(Icons.Outlined.KeyboardArrowUp, "Move earlier", Modifier.size(18.dp)) }
                        IconButton(onClick = { store.move(widget.id, 1) }, Modifier.size(24.dp)) { Icon(Icons.Outlined.KeyboardArrowDown, "Move later", Modifier.size(18.dp)) }
                    }
                }
                Box(Modifier.weight(1f).fillMaxWidth()) {
                    WidgetContent(model, widget, activeCamera, setActiveCamera, openSession, expand, setup, recordings, notifications, devices, openDevice, retry)
                    // Content stays visible while editing but does not take taps (iOS allowsHitTesting(!editing)).
                    if (editing) Box(Modifier.matchParentSize().combinedClickable(onClick = { configure(widget) }, onLongClick = { menu = true }))
                }
            }
            if (editing) Row(Modifier.align(Alignment.BottomEnd).padding(4.dp).background(MaterialTheme.colorScheme.surfaceVariant, RoundedCornerShape(14.dp)).padding(horizontal = 4.dp),
                horizontalArrangement = Arrangement.spacedBy(2.dp)) {
                widget.kind.sizes.forEach { size ->
                    TextButton(onClick = { store.resize(widget.id, size) }, Modifier.size(36.dp), contentPadding = PaddingValues(0.dp)) {
                        Text(size.title.take(1), fontWeight = if (widget.size == size) FontWeight.Bold else FontWeight.Normal, color = if (widget.size == size) LensStyle.accent else MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                }
            }
            DropdownMenu(menu, onDismissRequest = { menu = false }) {
                DropdownMenuItem(text = { Text("Edit") }, leadingIcon = { Icon(Icons.Outlined.Edit, null) }, onClick = { menu = false; setActiveCamera(null); setEditing(true) })
                DropdownMenuItem(text = { Text("Delete widget", color = MaterialTheme.colorScheme.error) }, leadingIcon = { Icon(Icons.Outlined.Delete, null, tint = MaterialTheme.colorScheme.error) },
                    onClick = { menu = false; if (activeCamera == widget.id) setActiveCamera(null); store.remove(widget.id) })
            }
        }
    }
}

@Composable
private fun StatusAvatar(model: WendyModel, device: Device, size: androidx.compose.ui.unit.Dp, dot: androidx.compose.ui.unit.Dp) {
    val status = model.status(device)
    Box {
        DeviceAvatar(model, device, size, if (status == "Offline") LensStyle.offline else LensStyle.green)
        Box(Modifier.align(Alignment.BottomEnd).size(dot).background(MaterialTheme.colorScheme.surface, CircleShape).padding(1.5.dp)
            .background(if (status in setOf("Online", "Connected", "Nearby")) LensStyle.online else LensStyle.offline, CircleShape))
    }
}

@Composable
private fun WidgetContent(model: WendyModel, widget: HomeWidget, activeCamera: String?, setActiveCamera: (String?) -> Unit, openSession: (HomeWidget) -> Unit, expand: (HomeWidget) -> Unit,
                          setup: () -> Unit, recordings: () -> Unit, notifications: () -> Unit, devices: () -> Unit, openDevice: (Device) -> Unit, retry: () -> Unit) {
    when (widget.kind) {
        HomeWidgetKind.Favorites -> if (widget.size == HomeWidgetSize.Small) {
            Column(verticalArrangement = Arrangement.spacedBy(5.dp)) {
                widget.devices.take(4).chunked(2).forEach { row ->
                    Row(horizontalArrangement = Arrangement.spacedBy(5.dp)) {
                        row.forEach { reference ->
                            val device = model.resolve(reference)
                            Column(Modifier.weight(1f).heightIn(min = 44.dp).clickable(enabled = device != null) { device?.let(openDevice) }, horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(2.dp)) {
                                if (device != null) StatusAvatar(model, device, 32.dp, 7.dp) else Icon(Icons.Outlined.WifiOff, "Unavailable", Modifier.size(32.dp))
                                Text(reference.title, fontSize = 10.sp, maxLines = 1, overflow = TextOverflow.Ellipsis)
                            }
                        }
                        if (row.size == 1) Spacer(Modifier.weight(1f))
                    }
                }
            }
        } else Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
            widget.devices.take(if (widget.size == HomeWidgetSize.Large) 6 else 3).forEach { reference ->
                val device = model.resolve(reference)
                if (device == null) Text("${reference.title} · Unavailable", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 2, overflow = TextOverflow.Ellipsis)
                else Row(Modifier.fillMaxWidth().clickable { openDevice(device) }.padding(vertical = 2.dp), horizontalArrangement = Arrangement.spacedBy(10.dp), verticalAlignment = Alignment.CenterVertically) {
                    StatusAvatar(model, device, 32.dp, 9.dp)
                    Column(Modifier.weight(1f)) {
                        Text(device.title, style = MaterialTheme.typography.titleSmall, maxLines = 1, overflow = TextOverflow.Ellipsis)
                        Text("${model.status(device)} · ${if (device.isCloud) "Cloud" else "Local"}", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 2)
                    }
                    Icon(Icons.Outlined.ChevronRight, null, Modifier.size(14.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
                }
            }
        }
        HomeWidgetKind.Gallery -> GalleryWidget(model, widget, recordings, retry)
        HomeWidgetKind.Attention -> {
            val issues = listOfNotNull(model.nearbyError, model.cloudError, model.detailError)
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                if (issues.isEmpty()) { Icon(Icons.Outlined.CheckCircle, null, Modifier.size(32.dp), tint = LensStyle.green); Text("No issues reported", style = MaterialTheme.typography.bodyMedium) }
                else {
                    if (widget.size == HomeWidgetSize.Small) Text("${issues.size}", fontSize = 34.sp, fontWeight = FontWeight.Bold, color = LensStyle.warning)
                    issues.take(if (widget.size == HomeWidgetSize.Small) 1 else if (widget.size == HomeWidgetSize.Large) 3 else 2).forEach { issue ->
                        Row(Modifier.fillMaxWidth().clickable(onClick = devices), verticalAlignment = Alignment.CenterVertically) {
                            Text(PresentedFailure(issue).message, Modifier.weight(1f), style = MaterialTheme.typography.bodySmall, maxLines = 2)
                            Icon(Icons.Outlined.ChevronRight, null, Modifier.size(14.dp))
                        }
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
        else -> ShortcutWidget(model, widget, activeCamera, setActiveCamera, openSession, expand)
    }
}

/** Camera, campaign and portal widgets: one explicitly opened widget owns the connection (iOS shortcut). */
@Composable
private fun ShortcutWidget(model: WendyModel, widget: HomeWidget, activeCamera: String?, setActiveCamera: (String?) -> Unit, openSession: (HomeWidget) -> Unit, expand: (HomeWidget) -> Unit) {
    val scope = rememberCoroutineScope()
    var connecting by remember(widget.id) { mutableStateOf(false) }
    var problem by remember(widget.id) { mutableStateOf<String?>(null) }
    val device = model.resolve(widget.devices.firstOrNull())
    val offline = device == null || model.status(device) == "Offline"
    val connected = device != null && model.session != 0L && model.selected?.selector == device.selector
    fun openCamera() {
        val target = device ?: return
        if (connecting) return
        setActiveCamera(null); problem = null; connecting = true
        val account = model.account; val organization = model.organization
        scope.launch {
            try {
                val ready = model.connectAndWait(target)
                if (account != model.account || organization != model.organization) return@launch
                when {
                    !ready -> problem = model.detailError ?: "Could not connect to the device."
                    model.cameras.none { it.text("stable_id") == widget.resource } -> problem = model.camerasError ?: "This camera is unavailable. Edit the widget to choose another."
                    else -> setActiveCamera(widget.id)
                }
            } finally { connecting = false }
        }
    }
    Column(verticalArrangement = Arrangement.spacedBy(6.dp)) {
        if (widget.kind == HomeWidgetKind.Camera) {
            Box(Modifier.weight(1f).fillMaxWidth().clip(RoundedCornerShape(12.dp))) {
                val camera = model.cameras.firstOrNull { it.text("stable_id") == widget.resource }
                if (activeCamera == widget.id && connected && camera != null) CameraTile(model, camera, compact = true)
                else Box(Modifier.fillMaxSize().background(Color(0xFF171C23)).clickable(enabled = !offline && !connecting) { openCamera() }, contentAlignment = Alignment.Center) {
                    Icon(if (offline) Icons.Outlined.VideocamOff else Icons.Outlined.PlayCircle, if (offline) "Offline" else "Watch ${widget.resourceTitle}", Modifier.size(34.dp), tint = Color.White.copy(alpha = .85f))
                    Text(widget.resourceTitle, Modifier.align(Alignment.BottomStart).padding(6.dp), fontSize = 11.sp, color = Color.White, maxLines = 1, overflow = TextOverflow.Ellipsis)
                }
                IconButton(onClick = { expand(widget) }, Modifier.align(Alignment.BottomEnd).size(44.dp), enabled = !offline && !connecting) {
                    Box(Modifier.size(30.dp).background(Color.Black.copy(alpha = .6f), CircleShape), contentAlignment = Alignment.Center) { Icon(Icons.Outlined.OpenInFull, "Open camera full screen", Modifier.size(14.dp), tint = Color.White) }
                }
            }
        } else {
            Text(widget.resourceTitle, style = MaterialTheme.typography.titleSmall, maxLines = 2, overflow = TextOverflow.Ellipsis)
            Text(device?.title ?: widget.devices.firstOrNull()?.title ?: "Device unavailable", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1)
            if (widget.kind == HomeWidgetKind.Portal && connected) model.apps.firstOrNull { it.name == widget.resource && it.hasWebpage }?.let { app ->
                Text(":${app.httpPort} · ${app.state.title}", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1)
            }
            if (widget.size == HomeWidgetSize.Large) Box(Modifier.weight(1f).fillMaxWidth(), contentAlignment = Alignment.Center) { Icon(widget.kind.icon, null, Modifier.size(36.dp), tint = widget.kind.color.copy(alpha = .6f)) }
            else Spacer(Modifier.weight(1f))
        }
        if (widget.kind != HomeWidgetKind.Camera || (widget.size != HomeWidgetSize.Small && activeCamera != widget.id)) {
            val label = when {
                offline -> "Offline"
                widget.kind == HomeWidgetKind.Camera -> "Watch camera"
                widget.kind == HomeWidgetKind.Campaign -> if (widget.size == HomeWidgetSize.Small) "Record" else "Record a clip"
                else -> if (widget.size == HomeWidgetSize.Small) "Open" else "Open web portal"
            }
            WidgetButton(label, enabled = !offline && !connecting, danger = widget.kind == HomeWidgetKind.Campaign) {
                if (widget.kind == HomeWidgetKind.Camera) openCamera() else { setActiveCamera(null); openSession(widget) }
            }
        }
        if (connecting) LinearProgressIndicator(Modifier.fillMaxWidth().height(2.dp))
        problem?.let { Text(PresentedFailure(it).message, fontSize = 10.sp, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 2) }
    }
}

/** The latest recordings from one or every device, with real thumbnails (iOS gallery widget). */
@Composable
private fun GalleryWidget(model: WendyModel, widget: HomeWidget, recordings: () -> Unit, retry: () -> Unit) {
    val catalog = LocalRecordingCatalog.current ?: return
    val nav = LocalNavigator.current
    val libraries = catalog.reads.values.mapNotNull { it.library }.filter { library -> widget.devices.isEmpty() || widget.devices.any { it.id == recordingDeviceID(library.device) } }
    val entries = libraries.flatMap { library -> library.episodes(widget.label.ifEmpty { null }, widget.dataKind).map { library.device to it } }
        .sortedByDescending { it.second.startedNanos }
    val count = if (widget.size == HomeWidgetSize.Small) 1 else if (widget.size == HomeWidgetSize.Large) 6 else 3
    fun openPicker(device: Device, selected: String?) = nav.push(device.title) { EpisodePickerPage(model, recordingDeviceID(device), device, widget.label.ifEmpty { null }, widget.dataKind, selected) }
    Column(verticalArrangement = Arrangement.spacedBy(6.dp)) {
        if (entries.isEmpty()) {
            if (catalog.loading && libraries.isEmpty()) LoadingPlaceholder("Loading recordings…", PlaceholderStyle.Recordings, rows = 1) else Text("No matching recordings", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            Spacer(Modifier.weight(1f))
        } else {
            val columns = if (widget.size == HomeWidgetSize.Small) 1 else 3
            Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(6.dp)) {
                entries.take(count).chunked(columns).forEach { row ->
                    Row(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                        row.forEach { (device, episode) ->
                            Box(Modifier.weight(1f).height(if (widget.size == HomeWidgetSize.Large) 112.dp else 82.dp).clip(RoundedCornerShape(12.dp)).clickable(onClickLabel = "Open ${episode.title}") { openPicker(device, episode.id) }) {
                                RecordingThumbnail(device, episode, Modifier.fillMaxSize())
                                Box(Modifier.fillMaxSize().background(Brush.verticalGradient(0.5f to Color.Transparent, 1f to Color.Black.copy(alpha = .75f))))
                                Column(Modifier.align(Alignment.BottomStart).padding(6.dp)) {
                                    Text(episode.title, fontSize = 10.sp, fontWeight = FontWeight.SemiBold, color = Color.White, maxLines = 1, overflow = TextOverflow.Ellipsis)
                                    Text(recordingKindTitle(episode.kind), fontSize = 9.sp, color = Color.White, maxLines = 1)
                                }
                            }
                        }
                        repeat(columns - row.size) { Spacer(Modifier.weight(1f)) }
                    }
                }
            }
        }
        Row(verticalAlignment = Alignment.CenterVertically) {
            if (widget.size != HomeWidgetSize.Small) Text("${entries.size} recordings", fontSize = 10.sp, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1)
            Spacer(Modifier.weight(1f))
            Text("Open gallery", Modifier.clickable {
                val first = widget.devices.firstOrNull()
                val device = first?.let { reference -> libraries.firstOrNull { recordingDeviceID(it.device) == reference.id }?.device ?: model.resolve(reference) }
                if (first == null || device == null) recordings() else openPicker(device, null)
            }.padding(vertical = 4.dp), fontSize = 10.sp, fontWeight = FontWeight.SemiBold, color = LensStyle.accent)
        }
        if (libraries.any { it.labelCoverage != null }) Text("Some details unchecked", fontSize = 9.sp, color = MaterialTheme.colorScheme.onSurfaceVariant)
        RecordingSources.of(model).targets.filter { widget.devices.isEmpty() || widget.devices.any { d -> d.id == it.id } }
            .firstNotNullOfOrNull { catalog.reads[it.id]?.problem ?: catalog.detailProblems[it.id] }?.let { problem ->
                Row(Modifier.clickable(onClick = retry), horizontalArrangement = Arrangement.spacedBy(4.dp), verticalAlignment = Alignment.CenterVertically) {
                    Icon(Icons.Outlined.Refresh, "Retry loading recordings", Modifier.size(10.dp)); Text(PresentedFailure(problem).message, fontSize = 9.sp, maxLines = 1, overflow = TextOverflow.Ellipsis)
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

/**
 * A campaign or portal widget opened from Home (iOS HomeWidgetSession): it connects to its device,
 * then records a clip or opens the app's web page.
 */
@Composable
private fun HomeWidgetSession(model: WendyModel, widget: HomeWidget, dismiss: () -> Unit) {
    val scope = rememberCoroutineScope()
    val device = model.resolve(widget.devices.firstOrNull())
    var owned by remember { mutableLongStateOf(0L) }
    var busy by remember { mutableStateOf(true) }
    var error by remember { mutableStateOf<String?>(null) }
    var campaign by remember { mutableStateOf<CampaignInfo?>(null) }
    var installed by remember { mutableStateOf(true) }
    var episodes by remember { mutableStateOf(emptyList<JSONObject>()) }
    var checked by remember { mutableStateOf<Long?>(null) }
    var confirm by remember { mutableStateOf(false) }
    var attempt by remember { mutableIntStateOf(0) }
    val ready = owned != 0L && owned == model.session && device != null && model.selected?.selector == device.selector
    suspend fun refreshCampaign() {
        if (!ready) return
        busy = true
        try {
            val rows = model.backend.call("Campaigns", owned).items().map(CampaignInfo::parse)
            val active = model.backend.call("HomeCampaign", owned, widget.resource, false).items()
            if (!ready) return
            campaign = rows.firstOrNull { it.name == widget.resource }; installed = campaign != null; episodes = active; checked = System.currentTimeMillis()
            error = if (campaign == null) "This campaign is no longer installed. Edit the widget to choose another." else null
        } catch (e: CancellationException) { throw e } catch (e: Exception) { if (ready) error = PresentedFailure(e.message ?: "").details } finally { busy = false }
    }
    LaunchedEffect(attempt) {
        if (device == null) { busy = false; error = "Device unavailable in this account and organization."; return@LaunchedEffect }
        busy = true; error = null
        if (!model.connectAndWait(device)) { busy = false; error = model.detailError ?: "Could not connect to the device."; return@LaunchedEffect }
        owned = model.session; busy = false
        if (widget.kind == HomeWidgetKind.Campaign) refreshCampaign()
        if (widget.kind == HomeWidgetKind.Portal) {
            val app = model.apps.firstOrNull { it.name == widget.resource && it.hasWebpage }
            if (app == null) { error = model.appsError ?: "This app no longer publishes a web endpoint."; return@LaunchedEffect }
            busy = true
            try {
                val url = model.backend.call("OpenApp", app.name, owned) as? String
                if (url != null && owned == model.session) { model.linkRequest = url; dismiss() }
            } catch (e: CancellationException) { throw e } catch (e: Exception) { error = PresentedFailure(e.message ?: "").details } finally { busy = false }
        }
    }
    // Clip status follows the device while the sheet is open.
    LaunchedEffect(ready, model.inForeground) {
        if (!ready || widget.kind != HomeWidgetKind.Campaign || !model.inForeground) return@LaunchedEffect
        while (true) { kotlinx.coroutines.delay(5_000); if (!busy) refreshCampaign() }
    }
    NavigationSheet(widget.displayTitle, dismiss) {
        PageColumn {
            when {
                busy && (owned == 0L || widget.kind == HomeWidgetKind.Portal) -> item { LensLoading(if (owned == 0L) "Connecting…" else "Opening portal…") }
                ready && widget.kind == HomeWidgetKind.Campaign -> {
                    formSection {
                        Text(widget.resourceTitle, style = MaterialTheme.typography.titleMedium, maxLines = 3, overflow = TextOverflow.Ellipsis)
                        Text(device?.title ?: "", maxLines = 2, overflow = TextOverflow.Ellipsis)
                        campaign?.let { LabeledRow("Campaign", it.state); if (it.problem.isNotEmpty()) SecondaryText(it.problem) }
                        episodes.forEach { LabeledRow(it.text("name").ifEmpty { it.text("id") }, it.text("state")) }
                        if (episodes.isEmpty()) SecondaryText("No active clips reported.")
                        checked?.let { SecondaryText("Checked ${DateFormat.getTimeInstance(DateFormat.MEDIUM).format(Date(it))}") }
                    }
                    formSection(footer = "Clips stop automatically. Notifications and upload follow the campaign’s settings.") {
                        ActionRow("Record clip", Icons.Outlined.FiberManualRecord, enabled = !busy && installed && campaign != null && episodes.isEmpty() && model.inForeground) { confirm = true }
                        ActionRow("Refresh status", Icons.Outlined.Refresh, enabled = !busy) { scope.launch { refreshCampaign() } }
                    }
                    error?.let { item { FormSection { SecondaryText(it) } } }
                }
                ready && error != null -> item { Column { Text("Could not open widget", style = MaterialTheme.typography.titleMedium); FailureNote(error!!); TextButton(onClick = { attempt++ }) { Text("Try again") } } }
                !ready && !busy -> item { Column { Text("Widget unavailable", style = MaterialTheme.typography.titleMedium); FailureNote(error ?: "The selected device is no longer connected."); TextButton(onClick = { attempt++ }) { Text("Try again") } } }
            }
        }
    }
    if (confirm) ConfirmAction("Record a clip on ${device?.title ?: "this device"}?", "Run ${widget.resourceTitle} once with its configured sources, duration, notifications, and upload policy.", "Record clip", destructive = false,
        onDismiss = { confirm = false }) {
        confirm = false
        scope.launch {
            if (!ready || busy || campaign == null) return@launch
            busy = true; error = null
            try { val rows = model.backend.call("HomeCampaign", owned, widget.resource, true).items(); if (ready) { episodes = rows; checked = System.currentTimeMillis() } }
            catch (e: CancellationException) { throw e } catch (e: Exception) { if (ready) error = PresentedFailure(e.message ?: "").details } finally { busy = false }
        }
    }
}

/** A camera widget opened full screen (iOS fullScreenCover with HomeWidgetSession). */
@Composable
private fun HomeWidgetFullScreen(model: WendyModel, widget: HomeWidget, dismiss: () -> Unit) {
    val device = model.resolve(widget.devices.firstOrNull())
    var ready by remember { mutableStateOf(false) }
    var busy by remember { mutableStateOf(true) }
    var error by remember { mutableStateOf<String?>(null) }
    var attempt by remember { mutableIntStateOf(0) }
    LaunchedEffect(attempt) {
        busy = true; error = null
        if (device == null) { error = "Device unavailable in this account and organization."; busy = false; return@LaunchedEffect }
        ready = model.connectAndWait(device)
        if (!ready) error = model.detailError ?: "Could not connect to the device."
        busy = false
    }
    Dialog(onDismissRequest = dismiss, properties = DialogProperties(usePlatformDefaultWidth = false)) {
        Surface(Modifier.fillMaxSize(), color = MaterialTheme.colorScheme.background) {
            Column(Modifier.fillMaxSize().windowInsetsPadding(WindowInsets.systemBars)) {
                Row(Modifier.fillMaxWidth().padding(horizontal = 12.dp).heightIn(min = 48.dp), verticalAlignment = Alignment.CenterVertically) {
                    Text(widget.displayTitle, Modifier.weight(1f).padding(horizontal = 8.dp), style = MaterialTheme.typography.titleMedium, maxLines = 1, overflow = TextOverflow.Ellipsis)
                    TextButton(onClick = dismiss) { Text("Done") }
                }
                Box(Modifier.weight(1f).fillMaxWidth().padding(16.dp), contentAlignment = Alignment.Center) {
                    val camera = model.cameras.firstOrNull { it.text("stable_id") == widget.resource }
                    when {
                        busy -> LensLoading("Connecting…")
                        ready && device != null && model.selected?.selector == device.selector && camera != null -> Column(horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(12.dp)) {
                            Box(Modifier.fillMaxWidth().aspectRatio(4f / 3f).clip(RoundedCornerShape(22.dp))) { CameraTile(model, camera) }
                            Text(camera.text("name"), style = MaterialTheme.typography.titleMedium)
                        }
                        ready -> EmptyMessage("Camera unavailable", "This camera was not reported by the device. Edit the widget to choose another camera.", Icons.Outlined.VideocamOff)
                        else -> Column { Text("Widget unavailable", style = MaterialTheme.typography.titleMedium); FailureNote(error ?: "The selected device is no longer connected."); TextButton(onClick = { attempt++ }) { Text("Try again") } }
                    }
                }
            }
        }
    }
}

/** Choose a widget to add (iOS HomeWidgetManager). */
@Composable
fun HomeWidgetManager(model: WendyModel, store: HomeWidgetStore) {
    val nav = LocalNavigator.current
    PageColumn {
        item {
            LensSection(null) {
                HomeWidgetKind.entries.forEachIndexed { index, kind ->
                    if (index > 0) HorizontalDivider(Modifier.padding(start = 80.dp), color = MaterialTheme.colorScheme.outlineVariant)
                    Row(Modifier.fillMaxWidth().clickable(onClickLabel = "Add ${kind.title}") {
                        nav.push(kind.title) { HomeWidgetEditor(model, HomeWidget(kind = kind)) { store.save(it); nav.pop(); nav.pop() } }
                    }.padding(horizontal = 16.dp, vertical = 14.dp), horizontalArrangement = Arrangement.spacedBy(16.dp), verticalAlignment = Alignment.CenterVertically) {
                        Box(Modifier.size(48.dp).background(LensStyle.visible(kind.tint(LensStyle.isDark)).let { if (it == MaterialTheme.colorScheme.onSurface) Color(0xFF3A3A3C) else it }, RoundedCornerShape(14.dp)), contentAlignment = Alignment.Center) {
                            Icon(kind.icon, null, Modifier.size(24.dp), tint = Color.White)
                        }
                        Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(3.dp)) {
                            Text(kind.title, style = MaterialTheme.typography.titleMedium)
                            Text(kind.summary, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                        }
                        Box(Modifier.size(40.dp).background(LensStyle.green, CircleShape), contentAlignment = Alignment.Center) { Icon(Icons.Outlined.Add, null, tint = Color.White) }
                    }
                }
            }
        }
    }
}

/** Configure a widget's title, devices, resource and filters (iOS HomeWidgetEditor). */
@Composable
fun HomeWidgetEditor(model: WendyModel, initial: HomeWidget, save: (HomeWidget) -> Unit) {
    val nav = LocalNavigator.current
    var widget by remember { mutableStateOf(initial) }
    val devices = model.widgetDevices().filter { widget.kind == HomeWidgetKind.Favorites || !it.lite }
    val device = model.resolve(widget.devices.firstOrNull())
    val resourceKind = when (widget.kind) { HomeWidgetKind.Camera -> "Camera"; HomeWidgetKind.Campaign -> "Campaign"; else -> "App" }
    val deviceSummary = when {
        widget.kind == HomeWidgetKind.Favorites -> if (widget.devices.isEmpty()) "Choose devices" else "${widget.devices.size} selected"
        widget.kind == HomeWidgetKind.Gallery && widget.devices.isEmpty() -> "All devices"
        else -> widget.devices.firstOrNull()?.title ?: "Choose a device"
    }
    val page = LocalNavPage.current
    LaunchedEffect(widget) { page?.actions = { TextButton(onClick = { save(widget.copy(label = widget.label.trim())) }, enabled = widget.valid) { Text("Done", fontWeight = FontWeight.SemiBold) } } }
    PageColumn {
        formSection("Title") {
            OutlinedTextField(widget.title, { widget = widget.copy(title = it) }, Modifier.fillMaxWidth(), placeholder = { Text(widget.kind.title) }, singleLine = true)
        }
        if (!widget.kind.builtIn) formSection(footer = if (widget.kind == HomeWidgetKind.Favorites) "Choose up to six favorite devices." else if (widget.kind != HomeWidgetKind.Gallery && device == null) "Choose a device to see its available choices." else null) {
            NavRow(if (widget.kind == HomeWidgetKind.Favorites) "Devices" else "Device", value = deviceSummary) {
                nav.push(if (widget.kind == HomeWidgetKind.Favorites) "Favorite devices" else "Choose a device") { WidgetDevicePicker(model, widget, devices) { widget = it } }
            }
            if (widget.kind != HomeWidgetKind.Favorites && widget.kind != HomeWidgetKind.Gallery) NavRow(resourceKind, value = widget.resourceTitle.ifEmpty { "Choose a ${resourceKind.lowercase()}" }, enabled = device != null) {
                nav.push("Choose a ${resourceKind.lowercase()}") { WidgetResourcePicker(model, widget, resourceKind) { widget = it } }
            }
        }
        if (widget.kind == HomeWidgetKind.Gallery) formSection("Recordings") {
            ChoiceField("Kind", listOf("" to "All kinds") + recordingKinds.map { it to if (it == "unknown") "Unknown" else recordingKindTitle(it) }, widget.dataKind ?: "") { widget = widget.copy(dataKind = it.ifEmpty { null }) }
            OutlinedTextField(widget.label, { widget = widget.copy(label = it) }, Modifier.fillMaxWidth(), label = { Text("Label (optional)") }, singleLine = true)
        }
        if (widget.kind == HomeWidgetKind.Campaign) formSection { SecondaryText("Clips finish automatically using the campaign’s settings.") }
    }
}

@Composable
private fun SelectionRow(title: String, subtitle: String? = null, selected: Boolean, enabled: Boolean = true, onClick: () -> Unit) {
    Row(Modifier.fillMaxWidth().clickable(enabled = enabled, onClick = onClick).padding(vertical = 10.dp), verticalAlignment = Alignment.CenterVertically) {
        Column(Modifier.weight(1f)) {
            Text(title, maxLines = 3, overflow = TextOverflow.Ellipsis, color = if (enabled) MaterialTheme.colorScheme.onSurface else MaterialTheme.colorScheme.onSurfaceVariant)
            if (subtitle != null) Text(subtitle, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 2)
        }
        if (selected) Icon(Icons.Outlined.Check, "Selected", tint = LensStyle.accent)
    }
}

/** Searchable devices, offline last; favorites pick up to six, other widgets one (iOS devicePicker). */
@Composable
private fun WidgetDevicePicker(model: WendyModel, initial: HomeWidget, devices: List<Device>, update: (HomeWidget) -> Unit) {
    val nav = LocalNavigator.current
    var widget by remember { mutableStateOf(initial) }
    var search by remember { mutableStateOf("") }
    fun set(next: HomeWidget) { widget = next; update(next) }
    val matching = devices.filter { search.isBlank() || it.title.contains(search.trim(), ignoreCase = true) }
        .sortedWith(compareBy<Device> { model.status(it) == "Offline" }.thenBy { it.title.lowercase() })
    val unavailable = widget.devices.filter { reference -> devices.none { widgetDeviceID(it) == reference.id } }
    PageColumn {
        item { OutlinedTextField(search, { search = it }, Modifier.fillMaxWidth(), placeholder = { Text("Search devices") }, leadingIcon = { Icon(Icons.Outlined.Search, null) }, singleLine = true) }
        if (widget.kind == HomeWidgetKind.Gallery) formSection { SelectionRow("All devices", selected = widget.devices.isEmpty()) { set(widget.copy(devices = emptyList())); nav.pop() } }
        formSection(footer = if (widget.kind == HomeWidgetKind.Favorites) "${widget.devices.size} of 6 devices selected" else null) {
            matching.forEach { candidate ->
                val id = widgetDeviceID(candidate)
                val selected = widget.devices.any { it.id == id }
                SelectionRow(candidate.title, "${model.status(candidate)} · ${if (candidate.isCloud) "Cloud" else "Local"}", selected,
                    enabled = widget.kind != HomeWidgetKind.Favorites || selected || widget.devices.size < 6) {
                    if (widget.kind == HomeWidgetKind.Favorites) {
                        set(widget.copy(devices = if (selected) widget.devices.filter { it.id != id } else (widget.devices + HomeWidgetDevice(id, candidate.title)).take(6)))
                    } else {
                        if (!selected) set(widget.copy(devices = listOf(HomeWidgetDevice(id, candidate.title)), resource = "", resourceTitle = ""))
                        nav.pop()
                    }
                }
            }
            if (matching.isEmpty()) SecondaryText(if (devices.isEmpty()) "No devices available" else "No matching devices")
        }
        if (unavailable.isNotEmpty()) formSection("Unavailable devices") {
            unavailable.forEach { reference ->
                Row(verticalAlignment = Alignment.CenterVertically) {
                    Text(reference.title, Modifier.weight(1f), color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 3, overflow = TextOverflow.Ellipsis)
                    IconButton(onClick = {
                        val rest = widget.devices.filter { it.id != reference.id }
                        set(if (widget.kind == HomeWidgetKind.Favorites) widget.copy(devices = rest) else widget.copy(devices = rest, resource = "", resourceTitle = ""))
                    }) { Icon(Icons.Outlined.RemoveCircleOutline, "Remove") }
                }
            }
        }
    }
}

/** The chosen device's cameras, campaigns or web apps; opening it connects to the device (iOS resourcePicker). */
@Composable
private fun WidgetResourcePicker(model: WendyModel, initial: HomeWidget, resourceKind: String, update: (HomeWidget) -> Unit) {
    val nav = LocalNavigator.current
    var widget by remember { mutableStateOf(initial) }
    var search by remember { mutableStateOf("") }
    var choices by remember { mutableStateOf(emptyList<Pair<String, String>>()) }
    var loading by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    var attempt by remember { mutableIntStateOf(0) }
    val device = model.resolve(widget.devices.firstOrNull())
    LaunchedEffect(widget.devices.firstOrNull()?.id, attempt) {
        val target = device ?: return@LaunchedEffect
        loading = true; error = null
        try {
            if (!model.connectAndWait(target)) { error = model.detailError ?: "Could not connect to this device."; return@LaunchedEffect }
            val session = model.session
            when (widget.kind) {
                HomeWidgetKind.Camera -> { model.camerasError?.let { throw Exception(it) }; choices = model.cameras.filter { it.text("stable_id").isNotEmpty() }.map { it.text("stable_id") to it.text("name") }; if (choices.isEmpty()) error = "No cameras with a stable identity were reported." }
                HomeWidgetKind.Campaign -> { val rows = model.backend.call("Campaigns", session).items().map(CampaignInfo::parse); if (session == model.session) { choices = rows.map { it.name to it.name }; if (rows.isEmpty()) error = "No campaigns are installed on this device." } }
                HomeWidgetKind.Portal -> { model.appsError?.let { throw Exception(it) }; choices = model.apps.filter { it.hasWebpage }.map { it.name to it.name }; if (choices.isEmpty()) error = "No apps publish a web endpoint." }
                else -> {}
            }
        } catch (e: CancellationException) { throw e } catch (e: Exception) { error = PresentedFailure(e.message ?: "").details } finally { loading = false }
    }
    fun matches(title: String) = search.isBlank() || title.contains(search.trim(), ignoreCase = true)
    PageColumn {
        item { OutlinedTextField(search, { search = it }, Modifier.fillMaxWidth(), placeholder = { Text("Search ${resourceKind.lowercase()}s") }, leadingIcon = { Icon(Icons.Outlined.Search, null) }, singleLine = true) }
        if (loading) item { LoadingRow("Loading ${resourceKind.lowercase()} choices…") }
        if (widget.resource.isNotEmpty()) formSection("Selected") { SelectionRow(widget.resourceTitle.ifEmpty { widget.resource }, selected = true) { nav.pop() } }
        formSection {
            choices.filter { it.first != widget.resource && matches(it.second) }.forEach { (id, title) ->
                SelectionRow(title, selected = false) { widget = widget.copy(resource = id, resourceTitle = title); update(widget); nav.pop() }
            }
            if (!loading && error == null && search.isNotBlank() && choices.none { matches(it.second) }) SecondaryText("No matching choices")
        }
        error?.let { message -> formSection { SecondaryText(message); TextButton(onClick = { attempt++ }, enabled = !loading) { Text("Try again") } } }
    }
}
