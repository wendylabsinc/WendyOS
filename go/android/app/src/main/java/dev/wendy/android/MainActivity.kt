package dev.wendy.android

import android.content.Intent
import android.net.Uri
import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.BackHandler
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.compose.foundation.*
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
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
            WendyTheme {
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

private val devicePanes = listOf("Overview", "Cameras", "Apps")
private val moreDevicePanes = listOf("Digital Twin", "Campaigns", "Chat")

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun WendyApp(model: WendyModel, openVpn: Boolean = false, openURL: (String) -> Unit) {
    val nav = remember { Navigator() }
    var tab by rememberSaveableState("Devices")
    var accounts by remember { mutableStateOf(false) }
    var address by remember { mutableStateOf(false) }
    var addDevice by remember { mutableStateOf(false) }
    var setup by rememberSaveableState(if (openVpn) "vpn" else "")
    var pane by rememberSaveableState("Overview")
    var errorDetails by remember { mutableStateOf<String?>(null) }
    LaunchedEffect(model.browserURL) { model.browserURL?.let { model.browserURL = null; openURL(it) } }
    LaunchedEffect(model.linkRequest) { model.linkRequest?.let { model.linkRequest = null; openURL(it) } }
    LaunchedEffect(model.requestNearbySetup) { if (model.requestNearbySetup) { model.requestNearbySetup = false; nav.clear(); setup = "setup" } }
    // A different device or session invalidates pages pushed for the previous one.
    LaunchedEffect(model.selected?.selector, model.session) { nav.clear() }
    LaunchedEffect(model.selected?.selector) { pane = "Overview" }
    val top = nav.top
    fun back() { when { top != null -> nav.pop(); setup.isNotBlank() -> setup = ""; else -> model.disconnect() } }
    BackHandler(top != null || model.selected != null || setup.isNotBlank()) { back() }
    CompositionLocalProvider(LocalNavigator provides nav) {
        Scaffold(
            containerColor = MaterialTheme.colorScheme.background,
            topBar = {
                Row(Modifier.fillMaxWidth().windowInsetsPadding(WindowInsets.statusBars).padding(horizontal = 20.dp, vertical = 12.dp),
                    verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                    if (top != null || model.selected != null || setup.isNotBlank()) {
                        FilledTonalIconButton(onClick = ::back, colors = IconButtonDefaults.filledTonalIconButtonColors(containerColor = MaterialTheme.colorScheme.surface)) {
                            Icon(Icons.AutoMirrored.Outlined.ArrowBack, if (top != null) "Back" else "Back to devices", Modifier.size(22.dp))
                        }
                        Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(2.dp)) {
                            val title = top?.title ?: model.selected?.title ?: if (setup == "setup") "Device setup" else "Private network"
                            Text(title, style = MaterialTheme.typography.titleMedium, maxLines = 1, overflow = TextOverflow.Ellipsis)
                            val subtitle = when {
                                top != null -> model.selected?.title ?: ""
                                model.selected == null -> "Wendy"
                                model.session == 0L -> if (model.detailError != null) "Not connected" else "Connecting"
                                else -> "Connected via ${model.selected?.transport}"
                            }
                            if (subtitle.isNotEmpty()) Text(subtitle, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1)
                        }
                        when {
                            top?.actions != null -> top.actions.invoke(this)
                            top == null && model.selected != null && setup.isBlank() -> {
                                val ready = model.session != 0L && !model.lite
                                if (ready) CampaignEventButton(model)
                                IconButton(onClick = { nav.push("Settings") { DeviceSettingsScreen(model) } }, enabled = ready) { Icon(Icons.Outlined.Settings, "Device settings") }
                            }
                        }
                    } else {
                        WendyMark(Modifier.size(34.dp, 28.dp))
                        Text("wendy", style = MaterialTheme.typography.headlineMedium, fontWeight = FontWeight.Bold)
                        Spacer(Modifier.weight(1f))
                        IconButton(onClick = { addDevice = true }, enabled = model.initialized) { Icon(Icons.Outlined.Add, "Add a device") }
                    }
                    if (top == null && model.selected == null) FilledTonalIconButton(onClick = { accounts = true }, enabled = model.initialized,
                        colors = IconButtonDefaults.filledTonalIconButtonColors(containerColor = MaterialTheme.colorScheme.secondaryContainer)) {
                        val email = model.accounts.firstOrNull { it.text("Context") == model.account }?.text("Email") ?: ""
                        if (email.isBlank()) Icon(Icons.Outlined.PersonOutline, "Accounts", Modifier.size(22.dp))
                        else Text(email.take(1).uppercase(), style = MaterialTheme.typography.titleMedium, modifier = Modifier.semantics { contentDescription = "Accounts" })
                    }
                }
            },
            bottomBar = { if (model.selected == null && setup.isBlank() && top == null) WendyNavigation(tab) { tab = it } }
        ) { padding ->
            Column(Modifier.padding(padding).fillMaxSize()) {
                if (model.busy > 0 || !model.initialized) LinearProgressIndicator(Modifier.fillMaxWidth().height(2.dp))
                Box(Modifier.weight(1f)) {
                    // Pushed pages cover the screen; the pane underneath keeps its state.
                    val showing = top
                    if (showing != null) key(showing.key) { showing.content() }
                    else when {
                        !model.initialized -> Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) { EmptyMessage("Getting ready", "Your devices will be here in a moment.") }
                        setup == "setup" -> NearbySetupScreen(model) { setup = "" }
                        setup == "vpn" -> VpnScreen(model) { setup = "" }
                        model.selected != null -> if (model.session == 0L) DeviceConnectionScreen(model)
                            else if (model.lite) key(model.session) { LiteDeviceScreen(model) }
                            else key(model.session) { DeviceScreen(model, pane) { pane = it } }
                        else -> androidx.compose.animation.Crossfade(tab, label = "main navigation") { current ->
                            when (current) {
                                "Devices" -> DevicesScreen(model, { setup = "setup" }, { setup = "vpn" }, { address = true }, { accounts = true })
                                "Sites" -> SitesScreen(model)
                                else -> ActivityScreen(model) { device -> tab = "Devices"; model.connect(device) }
                            }
                        }
                    }
                }
            }
        }
        model.appURL?.let { url -> AppBrowser(url, { model.appURL = null }, openURL) }
        if (addDevice) ModalBottomSheet(onDismissRequest = { addDevice = false }, containerColor = MaterialTheme.colorScheme.background) {
            Column(Modifier.padding(start = 24.dp, end = 24.dp, bottom = 32.dp), verticalArrangement = Arrangement.spacedBy(14.dp)) {
                PageTitle("Bring a device online", "Choose how you want to connect.")
                SettingsRow("Set up a device", "Find it nearby using Bluetooth", Icons.Outlined.Bluetooth) { addDevice = false; setup = "setup" }
                SettingsRow("Connect by address", "Use a hostname or IP address", Icons.Outlined.Link) { addDevice = false; address = true }
            }
        }
        if (address) InputDialog("Connect to device", listOf(Field("address", "Hostname or IP address", "", "hostname:50051")), onDismiss = { address = false }, submitLabel = "Connect") { values ->
            val host = values.getValue("address").trim()
            if (host.isNotBlank()) { address = false; model.connect(Device(host, host, host, "LAN")) }
        }
        if (accounts) AccountSheet(model, { accounts = false }) { title, content -> accounts = false; nav.push(title, null, content) }
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

/** A connected device: Overview, Cameras and Apps, with the other tools in an overflow menu (iOS MobileDeviceView). */
@Composable
fun DeviceScreen(model: WendyModel, pane: String, select: (String) -> Unit) {
    var more by remember { mutableStateOf(false) }
    Column {
        Row(Modifier.fillMaxWidth().padding(horizontal = 20.dp, vertical = 8.dp), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Row(Modifier.weight(1f).horizontalScroll(rememberScrollState()), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                devicePanes.forEach { name -> PaneChip(name, pane == name) { select(name) } }
                if (pane in moreDevicePanes) PaneChip(pane, true) {}
            }
            Box {
                IconButton(onClick = { more = true }, modifier = Modifier.semantics { contentDescription = "More device tools" }) { Icon(Icons.Outlined.MoreHoriz, null) }
                DropdownMenu(more, onDismissRequest = { more = false }) {
                    moreDevicePanes.forEach { name -> DropdownMenuItem(text = { Text(name) }, onClick = { more = false; select(name) },
                        trailingIcon = if (pane == name) ({ Icon(Icons.Outlined.Check, null) }) else null) }
                }
            }
        }
        HorizontalDivider(color = MaterialTheme.colorScheme.outlineVariant)
        Box(Modifier.weight(1f)) { key(pane) {
            when (pane) {
                "Overview" -> DeviceOverviewScreen(model, select)
                "Apps" -> AppsScreen(model)
                "Cameras" -> CamerasScreen(model)
                "Digital Twin" -> DigitalTwinScreen(model)
                "Campaigns" -> CampaignsScreen(model)
                "Chat" -> ChatScreen(model)
            }
        } }
    }
}

@Composable
private fun WendyNavigation(selected: String, select: (String) -> Unit) {
    Surface(Modifier.fillMaxWidth().navigationBarsPadding().padding(horizontal = 20.dp, vertical = 10.dp), color = MaterialTheme.colorScheme.surface,
        shape = RoundedCornerShape(26.dp), border = BorderStroke(1.dp, MaterialTheme.colorScheme.outlineVariant), shadowElevation = 3.dp) {
        Row(Modifier.padding(7.dp), horizontalArrangement = Arrangement.spacedBy(6.dp)) {
            listOf("Devices" to Icons.Outlined.Dashboard, "Sites" to Icons.Outlined.Business, "Activity" to Icons.Outlined.Schedule).forEach { (name, icon) ->
                val active = name == selected
                val background by androidx.compose.animation.animateColorAsState(if (active) MaterialTheme.colorScheme.primaryContainer else Color.Transparent, label = "navigation")
                Surface(onClick = { select(name) }, color = background, shape = RoundedCornerShape(20.dp), modifier = Modifier.weight(1f).semantics { this.selected = active }) {
                    Column(Modifier.padding(vertical = 10.dp), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(4.dp)) {
                        Icon(icon, null, Modifier.size(22.dp), tint = if (active) MaterialTheme.colorScheme.onPrimaryContainer else MaterialTheme.colorScheme.onSurfaceVariant)
                        Text(name, style = MaterialTheme.typography.labelMedium, color = if (active) MaterialTheme.colorScheme.onPrimaryContainer else MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                }
            }
        }
    }
}

@Composable private fun rememberSaveableState(initial: String) = androidx.compose.runtime.saveable.rememberSaveable { mutableStateOf(initial) }

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun DevicesScreen(model: WendyModel, setup: () -> Unit, vpn: () -> Unit, address: () -> Unit, accounts: () -> Unit) {
    val scope = rememberCoroutineScope()
    var search by remember { mutableStateOf("") }
    var searching by remember { mutableStateOf(false) }
    var refreshing by remember { mutableStateOf(false) }
    val devices = model.devices.distinctBy { it.selector }.filter { search.isEmpty() || it.title.contains(search, true) || it.selector.contains(search, true) }
    PullToRefreshBox(refreshing, onRefresh = { scope.launch { refreshing = true; model.refresh(); refreshing = false } }) {
        LazyColumn(Modifier.fillMaxSize(), contentPadding = PaddingValues(start = 20.dp, end = 20.dp, top = 8.dp, bottom = 24.dp), verticalArrangement = Arrangement.spacedBy(16.dp)) {
            item {
                Box(Modifier.fillMaxWidth().clip(RoundedCornerShape(28.dp)).background(androidx.compose.ui.graphics.Brush.linearGradient(listOf(Color(0xFF163C31), Color(0xFF0A2722))))) {
                    DeviceIllustration(Modifier.align(Alignment.CenterEnd).size(160.dp).offset(x = 22.dp, y = 10.dp))
                    Column(Modifier.padding(24.dp), verticalArrangement = Arrangement.spacedBy(16.dp)) {
                        Eyebrow("YOUR DEVICE NETWORK", WendyMint)
                        Text("Your devices.\nWithin reach.", style = MaterialTheme.typography.headlineLarge, color = Color(0xFFF3F8EA), modifier = Modifier.fillMaxWidth(.77f))
                        Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(7.dp)) {
                            Box(Modifier.size(6.dp).background(WendyLime, androidx.compose.foundation.shape.CircleShape))
                            Text("${model.devices.distinctBy { it.selector }.size} ${if (model.source == "Cloud") "cloud" else "nearby"} devices", style = MaterialTheme.typography.labelMedium, color = WendyMint)
                        }
                    }
                }
            }
            item {
                Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                    ActionTile("Set up device", "Pair over Bluetooth", Icons.Outlined.AddCircleOutline, Modifier.weight(1f), setup)
                    ActionTile("Private network", "Access from other apps", Icons.Outlined.Shield, Modifier.weight(1f), vpn)
                }
            }
            item {
                Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
                    Text("Connections", style = MaterialTheme.typography.titleLarge, modifier = Modifier.weight(1f))
                    IconButton(onClick = { searching = !searching; if (!searching) search = "" }) { Icon(if (searching) Icons.Outlined.Close else Icons.Outlined.Search, "Search devices", Modifier.size(21.dp)) }
                    IconButton(onClick = { model.task { model.refresh() } }) { Icon(Icons.Outlined.Refresh, "Refresh devices", Modifier.size(21.dp)) }
                }
                ChoiceTabs(listOf("Nearby", "Cloud"), model.source, model::changeSource)
                androidx.compose.animation.AnimatedVisibility(searching) {
                    OutlinedTextField(search, { search = it }, Modifier.fillMaxWidth().padding(top = 12.dp), singleLine = true, placeholder = { Text("Find a device") }, leadingIcon = { Icon(Icons.Outlined.Search, null) })
                }
            }
            model.discoveryError?.let { item { FormSection { FailureNote(it) } } }
            if (devices.isEmpty()) item {
                Surface(color = MaterialTheme.colorScheme.surface, shape = RoundedCornerShape(24.dp), border = BorderStroke(1.dp, MaterialTheme.colorScheme.outlineVariant)) {
                    Column(Modifier.fillMaxWidth().padding(24.dp), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(12.dp)) {
                        IconTile(if (model.source == "Cloud") Icons.Outlined.CloudQueue else Icons.Outlined.WifiTethering, Modifier.size(54.dp))
                        Text(if (search.isNotBlank()) "No matching devices" else if (model.source == "Cloud" && model.account.isBlank()) "Your devices, anywhere" else if (model.source == "Cloud") "No cloud devices yet" else "No devices nearby", style = MaterialTheme.typography.titleMedium)
                        Text(if (search.isNotBlank()) "Try another name or address." else if (model.source == "Cloud" && model.account.isBlank()) "Sign in to bring your cloud devices here." else "Connect a device on your network, or add one by address.", style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant, textAlign = TextAlign.Center)
                        if (search.isBlank()) OutlinedButton(onClick = if (model.source == "Cloud" && model.account.isBlank()) accounts else address) { Text(if (model.source == "Cloud" && model.account.isBlank()) "Sign in" else "Connect by address"); Spacer(Modifier.width(8.dp)); Icon(Icons.Outlined.NorthEast, null, Modifier.size(16.dp)) }
                    }
                }
            }
            items(devices, key = { it.selector }) { device ->
                val current = model.selected?.selector == device.selector
                DeviceCard(device, connected = current && model.session != 0L, connecting = current && model.connecting) { model.connect(device) }
            }
        }
    }
}

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
