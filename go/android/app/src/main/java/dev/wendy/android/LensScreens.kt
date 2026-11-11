package dev.wendy.android

import androidx.compose.animation.core.LinearEasing
import androidx.compose.animation.core.animateFloat
import androidx.compose.animation.core.infiniteRepeatable
import androidx.compose.animation.core.rememberInfiniteTransition
import androidx.compose.animation.core.tween
import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.material.icons.automirrored.outlined.*
import androidx.compose.material3.*
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.drawBehind
import androidx.compose.ui.geometry.CornerRadius
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.PathEffect
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.graphics.drawscope.rotate
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import org.json.JSONObject
import java.text.DateFormat
import java.util.Date

// The lens-based app from PR #2715: first-launch lens choice, General Home,
// the Devices lens (Home, Data, Activity) and device and app pages.

@Composable
fun LensPreferenceScreen(initial: Lens = Lens.Devices, save: (Lens) -> Unit) {
    var choice by remember { mutableStateOf(initial) }
    Column(Modifier.fillMaxSize().background(MaterialTheme.colorScheme.background).verticalScroll(rememberScrollState()).statusBarsPadding().padding(22.dp).padding(top = 32.dp),
        verticalArrangement = Arrangement.spacedBy(18.dp)) {
        Box(Modifier.size(64.dp).background(MaterialTheme.colorScheme.surface, RoundedCornerShape(15.dp)), contentAlignment = Alignment.Center) { WendyMark(Modifier.size(40.dp, 30.dp)) }
        Text("Welcome to Wendy", style = MaterialTheme.typography.headlineLarge, fontWeight = FontWeight.Bold)
        Text("What do you work with most? We’ll set up Home around it.", color = MaterialTheme.colorScheme.onSurfaceVariant)
        Lens.entries.forEach { lens ->
            Surface(onClick = { choice = lens }, shape = RoundedCornerShape(20.dp), color = MaterialTheme.colorScheme.surface) {
                Row(Modifier.fillMaxWidth().padding(14.dp), horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
                    LensIcon(lens.icon, lens.color)
                    Column(Modifier.weight(1f)) { Text(lens.choice, style = MaterialTheme.typography.titleMedium); SecondaryText(lens.summary) }
                    Icon(if (choice == lens) Icons.Outlined.CheckCircle else Icons.Outlined.Circle, if (choice == lens) "Selected" else null, tint = lens.color)
                }
            }
        }
        LensButton("Continue", primary = true, modifier = Modifier.fillMaxWidth()) { save(choice) }
        SecondaryText("You can change this anytime in Settings.")
    }
}

/** General Home: lens tiles and the widget deck (iOS MobileGeneralHome). */
@Composable
fun GeneralHomeScreen(model: WendyModel, widgets: HomeWidgetStore, editing: Boolean, setEditing: (Boolean) -> Unit, open: (Lens) -> Unit,
                      setup: () -> Unit, notifications: () -> Unit, recordings: () -> Unit, openDevice: (Device) -> Unit) {
    LaunchedEffect(model.account, model.organization) { setEditing(false); widgets.load(model.account, model.organization) }
    Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(horizontal = 16.dp).padding(bottom = 96.dp), verticalArrangement = Arrangement.spacedBy(16.dp)) {
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Lens.entries.forEach { lens ->
                Surface(onClick = { open(lens) }, Modifier.weight(1f).heightIn(min = 76.dp), shape = RoundedCornerShape(16.dp), color = lens.color.copy(alpha = .12f)) {
                    Column(Modifier.padding(vertical = 12.dp), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(10.dp)) {
                        LensIcon(lens.icon, lens.color)
                        Row(verticalAlignment = Alignment.CenterVertically) {
                            Text(lens.title, style = MaterialTheme.typography.titleSmall, color = lens.textColor)
                            Icon(Icons.Outlined.ChevronRight, null, Modifier.size(16.dp), tint = lens.textColor)
                        }
                    }
                }
            }
        }
        if (model.initialized) HomeWidgetDeck(model, widgets, editing, setEditing, setup, recordings, notifications, { open(Lens.Devices) }, openDevice)
        else LensLoading("Loading Home…")
    }
}

/** The app model for screens that only need it to draw shared chrome. */
val LocalWendyModel = staticCompositionLocalOf<WendyModel?> { null }

enum class DevicesTab(val title: String) { Home("Home"), Data("Data"), Activity("Activity") }

/** The Devices lens: Home, Data and Activity, with Ask Wendy above the tabs (iOS lens tabs). */
@Composable
fun DevicesLens(model: WendyModel, tab: DevicesTab, select: (DevicesTab) -> Unit, setup: () -> Unit, notifications: () -> Unit, askWendy: () -> Unit, openDevice: (Device) -> Unit,
                activity: ActivitySegment, setActivity: (ActivitySegment) -> Unit) {
    val page = LocalNavPage.current
    LaunchedEffect(tab) { page?.title = if (tab == DevicesTab.Home) "Devices" else tab.title }
    Column(Modifier.fillMaxSize()) {
        Box(Modifier.weight(1f)) {
            when (tab) {
                DevicesTab.Home -> DevicesHome(model, setup, notifications, openDevice)
                DevicesTab.Data -> DevicesDataTab(model, openDevice)
                DevicesTab.Activity -> DeviceActivityScreen(model, activity, setActivity, openDevice)
            }
        }
        AskWendyBar(model, askWendy)
        NavigationBar(containerColor = MaterialTheme.colorScheme.surface) {
            DevicesTab.entries.forEach { item ->
                NavigationBarItem(selected = tab == item, onClick = { select(item) }, label = { Text(item.title) },
                    icon = { Icon(when (item) { DevicesTab.Home -> Icons.Outlined.Home; DevicesTab.Data -> Icons.Outlined.Storage; DevicesTab.Activity -> Icons.Outlined.Schedule }, null) },
                    colors = NavigationBarItemDefaults.colors(selectedIconColor = LensStyle.accent, selectedTextColor = LensStyle.accent, indicatorColor = LensStyle.green.copy(alpha = .12f)))
            }
        }
    }
}

/** The capsule above the tab bar that opens Ask Wendy for the connected device. */
@Composable
fun AskWendyBar(model: WendyModel, open: () -> Unit) {
    val chat = model.deviceChat
    Surface(onClick = open, Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 6.dp), shape = CircleShape, color = MaterialTheme.colorScheme.surface,
        border = BorderStroke(1.dp, MaterialTheme.colorScheme.outlineVariant), shadowElevation = 2.dp) {
        Row(Modifier.padding(horizontal = 16.dp).heightIn(min = 44.dp), horizontalArrangement = Arrangement.spacedBy(10.dp), verticalAlignment = Alignment.CenterVertically) {
            Icon(Icons.Outlined.AutoAwesome, null, Modifier.size(18.dp), tint = LensStyle.accent)
            Text("Ask Wendy about ${if (model.session != 0L) model.selected?.title ?: "a device" else "a device"}…", Modifier.weight(1f), maxLines = 1, style = MaterialTheme.typography.bodyMedium)
            if ((chat?.unreadReplies ?: 0) > 0 || chat?.approval != null) Box(Modifier.size(8.dp).background(LensStyle.danger, CircleShape))
        }
    }
}

private enum class InventorySource(val title: String, val tile: String) { Local("Local devices", "Local · nearby"), Cloud("Cloud devices", "Cloud") }

private fun WendyModel.devicesFor(source: InventorySource) = if (source == InventorySource.Local) nearbyDevices else cloudDevices
private fun WendyModel.countFor(source: InventorySource): Int? = when (source) {
    InventorySource.Local -> if (nearbyLoaded && nearbyError == null) nearbyDevices.size else null
    InventorySource.Cloud -> if (organization.isNotEmpty() && cloudLoaded && cloudError == null) cloudDevices.size else null
}

private data class PresenceCount(val title: String, val count: Int, val color: Color)
private fun presenceCounts(model: WendyModel): List<PresenceCount> {
    if (model.organization.isNotEmpty()) {
        val rows = model.presence ?: return emptyList()
        if (model.cloudError != null || !model.cloudLoaded || System.currentTimeMillis() - model.presenceAt > 60_000) return emptyList()
        if (model.presenceError.isNotEmpty()) return listOf(PresenceCount("unknown", model.cloudDevices.size, LensStyle.offline))
        val online = rows.count { it.online }
        return listOf(PresenceCount("online", online, LensStyle.online), PresenceCount("offline", rows.size - online, LensStyle.offline)).filter { it.count > 0 }
    }
    if (!model.nearbyLoaded || model.nearbyError != null) return emptyList()
    return listOf(PresenceCount("nearby", model.nearbyDevices.size, LensStyle.green)).filter { it.count > 0 }
}

/** Devices Home: counts and presence, Local and Cloud tiles, setup, attention and recent data (iOS MobileDevicesHome). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun DevicesHome(model: WendyModel, setup: () -> Unit, notifications: () -> Unit, openDevice: (Device) -> Unit) {
    val nav = LocalNavigator.current
    val scope = rememberCoroutineScope()
    var refreshing by remember { mutableStateOf(false) }
    val organization = model.organizations.firstOrNull { it.text("id") == model.organization }?.text("name")
    LaunchedEffect(model.account, model.organization) { model.refresh() }
    PullToRefreshBox(refreshing, onRefresh = { scope.launch { refreshing = true; model.refresh(); refreshing = false } }) {
        Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(horizontal = 16.dp, vertical = 8.dp), verticalArrangement = Arrangement.spacedBy(18.dp)) {
            Surface(Modifier.fillMaxWidth(), shape = RoundedCornerShape(26.dp), color = MaterialTheme.colorScheme.surface) {
                Column(Modifier.padding(18.dp), verticalArrangement = Arrangement.spacedBy(14.dp)) {
                    val total = model.countFor(if (organization == null) InventorySource.Local else InventorySource.Cloud)
                    Row(verticalAlignment = Alignment.Bottom) {
                        Column(Modifier.weight(1f)) {
                            Text(total?.toString() ?: "—", fontSize = 44.sp, fontWeight = FontWeight.Bold)
                            Text(organization?.let { "devices in $it" } ?: "devices nearby", style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 2)
                        }
                        Column(horizontalAlignment = Alignment.End, verticalArrangement = Arrangement.spacedBy(4.dp)) {
                            presenceCounts(model).forEach { value ->
                                Row(horizontalArrangement = Arrangement.spacedBy(5.dp), verticalAlignment = Alignment.CenterVertically) {
                                    Box(Modifier.size(7.dp).background(value.color, CircleShape)); Text("${value.count} ${value.title}", style = MaterialTheme.typography.labelMedium, fontWeight = FontWeight.SemiBold)
                                }
                            }
                        }
                    }
                    // Zero counts have no segment: a zero weight is invalid in Compose.
                    val counts = presenceCounts(model).filter { it.count > 0 }
                    val sum = counts.sumOf { it.count }.coerceAtLeast(1)
                    Row(Modifier.fillMaxWidth().height(7.dp).background(MaterialTheme.colorScheme.surfaceVariant, CircleShape).clipToCircle()) {
                        counts.forEach { Box(Modifier.weight(it.count.toFloat() / sum).fillMaxHeight().background(it.color)) }
                        if (counts.isEmpty()) Spacer(Modifier.weight(1f))
                    }
                    Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        InventorySource.entries.forEach { source ->
                            Surface(onClick = { nav.push(source.title) { InventoryPage(model, source, openDevice) } }, Modifier.weight(1f), shape = RoundedCornerShape(16.dp), color = MaterialTheme.colorScheme.background) {
                                Row(Modifier.padding(12.dp), horizontalArrangement = Arrangement.spacedBy(9.dp), verticalAlignment = Alignment.CenterVertically) {
                                    LensIcon(if (source == InventorySource.Local) Icons.Outlined.SettingsInputAntenna else Icons.Outlined.Public, size = 34.dp)
                                    Column(Modifier.weight(1f)) {
                                        Text(model.countFor(source)?.toString() ?: "—", style = MaterialTheme.typography.titleMedium, fontWeight = FontWeight.Bold)
                                        Text(source.tile, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                                    }
                                    Icon(Icons.Outlined.ChevronRight, null, Modifier.size(16.dp), tint = MaterialTheme.colorScheme.outline)
                                }
                            }
                        }
                    }
                }
            }
            Surface(onClick = setup, Modifier.fillMaxWidth().dashedBorder(LensStyle.accent.copy(alpha = .45f), 22.dp), shape = RoundedCornerShape(22.dp), color = LensStyle.accent.copy(alpha = .05f)) {
                Row(Modifier.padding(14.dp), horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
                    Box(Modifier.size(36.dp).background(LensStyle.green, CircleShape), contentAlignment = Alignment.Center) { Icon(Icons.Outlined.Add, null, tint = Color.White) }
                    Column { Text("Set up a new device", style = MaterialTheme.typography.titleMedium, color = LensStyle.accent); SecondaryText("Nearby over Bluetooth") }
                }
            }
            HomeAttention(model, openDevice, notifications)
            RecentRecordings(model, openDevice)
            Spacer(Modifier.height(8.dp))
        }
    }
}

private fun Modifier.clipToCircle() = this.clip(CircleShape)

/** A dashed rounded outline, as the iOS setup call to action uses. */
fun Modifier.dashedBorder(color: Color, radius: androidx.compose.ui.unit.Dp) = this.drawBehind {
    drawRoundRect(color, style = Stroke(width = 1.5.dp.toPx(), pathEffect = PathEffect.dashPathEffect(floatArrayOf(5.dp.toPx(), 4.dp.toPx()))), cornerRadius = CornerRadius(radius.toPx()))
}

/** One issue worth opening first, or a calm "no issues" row (iOS MobileHomeAttention). */
@Composable
fun HomeAttention(model: WendyModel, openDevice: (Device) -> Unit, openActivity: () -> Unit) {
    LensSection("Needs attention") {
        val issue = model.nearbyError ?: model.cloudError
        val crashing = if (model.session != 0L) model.apps.firstOrNull { it.state == AppState.CrashLooping } else null
        val offline = model.presence?.takeIf { model.cloudError == null && model.presenceError.isEmpty() && System.currentTimeMillis() - model.presenceAt < 60_000 }?.firstOrNull { !it.online }
        when {
            issue != null -> Box(Modifier.padding(16.dp)) { FailureNote(issue) }
            crashing != null && model.selected != null -> LensRow(crashing.name, "Crash looping · ${model.selected!!.title}", Icons.Outlined.WarningAmber, LensStyle.warning) { openDevice(model.selected!!) }
            model.selected != null && model.detailError != null -> LensRow(model.selected!!.title, PresentedFailure(model.detailError!!).message, Icons.Outlined.WifiOff, LensStyle.warning) { openDevice(model.selected!!) }
            offline != null -> LensRow(offline.name, "Offline in Wendy Cloud", Icons.Outlined.WifiOff, LensStyle.offline) { openDevice(offline.device) }
            else -> LensRow("No reported issues", "View device activity", Icons.Outlined.CheckCircle, onClick = openActivity)
        }
    }
}

/** The three latest recordings on the connected device (iOS MobileRecordingSection). */
@Composable
fun RecentRecordings(model: WendyModel, openDevice: (Device) -> Unit, recent: Boolean = true) {
    val nav = LocalNavigator.current
    val session = model.session
    val device = model.selected
    var episodes by remember(session) { mutableStateOf<List<JSONObject>?>(null) }
    var error by remember(session) { mutableStateOf<String?>(null) }
    LaunchedEffect(session) {
        if (session == 0L || model.lite) return@LaunchedEffect
        try { episodes = model.backend.call("VisualEpisodes", session).items().sortedByDescending { it.number("started_unix_nanos") } }
        catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message }
    }
    LensSection(if (recent) "Recent data" else "Recordings") {
        val rows = episodes
        when {
            session == 0L || device == null || model.lite -> Text("Connect a device to see its recordings.", Modifier.padding(16.dp), color = MaterialTheme.colorScheme.onSurfaceVariant)
            error != null -> Box(Modifier.padding(16.dp)) { FailureNote(error!!) }
            rows == null -> LoadingPlaceholder("Loading recordings…", PlaceholderStyle.Recordings, rows = 2, modifier = Modifier.padding(16.dp))
            rows.isEmpty() -> Text("No recordings on ${device.title}.", Modifier.padding(16.dp), color = MaterialTheme.colorScheme.onSurfaceVariant)
            else -> (if (recent) rows.take(3) else rows).forEachIndexed { index, episode ->
                if (index > 0) LensDivider()
                val title = episode.text("name").ifEmpty { episode.text("campaign_name").ifEmpty { "Recording" } }
                LensRow(title, device.title, Icons.Outlined.Storage, LensStyle.offline, DateFormat.getDateInstance(DateFormat.MEDIUM).format(Date(episode.number("started_unix_nanos") / 1_000_000))) {
                    nav.push(title) { EpisodeScreen(model, episode.text("id")) }
                }
            }
        }
    }
}

/** Local or Cloud devices, online first, offline rows muted (iOS MobileInventoryList). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun InventoryPage(model: WendyModel, source: InventorySource, openDevice: (Device) -> Unit) {
    val scope = rememberCoroutineScope()
    var search by remember { mutableStateOf("") }
    var refreshing by remember { mutableStateOf(false) }
    suspend fun refresh() { if (source == InventorySource.Local) model.refreshNearby() else model.refreshCloud() }
    LaunchedEffect(source) { refresh() }
    val matching = model.devicesFor(source).filter { search.isEmpty() || it.title.contains(search, true) }
        .sortedWith(compareBy<Device> { model.isOffline(it) }.thenBy(String.CASE_INSENSITIVE_ORDER) { it.title }.thenBy { it.id })
    val error = if (source == InventorySource.Local) model.nearbyError else model.cloudError
    val loading = if (source == InventorySource.Local) model.nearbyLoading else model.cloudLoading
    PullToRefreshBox(refreshing, onRefresh = { scope.launch { refreshing = true; refresh(); refreshing = false } }) {
        LazyColumn(Modifier.fillMaxSize(), contentPadding = PaddingValues(16.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            item { OutlinedTextField(search, { search = it }, Modifier.fillMaxWidth(), singleLine = true, placeholder = { Text("Find a device") }, leadingIcon = { Icon(Icons.Outlined.Search, null) }, shape = RoundedCornerShape(14.dp)) }
            item {
                LensSection(if (source == InventorySource.Local) "Local · nearby now" else "Wendy Cloud") {
                    if (source == InventorySource.Cloud && model.organization.isEmpty() && !loading && error == null) {
                        LensRow(if (model.accounts.isEmpty()) "Sign in to Wendy Cloud" else "Choose an organization", "", Icons.Outlined.CloudQueue) { if (model.accounts.isEmpty()) model.signIn() else model.requestSettings = true }
                        return@LensSection
                    }
                    matching.forEachIndexed { index, device ->
                        if (index > 0) LensDivider()
                        val connected = model.selected?.selector == device.selector && model.session != 0L
                        val offline = model.isOffline(device)
                        Row(Modifier.fillMaxWidth().background(if (offline) MaterialTheme.colorScheme.surfaceVariant.copy(alpha = .5f) else Color.Transparent).clickable { openDevice(device) }.padding(horizontal = 16.dp, vertical = 10.dp),
                            verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                            DeviceAvatar(model, device, 44.dp, if (offline) LensStyle.offline else LensStyle.green)
                            Column(Modifier.weight(1f)) {
                                Text(device.title, style = MaterialTheme.typography.titleMedium, maxLines = 2, color = if (offline) MaterialTheme.colorScheme.onSurfaceVariant else MaterialTheme.colorScheme.onSurface)
                                SecondaryText(if (connected) "Connected" else if (offline) "Offline" else if (device.lite) "Wendy Lite" else "")
                            }
                            when {
                                model.connecting && model.selected?.selector == device.selector -> CircularProgressIndicator(Modifier.size(18.dp), strokeWidth = 2.dp)
                                connected -> Icon(Icons.Outlined.CheckCircle, "Connected", tint = LensStyle.online)
                                else -> Icon(Icons.Outlined.ChevronRight, null, tint = MaterialTheme.colorScheme.outline)
                            }
                        }
                    }
                    when {
                        error != null -> Column(Modifier.padding(16.dp)) { FailureNote(error); TextButton(onClick = { scope.launch { refresh() } }) { Text("Try again") } }
                        matching.isEmpty() && loading -> LensLoading("Loading devices…")
                        matching.isEmpty() && search.isNotEmpty() -> Text("No results for “$search”", Modifier.padding(16.dp), color = MaterialTheme.colorScheme.onSurfaceVariant)
                        matching.isEmpty() -> EmptyMessage(if (source == InventorySource.Local) "No nearby devices" else "No cloud devices",
                            if (source == InventorySource.Local) "Connect to the device’s network. Wendy finds devices on the same Wi-Fi or Ethernet network." else "Devices enrolled in this organization will appear here.",
                            if (source == InventorySource.Local) Icons.Outlined.SettingsInputAntenna else Icons.Outlined.Public)
                    }
                }
            }
        }
    }
}

private enum class DeviceSegment(val title: String) { Apps("Apps"), Cameras("Cameras"), Data("Data"), Location("Location"), Settings("Settings") }

/** A device: Apps, Cameras, Data, Location and Settings, with the More device tools menu (iOS MobileDeviceView). */
@Composable
fun DeviceLensPage(model: WendyModel, device: Device, openRecordings: () -> Unit = {}) {
    val nav = LocalNavigator.current
    val ready = model.selected?.selector == device.selector && model.session != 0L
    var segment by rememberSaveableString(DeviceSegment.Apps.name)
    LaunchedEffect(device.selector) { if (model.selected?.selector != device.selector || model.session == 0L) model.connect(device) }
    if (!ready) { DeviceConnectionScreen(model); return }
    if (model.lite) { LiteDeviceScreen(model); return }
    Column(Modifier.fillMaxSize()) {
        LensSegments(DeviceSegment.entries, DeviceSegment.valueOf(segment), { it.title }, Modifier.padding(horizontal = 16.dp, vertical = 8.dp)) { segment = it.name }
        Box(Modifier.weight(1f)) {
            when (DeviceSegment.valueOf(segment)) {
                DeviceSegment.Apps -> DeviceAppsList(model, device)
                DeviceSegment.Cameras -> CamerasScreen(model)
                DeviceSegment.Settings -> DeviceSettingsScreen(model)
                DeviceSegment.Data -> Column(Modifier.verticalScroll(rememberScrollState()).padding(16.dp), verticalArrangement = Arrangement.spacedBy(18.dp)) {
                    RecentRecordings(model, {})
                    LensSection(null) { LensRow("All recordings", "", Icons.Outlined.Storage, onClick = openRecordings) }
                }
                DeviceSegment.Location -> Column(Modifier.verticalScroll(rememberScrollState()).padding(16.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
                    val site = model.presence?.firstOrNull { it.selector == device.selector }?.site?.ifEmpty { null }
                    LensSection("Location") {
                        LensRow("Home place", site ?: "Unassigned", Icons.Outlined.Business, navigates = false)
                        LensDivider()
                        LensRow("Current place", "Not reported by this device", Icons.Outlined.Place, Color(0xFFFF2D55), navigates = false)
                    }
                    Text(if (device.selector.startsWith("cloud://")) "Remote: reached through Wendy Cloud." else "Nearby: this phone can reach the device directly.",
                        Modifier.padding(horizontal = 12.dp), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                }
            }
        }
        AskWendyBar(model) { model.requestChat = true }
    }
}

/** The device page toolbar: campaign events and More device tools (iOS MobileDeviceView toolbar). */
@Composable
fun DeviceLensActions(model: WendyModel, device: Device) {
    val nav = LocalNavigator.current
    val ready = model.selected?.selector == device.selector && model.session != 0L && !model.lite
    var update by remember(model.session) { mutableStateOf<UpdateSnapshot?>(null) }
    LaunchedEffect(model.session, ready) {
        if (!ready) return@LaunchedEffect
        val captured = model.session
        while (captured == model.session) {
            update = try { UpdateSnapshot.parse(model.backend.call("OnlineUpdate", captured, 0, "").obj()) } catch (e: CancellationException) { throw e } catch (_: Exception) { null }
            delay(if (update?.busy == true) 5_000 else 30_000)
        }
    }
    val updateTitle = update?.let { u ->
        if (u.busy) { if (u.phase == "downloading" && u.total > 0) "Downloading ${u.downloaded * 100 / u.total}%" else if (u.phase == "installing") "Installing…" else "Reconnecting…" }
        else if (u.available) "Update" else "Device info"
    } ?: "Device info"
    if (ready) CampaignEventButton(model)
    else if (model.selected?.selector == device.selector && model.session != 0L && model.lite) IconButton(onClick = { model.logApp = ""; nav.push("Logs") { DeviceLogsPage(model) } }) {
        Icon(Icons.AutoMirrored.Outlined.Article, "Logs")
    }
    OverflowMenu("More device tools", buildList {
        add(MenuItem("Customize appearance", Icons.Outlined.Palette) { nav.push("Appearance") { DeviceAppearanceEditor(model, device) } })
        if (ready) {
            add(MenuItem("Logs", Icons.AutoMirrored.Outlined.Article) { model.logApp = ""; nav.push("Logs") { DeviceLogsPage(model) } })
            add(MenuItem(updateTitle, Icons.Outlined.Info) { nav.push("Device info") { DeviceInfoPage(model, device.title) } })
            add(MenuItem("App activity", Icons.Outlined.MonitorHeart) { nav.push("App activity") { AppsScreen(model) } })
            add(MenuItem("Digital Twin", Icons.Outlined.ViewInAr) { nav.push("Digital Twin") { DigitalTwinScreen(model) } })
            add(MenuItem("Campaigns", Icons.Outlined.FiberManualRecord) { nav.push("Campaigns") { CampaignsScreen(model) } })
            add(MenuItem("Device management", Icons.Outlined.Settings) { nav.push("Device management") { DeviceManagementPage(model, device) } })
        }
    }, icon = Icons.Outlined.MoreHoriz)
}

@Composable
private fun DeviceAppsList(model: WendyModel, device: Device) {
    val nav = LocalNavigator.current
    var deploying by remember { mutableStateOf(false) }
    val scope = rememberCoroutineScope()
    LaunchedEffect(model.session) { model.refreshDetails() }
    LazyColumn(Modifier.fillMaxSize(), contentPadding = PaddingValues(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
        item {
            Row(Modifier.fillMaxWidth().padding(start = 16.dp), verticalAlignment = Alignment.CenterVertically) {
                Text("APPS", Modifier.weight(1f), style = MaterialTheme.typography.labelMedium, fontWeight = FontWeight.SemiBold, color = MaterialTheme.colorScheme.onSurfaceVariant)
                TextButton(onClick = { deploying = true }) { Icon(Icons.Outlined.Add, null, Modifier.size(16.dp)); Text("Add") }
            }
        }
        item {
            LensSection(null) {
                model.visibleApps.forEachIndexed { index, app ->
                    if (index > 0) LensDivider()
                    Row(Modifier.fillMaxWidth().clickable { nav.push(app.name, actions = { if (model.selected?.selector == device.selector && model.session != 0L) CampaignEventButton(model) }) { AppLensPage(model, device, app.name) } }.padding(horizontal = 16.dp, vertical = 10.dp),
                        verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                        LensIcon(Icons.Outlined.GridView, if (app.state == AppState.Running) LensStyle.online else LensStyle.offline)
                        Column(Modifier.weight(1f)) {
                            Text(app.name, style = MaterialTheme.typography.bodyLarge, fontWeight = FontWeight.SemiBold, maxLines = 2)
                            if (app.terminationReason.isNotEmpty() && !app.terminationReason.equals(app.state.title, true)) SecondaryText(app.terminationReason)
                        }
                        AppStateBadge(app.state)
                        if (app.hasWebpage) Icon(Icons.Outlined.Public, "Has a web page", Modifier.size(16.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                }
                val error = model.appsError
                when {
                    error != null -> Box(Modifier.padding(16.dp)) { FailureNote(error) }
                    model.visibleApps.isEmpty() && model.detailsSession != model.session -> LoadingPlaceholder("Loading apps…", modifier = Modifier.padding(16.dp))
                    model.visibleApps.isEmpty() -> Text("No visible apps on this device.", Modifier.padding(16.dp), color = MaterialTheme.colorScheme.onSurfaceVariant)
                }
                if (model.hiddenAppCount > 0) Box(Modifier.padding(horizontal = 12.dp)) { ToggleRow("Show hidden apps", model.showHiddenApps, subtitle = "${model.hiddenAppCount} hidden apps") { model.showHiddenApps = it } }
            }
        }
        item { Spacer(Modifier.height(8.dp)) }
    }
    if (deploying) AlertDialog(onDismissRequest = { deploying = false }, title = { Text("Deploy an app") }, text = {
        Column(verticalArrangement = Arrangement.spacedBy(12.dp)) {
            Text("Run this command in your project on your developer machine.")
            SelectionContainer { Text("wendy run --device '${device.selector.replace("'", "'\\''")}'", fontFamily = FontFamily.Monospace, style = MaterialTheme.typography.bodyMedium) }
        }
    }, confirmButton = { TextButton(onClick = { deploying = false }) { Text("Done") } })
}

@Composable
private fun AppStateBadge(state: AppState) {
    val color = when (state) { AppState.Running -> LensStyle.online; AppState.CrashLooping -> LensStyle.danger; else -> LensStyle.offline }
    Text(state.title.uppercase(), Modifier.background(color.copy(alpha = .12f), CircleShape).padding(horizontal = 7.dp, vertical = 4.dp),
        style = MaterialTheme.typography.labelSmall, fontWeight = FontWeight.Bold, color = color, maxLines = 1)
}

/** An app: hero with start or stop and open, ports, logs, version and restart (iOS MobileAppDetail). */
@Composable
fun AppLensPage(model: WendyModel, device: Device, name: String) {
    val nav = LocalNavigator.current
    val ready = model.selected?.selector == device.selector && model.session != 0L
    val app = if (ready) model.apps.firstOrNull { it.name == name } else null
    var ports by remember { mutableStateOf(emptyList<JSONObject>()) }
    var portError by remember { mutableStateOf<String?>(null) }
    LaunchedEffect(model.session, app?.state) {
        ports = emptyList(); portError = null
        if (app?.state != AppState.Running) return@LaunchedEffect
        try { ports = model.backend.call("Ports", name, model.session).items() } catch (e: CancellationException) { throw e } catch (e: Exception) { portError = e.message }
    }
    if (app == null) { EmptyMessage(if (ready) "App unavailable" else "Device disconnected", "", Icons.Outlined.GridView); return }
    fun openLogs() { model.logApp = name; nav.push("Logs") { DeviceLogsPage(model) } }
    val busy = name in model.appActions
    Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(16.dp), verticalArrangement = Arrangement.spacedBy(18.dp)) {
        LensHero(Icons.Outlined.GridView, if (app.state == AppState.Running) LensStyle.online else LensStyle.offline, "APP · ${device.title.uppercase()}", name,
            app.terminationReason.ifEmpty { app.state.title }) {
            LensButton(if (app.state == AppState.Running) "Stop" else "Start", enabled = !busy && app.state != AppState.Unknown, modifier = Modifier.weight(1f)) { model.setAppRunning(name, app.state != AppState.Running) }
            if (app.hasWebpage) LensButton("Open", primary = true, enabled = app.state == AppState.Running && !busy, modifier = Modifier.weight(1f)) { model.openApp(name) }
            else LensButton("Logs", primary = true, modifier = Modifier.weight(1f)) { openLogs() }
        }
        if (busy) LensLoading("Updating app…")
        if (app.hasWebpage || ports.isNotEmpty() || portError != null) LensSection("Ports") {
            if (app.hasWebpage) LensRow("Dashboard", endpoint(device, app), Icons.Outlined.Public, trailing = ":${app.httpPort}", enabled = app.state == AppState.Running) { model.openApp(name) }
            ports.filter { it.number("port") != app.httpPort }.forEach { port ->
                LensDivider()
                val address = port.text("address").let { if (':' in it) "[$it]" else it }
                LensRow(port.text("protocol").uppercase(), "$address:${port.number("port")}", Icons.Outlined.Lan, navigates = false)
            }
            portError?.let { Box(Modifier.padding(12.dp)) { FailureNote(it) } }
        }
        LensSection("App") {
            LensRow("Logs", "Follow live output", Icons.Outlined.Schedule, LensStyle.offline) { openLogs() }
            LensDivider()
            LensRow("Version", app.version.ifEmpty { "Not reported" }, Icons.Outlined.GridView, LensStyle.offline, navigates = false)
            LensDivider()
            LensRow("Restart", "Stops and starts the app", Icons.Outlined.Refresh, LensStyle.warning, enabled = app.state == AppState.Running && !busy) { model.restartApp(name) }
        }
    }
}

private fun endpoint(device: Device, app: DeviceApp): String = when {
    device.selector.startsWith("cloud://") -> "Wendy Cloud tunnel"
    device.selector.startsWith("mesh:") -> "Wendy mesh tunnel"
    else -> device.selector.substringBefore(':').takeIf { it.isNotEmpty() }?.let { "http://$it:${app.httpPort}" } ?: "Published HTTP endpoint"
}

/** A lens still being designed: four tabs, each under construction, with Ask Wendy above them (iOS unfinishedLensTabs). */
@Composable
fun UnfinishedLensTabs(lens: Lens, askWendy: () -> Unit) {
    var tab by androidx.compose.runtime.saveable.rememberSaveable { mutableIntStateOf(0) }
    val tabs = listOf(Triple("Home", Icons.Outlined.Home, lens.title), Triple(lens.title, lens.icon, lens.title),
        Triple("Data", Icons.Outlined.Storage, lens.title + " Data"), Triple("Activity", Icons.Outlined.Schedule, lens.title + " Activity"))
    Column(Modifier.fillMaxSize()) {
        Box(Modifier.weight(1f)) { UnderConstructionLens(tabs[tab].third) }
        AskWendyBar(LocalWendyModel.current ?: return@Column, askWendy)
        NavigationBar(containerColor = MaterialTheme.colorScheme.surface) {
            tabs.forEachIndexed { index, (title, icon, _) ->
                NavigationBarItem(selected = tab == index, onClick = { tab = index }, label = { Text(title) }, icon = { Icon(icon, null) },
                    colors = NavigationBarItemDefaults.colors(selectedIconColor = LensStyle.accent, selectedTextColor = LensStyle.accent, indicatorColor = LensStyle.green.copy(alpha = .12f)))
            }
        }
    }
}

/** Lenses still being designed show the iOS under-construction state. */
@Composable
fun UnderConstructionLens(title: String) {
    Column(Modifier.fillMaxSize().padding(24.dp), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(16.dp, Alignment.CenterVertically)) {
        RobotForge(Modifier.fillMaxWidth(.9f).aspectRatio(320f / 260f))
        Text("Under construction", style = MaterialTheme.typography.headlineSmall, fontWeight = FontWeight.Bold)
        Text("We’re building the $title lens.", color = MaterialTheme.colorScheme.onSurfaceVariant, textAlign = TextAlign.Center)
    }
}

/** A robot hammering an anvil, as in the iOS lens placeholder. */
@Composable
private fun RobotForge(modifier: Modifier) {
    val transition = rememberInfiniteTransition(label = "forge")
    val cycle by transition.animateFloat(0f, 1f, infiniteRepeatable(tween(1600, easing = LinearEasing)), label = "strike")
    val dark = Color(0xFF1F292B)
    Canvas(modifier) {
        val s = size.width / 320f
        fun r(x: Float, y: Float, w: Float, h: Float, rad: Float, c: Color) = drawRoundRect(c, Offset(x * s, y * s), Size(w * s, h * s), CornerRadius(rad * s))
        drawOval(LensStyle.green.copy(alpha = .09f), Offset(25 * s, 223 * s), Size(272 * s, 18 * s))
        r(68f, 182f, 18f, 40f, 8f, dark); r(109f, 182f, 18f, 40f, 8f, dark)
        r(57f, 212f, 36f, 17f, 7f, LensStyle.green); r(104f, 212f, 36f, 17f, 7f, LensStyle.green)
        r(57f, 113f, 83f, 78f, 22f, LensStyle.green); r(74f, 130f, 48f, 40f, 12f, Color.White.copy(alpha = .18f)); r(88f, 142f, 21f, 16f, 4f, Color.White.copy(alpha = .85f))
        drawLine(dark, Offset(98 * s, 49 * s), Offset(98 * s, 35 * s), 4 * s); drawCircle(LensStyle.green, 6 * s, Offset(98 * s, 33 * s))
        r(54f, 50f, 88f, 66f, 22f, LensStyle.green); r(65f, 64f, 66f, 36f, 13f, dark); r(77f, 76f, 9f, 12f, 4f, Color.White); r(109f, 76f, 9f, 12f, 4f, Color.White)
        r(198f, 189f, 64f, 38f, 8f, Color(0xFF9C754F))
        val anvil = androidx.compose.ui.graphics.Path().apply {
            moveTo(174 * s, 151 * s); lineTo(288 * s, 151 * s); lineTo(274 * s, 169 * s); lineTo(246 * s, 173 * s); lineTo(241 * s, 183 * s); lineTo(262 * s, 190 * s)
            lineTo(190 * s, 190 * s); lineTo(208 * s, 180 * s); lineTo(205 * s, 172 * s); close()
        }
        drawPath(anvil, dark)
        val lift = (1 - kotlin.math.cos(cycle * Math.PI * 2).toFloat()) / 2
        rotate(-58f * lift, Offset(136 * s, 128 * s)) {
            drawLine(dark, Offset(136 * s, 128 * s), Offset(213 * s, 132 * s), 12 * s, androidx.compose.ui.graphics.StrokeCap.Round)
            drawCircle(LensStyle.green, 10 * s, Offset(171 * s, 132 * s))
            r(201f, 116f, 34f, 30f, 6f, Color.Gray)
        }
        if (cycle < 0.12f) { val glow = 1 - cycle / 0.12f; drawLine(LensStyle.warning.copy(alpha = glow), Offset(236 * s, 139 * s), Offset(247 * s, 126 * s), 3 * s); drawLine(LensStyle.warning.copy(alpha = glow), Offset(246 * s, 147 * s), Offset(262 * s, 143 * s), 3 * s) }
    }
}
