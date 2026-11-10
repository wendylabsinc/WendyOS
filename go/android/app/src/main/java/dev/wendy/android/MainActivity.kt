package dev.wendy.android

import android.content.Intent
import android.net.Uri
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.BackHandler
import androidx.activity.compose.setContent
import androidx.activity.SystemBarStyle
import androidx.activity.enableEdgeToEdge
import androidx.compose.foundation.*
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.material.icons.automirrored.outlined.ArrowBack
import androidx.compose.material3.*
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.semantics.*
import androidx.lifecycle.ViewModelProvider
import kotlinx.coroutines.launch

class MainActivity : ComponentActivity() {
    private lateinit var model: WendyModel
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        model = ViewModelProvider(this)[WendyModel::class.java]
        setContent {
            val dark = when (model.appearance) { "Dark" -> true; "Light" -> false; else -> androidx.compose.foundation.isSystemInDarkTheme() }
            // Status and navigation bar icons follow Wendy's appearance, not only the system's.
            LaunchedEffect(dark) {
                val style = if (dark) SystemBarStyle.dark(android.graphics.Color.TRANSPARENT) else SystemBarStyle.light(android.graphics.Color.TRANSPARENT, android.graphics.Color.TRANSPARENT)
                enableEdgeToEdge(style, style)
            }
            WendyTheme(model.appearance) {
                WendyApp(model, intent?.action == WendyVpnService.OPEN) { url ->
                    try {
                        val uri = Uri.parse(url)
                        require(uri.scheme in setOf("https", "http")) { "Unsupported link" }
                        if (uri.host in setOf("127.0.0.1", "localhost")) model.appURL = url else startActivity(Intent(Intent.ACTION_VIEW, uri))
                    } catch (e: Exception) { model.error = e.message }
                }
            }
        }
    }
    override fun onStart() { super.onStart(); model.setForeground(true) }
    override fun onStop() { model.setForeground(false); super.onStop() }
}

/**
 * Routes of the lens app (iOS MobileRootView): General Home is the root, a lens is the
 * first pushed page, and device pages sit above it.
 */
@Stable
private class RootRoutes(val model: WendyModel, val nav: Navigator) {
    var devicesTab by mutableStateOf(DevicesTab.Home)
    var activitySegment by mutableStateOf(ActivitySegment.Alerts)
    var chat by mutableStateOf(false)
    var address by mutableStateOf(false)
    private var lensPage: Pair<Long, Lens>? = null
    private fun currentLens() = lensPage?.takeIf { (key, _) -> nav.stack.firstOrNull()?.key == key }?.second

    fun enter(lens: Lens) {
        if (currentLens() == lens) { while (nav.stack.size > 1) nav.pop(); return }
        nav.clear(); devicesTab = DevicesTab.Home
        when (lens) {
            Lens.Devices -> nav.push("Devices", actions = { if (devicesTab == DevicesTab.Home) DevicesHomeActions() }) {
                DevicesLens(model, devicesTab, { devicesTab = it }, { openSetup() }, ::openActivity, { chat = true }, ::select, activitySegment, { activitySegment = it })
            }
            Lens.Sites -> nav.push("Sites") { LensWithChat { SitesScreen(model) } }
            Lens.Robots -> nav.push("Robots") { CompositionLocalProvider(LocalWendyModel provides model) { UnfinishedLensTabs(Lens.Robots) { chat = true } } }
        }
        lensPage = nav.stack.first().key to lens
    }
    fun select(device: Device) {
        enter(Lens.Devices); devicesTab = DevicesTab.Home
        nav.pushScoped(device.title, actions = { DeviceLensActions(model, device) }) { DeviceLensPage(model, device) { openRecordings(device) } }
    }
    fun openActivity() { activitySegment = ActivitySegment.Alerts; nav.push("Activity") { DeviceActivityScreen(model, activitySegment, { activitySegment = it }, ::select) } }
    fun openRecordings() { enter(Lens.Devices); devicesTab = DevicesTab.Data }
    /** The Data tab with this device's episodes open (iOS openRecordings(for:)). */
    fun openRecordings(device: Device) {
        openRecordings()
        nav.push(device.title) { EpisodePickerPage(model, recordingDeviceID(device), device, null, null) }
    }
    fun openSetup(device: SetupDevice? = null) = nav.push("Device setup") { NearbySetupScreen(model, device) { nav.pop(); model.task { model.refresh() } } }
    fun openVpn() = nav.push("Private network") { VpnScreen(model) { nav.pop() } }

    @Composable private fun DevicesHomeActions() {
        IconButton(onClick = ::openActivity) { Icon(Icons.Outlined.Notifications, "Notifications") }
        OverflowMenu("Connect to a device", listOf(
            MenuItem("Set up a device nearby", Icons.Outlined.Bluetooth) { openSetup() },
            MenuItem("Connect by address", Icons.Outlined.Link) { address = true },
        ), icon = Icons.Outlined.Add, enabled = model.initialized)
    }
    @Composable private fun LensWithChat(content: @Composable () -> Unit) {
        Column(Modifier.fillMaxSize()) { Box(Modifier.weight(1f)) { content() }; AskWendyBar(model) { chat = true } }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun WendyApp(model: WendyModel, openVpn: Boolean = false, openURL: (String) -> Unit) {
    val context = LocalContext.current
    val nav = remember { Navigator() }
    val routes = remember { RootRoutes(model, nav) }
    val widgets = remember { HomeWidgetStore(context.applicationContext) }
    // Recordings and thumbnails belong to one account and organization (iOS MobileRecordingScope).
    val catalog = remember(model.account, model.organization) { RecordingCatalog(model) }
    val posters = remember(model.account, model.organization) { RecordingPosters(model) }
    var settings by remember { mutableStateOf(false) }
    var editingHome by remember { mutableStateOf(false) }
    var errorDetails by remember { mutableStateOf<String?>(null) }
    LaunchedEffect(Unit) { if (openVpn) routes.openVpn() }
    LaunchedEffect(model.browserURL) { model.browserURL?.let { model.browserURL = null; openURL(it) } }
    LaunchedEffect(model.linkRequest) { model.linkRequest?.let { model.linkRequest = null; openURL(it) } }
    LaunchedEffect(model.requestNearbySetup) { if (model.requestNearbySetup) { model.requestNearbySetup = false; routes.openSetup() } }
    LaunchedEffect(model.requestSettings) { if (model.requestSettings) { model.requestSettings = false; settings = true } }
    LaunchedEffect(model.requestChat) { if (model.requestChat) { model.requestChat = false; routes.chat = true } }
    // Device pages belong to one account and organization (iOS drops device routes when it changes).
    LaunchedEffect(model.account, model.organization) { nav.dropScoped() }
    val top = nav.top
    BackHandler(top != null) { nav.back() }
    BackHandler(top == null && editingHome) { editingHome = false }
    CompositionLocalProvider(LocalNavigator provides nav, LocalRecordingCatalog provides catalog, LocalRecordingPosters provides posters) {
        if (model.initialized && model.primaryLens == null) LensPreferenceScreen { model.choosePrimaryLens(it) }
        else Scaffold(
            containerColor = MaterialTheme.colorScheme.background,
            topBar = {
                Row(Modifier.fillMaxWidth().windowInsetsPadding(WindowInsets.statusBars).padding(horizontal = 12.dp, vertical = 8.dp).heightIn(min = 48.dp),
                    verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(4.dp)) {
                    when {
                        top != null -> {
                            IconButton(onClick = nav::back) { Icon(Icons.AutoMirrored.Outlined.ArrowBack, "Back") }
                            Text(top.title, Modifier.weight(1f), style = MaterialTheme.typography.titleMedium, maxLines = 1, overflow = TextOverflow.Ellipsis)
                            top.actions?.invoke(this)
                        }
                        editingHome -> {
                            TextButton(onClick = { nav.push("Add widget") { HomeWidgetManager(model, widgets) } }) { Icon(Icons.Outlined.Add, null, Modifier.size(18.dp)); Spacer(Modifier.width(6.dp)); Text("Add widget") }
                            Spacer(Modifier.weight(1f))
                            TextButton(onClick = { editingHome = false }) { Text("Done", fontWeight = FontWeight.SemiBold) }
                        }
                        else -> {
                            val organization = model.organizations.firstOrNull { it.text("id") == model.organization }?.text("name")?.ifEmpty { null }
                            Surface(onClick = { settings = true }, enabled = model.initialized, shape = CircleShape, color = MaterialTheme.colorScheme.surface,
                                modifier = Modifier.widthIn(max = 260.dp).semantics { contentDescription = "Settings, ${organization ?: "Account"}" }) {
                                Row(Modifier.padding(start = 6.dp, end = 12.dp, top = 6.dp, bottom = 6.dp), horizontalArrangement = Arrangement.spacedBy(7.dp), verticalAlignment = Alignment.CenterVertically) {
                                    Box(Modifier.size(26.dp).background(LensStyle.green, CircleShape), contentAlignment = Alignment.Center) { WendyMark(Modifier.size(16.dp, 12.dp), Color.White) }
                                    Text(organization ?: "Account", style = MaterialTheme.typography.titleSmall, maxLines = 1, overflow = TextOverflow.Ellipsis)
                                    Icon(Icons.Outlined.ExpandMore, null, Modifier.size(16.dp))
                                }
                            }
                            Spacer(Modifier.weight(1f))
                            IconButton(onClick = routes::openActivity, enabled = model.initialized) { Icon(Icons.Outlined.Notifications, "Notifications") }
                        }
                    }
                }
            },
            floatingActionButton = {
                if (top == null && !editingHome && model.initialized) FloatingActionButton(onClick = { routes.chat = true }, shape = CircleShape, containerColor = LensStyle.green, contentColor = Color.White) {
                    Icon(Icons.Outlined.AutoAwesome, "Ask Wendy")
                }
            }
        ) { padding ->
            Column(Modifier.padding(padding).fillMaxSize()) {
                if (model.busy > 0 || !model.initialized) LinearProgressIndicator(Modifier.fillMaxWidth().height(2.dp))
                Box(Modifier.weight(1f)) {
                    // Pushed pages cover the screen; the pages underneath keep their state.
                    NavigationHost(nav) {
                        if (!model.initialized) Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) { EmptyMessage("Getting ready", "Your devices will be here in a moment.") }
                        else GeneralHomeScreen(model, widgets, editingHome, { editingHome = it }, routes::enter, { routes.openSetup() }, routes::openActivity, routes::openRecordings, routes::select)
                    }
                }
            }
        }
    }
    CompositionLocalProvider(LocalNavigator provides nav, LocalRecordingCatalog provides catalog, LocalRecordingPosters provides posters) {
        model.appURL?.let { url -> AppBrowser(url, { model.appURL = null }, openURL) }
        NearbySetupPrompt(model, active = model.initialized && model.inForeground && nav.stack.none { it.title == "Device setup" } && !settings && !routes.chat && !routes.address && model.appDeletion == null && model.appURL == null) { device ->
            routes.openSetup(device)
        }
        if (settings) SettingsSheet(model) { settings = false }
        if (routes.chat) AskWendySheet(model, { routes.chat = false }) { routes.chat = false; routes.enter(Lens.Devices) }
        if (routes.address) InputDialog("Connect to device", listOf(Field("address", "Hostname or IP address", "", "hostname:50051")), onDismiss = { routes.address = false }, submitLabel = "Connect") { values ->
            val host = values.getValue("address").trim()
            if (host.isNotBlank()) { routes.address = false; routes.select(Device(host, host, host, "LAN")) }
        }
        model.error?.let { message ->
            val failure = PresentedFailure(message)
            AlertDialog(onDismissRequest = { model.error = null }, icon = { Icon(Icons.Outlined.ErrorOutline, null) }, title = { Text(failure.title ?: "Could not complete request") },
                text = { Text(failure.message) },
                confirmButton = { TextButton(onClick = { model.error = null }) { Text("OK") } },
                dismissButton = { TextButton(onClick = { errorDetails = failure.details; model.error = null }) { Text("Details") } })
        }
        errorDetails?.let { DetailsDialog("Details", it) { errorDetails = null } }
        model.appDeletion?.let { app ->
            ConfirmAction("Delete app?", "$app will be removed from the device.", "Delete $app", onDismiss = { model.appDeletion = null }) { model.appDeletion = null; model.deleteApp(app) }
        }
    }
}

/**
 * Ask Wendy (iOS MobileLensChat): the connected device's chat, or a remembered device with verified
 * models. Choosing one connects and checks its models again before chatting.
 */
@Composable
private fun AskWendySheet(model: WendyModel, dismiss: () -> Unit, chooseDevice: () -> Unit) {
    val remembered = rememberAIDevices()
    var suggestions by remember { mutableStateOf(false) }
    var selection by remember { mutableStateOf<Pair<RememberedAIDevice, Device>?>(null) }
    var selectionError by remember { mutableStateOf<String?>(null) }
    LaunchedEffect(model.account, model.organization) { selection = null; selectionError = null; suggestions = false }
    LaunchedEffect(model.inForeground) { if (!model.inForeground) selection = null }
    LaunchedEffect(selection) {
        val (record, device) = selection ?: return@LaunchedEffect
        val account = model.account; val organization = model.organization
        val ready = model.connectAndWait(device)
        if (selection?.first?.id != record.id || account != model.account || organization != model.organization || !model.inForeground) return@LaunchedEffect
        if (!ready || model.selected?.let(::recordingDeviceID) != record.id) {
            selectionError = model.detailError?.let { PresentedFailure(it).message } ?: "Could not connect. Try another device."
            suggestions = true; selection = null; return@LaunchedEffect
        }
        val chat = model.deviceChat ?: run { selection = null; return@LaunchedEffect }
        chat.verifySuggestedModel(record.preferred?.id, record.preferred?.port)
        if (selection?.first?.id == record.id && model.session == chat.session) { suggestions = false; selection = null }
    }
    val chat = model.deviceChat
    DisposableEffect(chat) { chat?.setPresented(true); onDispose { chat?.setPresented(false) } }
    NavigationSheet("Ask Wendy", dismiss) {
        when {
            selection != null -> Column(Modifier.fillMaxSize(), verticalArrangement = Arrangement.spacedBy(16.dp, Alignment.CenterVertically), horizontalAlignment = Alignment.CenterHorizontally) {
                LensLoading("Checking device and models…"); TextButton(onClick = { selection = null }) { Text("Cancel") }
            }
            !suggestions && model.session != 0L && !model.lite && chat != null -> key(model.session) { ChatScreen(model) { suggestions = true } }
            else -> Column(Modifier.fillMaxSize()) {
                selectionError?.let { SecondaryText(it, Modifier.padding(horizontal = 20.dp)) }
                Box(Modifier.weight(1f)) {
                    AIDeviceSuggestions(model, remembered, if (suggestions) model.selected?.let(::recordingDeviceID) else null, chooseDevice) { record, device -> selectionError = null; selection = record to device }
                }
                if (suggestions && model.session != 0L) TextButton(onClick = { suggestions = false }, Modifier.align(Alignment.CenterHorizontally).padding(bottom = 12.dp)) { Text("Back to chat") }
            }
        }
    }
}

@Composable private fun rememberSaveableState(initial: String) = androidx.compose.runtime.saveable.rememberSaveable { mutableStateOf(initial) }

@Composable fun PageTitle(title: String, subtitle: String = "") {
    Column(Modifier.padding(bottom = 6.dp), verticalArrangement = Arrangement.spacedBy(7.dp)) {
        Text(title, style = MaterialTheme.typography.headlineLarge)
        if (subtitle.isNotEmpty()) Text(subtitle, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}
@Composable fun EmptyMessage(title: String, message: String, icon: androidx.compose.ui.graphics.vector.ImageVector = Icons.Outlined.Sensors) {
    Column(Modifier.fillMaxWidth().padding(horizontal = 24.dp, vertical = 32.dp), verticalArrangement = Arrangement.spacedBy(12.dp), horizontalAlignment = Alignment.CenterHorizontally) {
        IconTile(icon, Modifier.size(56.dp))
        Text(title, style = MaterialTheme.typography.titleLarge, textAlign = TextAlign.Center)
        Text(message, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant, textAlign = TextAlign.Center)
    }
}
