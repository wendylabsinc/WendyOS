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
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.semantics.*
import androidx.lifecycle.ViewModelProvider
import org.json.JSONObject

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

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun WendyApp(model: WendyModel, openVpn: Boolean = false, openURL: (String) -> Unit) {
    var tab by rememberSaveableState("Devices")
    var accounts by remember { mutableStateOf(false) }
    var address by remember { mutableStateOf(false) }
    var addDevice by remember { mutableStateOf(false) }
    var setup by rememberSaveableState(if (openVpn) "vpn" else "")
    LaunchedEffect(model.browserURL) { model.browserURL?.let { model.browserURL = null; openURL(it) } }
    BackHandler(model.selected != null || setup.isNotBlank()) { if (setup.isNotBlank()) setup = "" else model.disconnect() }
    Scaffold(
        containerColor = MaterialTheme.colorScheme.background,
        topBar = {
            Row(Modifier.fillMaxWidth().windowInsetsPadding(WindowInsets.statusBars).padding(horizontal = 20.dp, vertical = 12.dp),
                verticalAlignment = androidx.compose.ui.Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                if (model.selected != null || setup.isNotBlank()) {
                    FilledTonalIconButton(onClick = { if (setup.isNotBlank()) setup = "" else model.disconnect() }, colors = IconButtonDefaults.filledTonalIconButtonColors(containerColor = MaterialTheme.colorScheme.surface)) {
                        Icon(Icons.AutoMirrored.Outlined.ArrowBack, "Back to devices", Modifier.size(22.dp))
                    }
                    Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(2.dp)) {
                        Text(model.selected?.name ?: if (setup == "setup") "Device setup" else "Private network", style = MaterialTheme.typography.titleMedium, maxLines = 1, overflow = androidx.compose.ui.text.style.TextOverflow.Ellipsis)
                        Text(if (model.selected == null) "Wendy" else if (model.session == 0L) "Connecting" else "Connected via ${model.selected?.transport}", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                } else {
                    WendyMark(Modifier.size(34.dp, 28.dp))
                    Text("wendy", style = MaterialTheme.typography.headlineMedium, fontWeight = FontWeight.Bold)
                    Spacer(Modifier.weight(1f))
                    IconButton(onClick = { addDevice = true }, enabled = model.initialized) { Icon(Icons.Outlined.Add, "Add a device") }
                }
                FilledTonalIconButton(onClick = { accounts = true }, enabled = model.initialized,
                    colors = IconButtonDefaults.filledTonalIconButtonColors(containerColor = MaterialTheme.colorScheme.secondaryContainer)) {
                    val email = model.accounts.firstOrNull { it.text("Context") == model.account }?.text("Email") ?: ""
                    if (email.isBlank()) Icon(Icons.Outlined.PersonOutline, "Accounts", Modifier.size(22.dp))
                    else Text(email.take(1).uppercase(), style = MaterialTheme.typography.titleMedium, modifier = Modifier.semantics { contentDescription = "Accounts" })
                }
            }
        },
        bottomBar = { if (model.selected == null && setup.isBlank()) WendyNavigation(tab) { tab = it } }
    ) { padding ->
        Column(Modifier.padding(padding).fillMaxSize()) {
            if (model.busy > 0 || !model.initialized) LinearProgressIndicator(Modifier.fillMaxWidth().height(2.dp))
            when {
                !model.initialized -> Box(Modifier.fillMaxSize(), contentAlignment = androidx.compose.ui.Alignment.Center) { EmptyMessage("Getting ready", "Your devices will be here in a moment.") }
                setup == "setup" -> NearbySetupScreen(model) { setup = "" }
                setup == "vpn" -> VpnScreen(model) { setup = "" }
                model.selected != null -> if (model.session == 0L) {
                    Column(Modifier.fillMaxSize().padding(24.dp), horizontalAlignment = androidx.compose.ui.Alignment.CenterHorizontally, verticalArrangement = Arrangement.Center) {
                        DeviceIllustration(Modifier.size(200.dp), MaterialTheme.colorScheme.primary)
                        EmptyMessage("Making the connection", "Connecting securely to ${model.selected!!.name}.")
                        OutlinedButton(onClick = { model.selected?.let { model.connect(it) } }) { Text("Try again") }
                    }
                } else key(model.session) { DeviceScreen(model, openURL) }
                else -> androidx.compose.animation.Crossfade(tab, label = "main navigation") { current ->
                    when (current) {
                        "Devices" -> DevicesScreen(model, { setup = "setup" }, { setup = "vpn" }, { address = true }, { accounts = true })
                        "Sites" -> SitesScreen(model)
                        else -> ActivityScreen(model)
                    }
                }
            }
        }
    }
    model.appURL?.let { url -> AppBrowser(url, { model.appURL = null }, openURL) }
    if (addDevice) ModalBottomSheet(onDismissRequest = { addDevice = false }, containerColor = MaterialTheme.colorScheme.background) {
        Column(Modifier.padding(start = 24.dp, end = 24.dp, bottom = 32.dp), verticalArrangement = Arrangement.spacedBy(14.dp)) {
            PageTitle("Bring a device online", "Choose how you want to connect.")
            SettingsRow("Set up a new device", "Find it nearby using Bluetooth", Icons.Outlined.Bluetooth) { addDevice = false; setup = "setup" }
            SettingsRow("Connect by address", "Use a hostname or IP address", Icons.Outlined.Link) { addDevice = false; address = true }
        }
    }
    if (address) InputDialog("Connect by address", listOf(Field("address", "Hostname or IP address", "", "wendy.local:50051")), onDismiss = { address = false }, submitLabel = "Connect") { values ->
        val host = values.getValue("address").trim()
        if (host.isNotBlank()) { address = false; model.connect(Device(host, host, host, "LAN")) }
    }
    if (accounts) AccountSheet(model) { accounts = false }
    model.error?.let { message -> AlertDialog(onDismissRequest = { model.error = null }, icon = { Icon(Icons.Outlined.ErrorOutline, null) }, title = { Text("Could not complete request") }, text = { Text(message, Modifier.verticalScroll(rememberScrollState())) }, confirmButton = { TextButton(onClick = { model.error = null }) { Text("OK") } }) }
    model.recovery?.let { device -> AlertDialog(onDismissRequest = { model.recovery = null }, title = { Text("Device identity changed") }, text = { Text("Only forget the saved identity if you have verified that ${device.name} was reinstalled or replaced. A different device may be using its address.") }, confirmButton = { TextButton(onClick = { model.recovery = null; model.error = null; model.connect(device, true) }) { Text("Forget identity and reconnect") } }, dismissButton = { TextButton(onClick = { model.recovery = null }) { Text("Cancel") } }) }
}

@Composable
private fun WendyNavigation(selected: String, select: (String) -> Unit) {
    Surface(Modifier.fillMaxWidth().navigationBarsPadding().padding(horizontal = 20.dp, vertical = 10.dp), color = MaterialTheme.colorScheme.surface,
        shape = RoundedCornerShape(26.dp), border = BorderStroke(1.dp, MaterialTheme.colorScheme.outlineVariant), shadowElevation = 3.dp) {
        Row(Modifier.padding(7.dp), horizontalArrangement = Arrangement.spacedBy(6.dp)) {
            listOf("Devices" to Icons.Outlined.Dashboard, "Sites" to Icons.Outlined.Language, "Activity" to Icons.Outlined.Bolt).forEach { (name, icon) ->
                val active = name == selected
                val background by androidx.compose.animation.animateColorAsState(if (active) MaterialTheme.colorScheme.primaryContainer else Color.Transparent, label = "navigation")
                Surface(onClick = { select(name) }, color = background, shape = RoundedCornerShape(20.dp), modifier = Modifier.weight(1f).semantics { this.selected = active }) {
                    Column(Modifier.padding(vertical = 10.dp), horizontalAlignment = androidx.compose.ui.Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(4.dp)) {
                        Icon(icon, null, Modifier.size(22.dp), tint = if (active) MaterialTheme.colorScheme.onPrimaryContainer else MaterialTheme.colorScheme.onSurfaceVariant)
                        Text(name, style = MaterialTheme.typography.labelMedium, color = if (active) MaterialTheme.colorScheme.onPrimaryContainer else MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                }
            }
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun AccountSheet(model: WendyModel, dismiss: () -> Unit) {
    ModalBottomSheet(onDismissRequest = dismiss, containerColor = MaterialTheme.colorScheme.background) {
        Column(Modifier.verticalScroll(rememberScrollState()).padding(start = 24.dp, end = 24.dp, bottom = 32.dp), verticalArrangement = Arrangement.spacedBy(16.dp)) {
            PageTitle(if (model.accounts.isEmpty()) "Your world, connected" else "Your workspace", if (model.accounts.isEmpty()) "Sign in to reach your devices from anywhere." else "Accounts and organizations")
            model.accounts.forEach { value ->
                val selected = value.text("Context") == model.account
                Surface(onClick = { model.chooseAccount(value.text("Context")) }, shape = RoundedCornerShape(20.dp), color = MaterialTheme.colorScheme.surface,
                    border = BorderStroke(1.dp, if (selected) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.outlineVariant)) {
                    Row(Modifier.fillMaxWidth().padding(18.dp), verticalAlignment = androidx.compose.ui.Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                        IconTile(Icons.Outlined.PersonOutline)
                        Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                            Text(value.text("Email", "Context"), style = MaterialTheme.typography.titleSmall)
                            Text(value.text("Cloud").removePrefix("https://"), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                        }
                        if (selected) Icon(Icons.Outlined.CheckCircle, "Selected account", tint = MaterialTheme.colorScheme.primary, modifier = Modifier.size(20.dp))
                    }
                }
            }
            if (model.organizations.isNotEmpty()) Eyebrow("Organization")
            model.organizations.forEach { value ->
                Surface(onClick = { model.chooseAccount(model.account, value.text("id")) }, shape = RoundedCornerShape(16.dp), color = MaterialTheme.colorScheme.surface) {
                    Row(Modifier.fillMaxWidth().padding(16.dp), verticalAlignment = androidx.compose.ui.Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                        Icon(Icons.Outlined.Business, null, Modifier.size(20.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
                        Text(value.text("name"), Modifier.weight(1f), style = MaterialTheme.typography.titleSmall)
                        if (value.text("id") == model.organization) Icon(Icons.Outlined.Check, "Selected organization", Modifier.size(20.dp), tint = MaterialTheme.colorScheme.primary)
                    }
                }
            }
            Button(onClick = { dismiss(); model.signIn() }, enabled = !model.loginPending, modifier = Modifier.fillMaxWidth().heightIn(min = 52.dp)) { Text(if (model.accounts.isEmpty()) "Sign in to Wendy Cloud" else "Add another account") }
            if (model.loginPending) OutlinedButton(onClick = model::cancelLogin, modifier = Modifier.fillMaxWidth()) { Text("Cancel sign-in") }
            TextButton(onClick = { dismiss(); model.signIn(true) }, enabled = !model.loginPending, modifier = Modifier.align(androidx.compose.ui.Alignment.CenterHorizontally)) { Text("Use development cloud", style = MaterialTheme.typography.bodySmall) }
        }
    }
}

@Composable private fun rememberSaveableState(initial: String) = androidx.compose.runtime.saveable.rememberSaveable { mutableStateOf(initial) }

@Composable
fun DevicesScreen(model: WendyModel, setup: () -> Unit, vpn: () -> Unit, address: () -> Unit, accounts: () -> Unit) {
    var search by remember { mutableStateOf("") }
    var searching by remember { mutableStateOf(false) }
    val devices = model.devices.distinctBy { it.selector }.filter { it.name.contains(search, true) || it.selector.contains(search, true) }
    LazyColumn(contentPadding = PaddingValues(start = 20.dp, end = 20.dp, top = 8.dp, bottom = 24.dp), verticalArrangement = Arrangement.spacedBy(16.dp)) {
        item {
            Box(Modifier.fillMaxWidth().clip(RoundedCornerShape(28.dp)).background(androidx.compose.ui.graphics.Brush.linearGradient(listOf(Color(0xFF163C31), Color(0xFF0A2722))))) {
                DeviceIllustration(Modifier.align(androidx.compose.ui.Alignment.CenterEnd).size(160.dp).offset(x = 22.dp, y = 10.dp))
                Column(Modifier.padding(24.dp), verticalArrangement = Arrangement.spacedBy(16.dp)) {
                    Eyebrow("YOUR DEVICE NETWORK", WendyMint)
                    Text("Your devices.\nWithin reach.", style = MaterialTheme.typography.headlineLarge, color = Color(0xFFF3F8EA), modifier = Modifier.fillMaxWidth(.77f))
                    Row(verticalAlignment = androidx.compose.ui.Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(7.dp)) {
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
            Row(Modifier.fillMaxWidth(), verticalAlignment = androidx.compose.ui.Alignment.CenterVertically) {
                Text("Connections", style = MaterialTheme.typography.titleLarge, modifier = Modifier.weight(1f))
                IconButton(onClick = { searching = !searching; if (!searching) search = "" }) { Icon(if (searching) Icons.Outlined.Close else Icons.Outlined.Search, "Search devices", Modifier.size(21.dp)) }
                IconButton(onClick = { model.task { model.refresh() } }) { Icon(Icons.Outlined.Refresh, "Refresh devices", Modifier.size(21.dp)) }
            }
            ChoiceTabs(listOf("Nearby", "Cloud"), model.source, model::changeSource)
            androidx.compose.animation.AnimatedVisibility(searching) {
                OutlinedTextField(search, { search = it }, Modifier.fillMaxWidth().padding(top = 12.dp), singleLine = true, placeholder = { Text("Find a device") }, leadingIcon = { Icon(Icons.Outlined.Search, null) })
            }
        }
        if (devices.isEmpty()) item {
            Surface(color = MaterialTheme.colorScheme.surface, shape = RoundedCornerShape(24.dp), border = BorderStroke(1.dp, MaterialTheme.colorScheme.outlineVariant)) {
                Column(Modifier.fillMaxWidth().padding(24.dp), horizontalAlignment = androidx.compose.ui.Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(12.dp)) {
                    IconTile(if (model.source == "Cloud") Icons.Outlined.CloudQueue else Icons.Outlined.WifiTethering, Modifier.size(54.dp))
                    Text(if (search.isNotBlank()) "No matching devices" else if (model.source == "Cloud" && model.account.isBlank()) "Your devices, anywhere" else if (model.source == "Cloud") "No cloud devices yet" else "No devices nearby", style = MaterialTheme.typography.titleMedium)
                    Text(if (search.isNotBlank()) "Try another name or address." else if (model.source == "Cloud" && model.account.isBlank()) "Sign in to bring your cloud devices here." else "Connect a device on your network, or add one by address.", style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant, textAlign = androidx.compose.ui.text.style.TextAlign.Center)
                    if (search.isBlank()) OutlinedButton(onClick = if (model.source == "Cloud" && model.account.isBlank()) accounts else address) { Text(if (model.source == "Cloud" && model.account.isBlank()) "Sign in" else "Connect by address"); Spacer(Modifier.width(8.dp)); Icon(Icons.Outlined.NorthEast, null, Modifier.size(16.dp)) }
                }
            }
        }
        items(devices, key = { it.selector }) { device -> DeviceCard(device) { model.connect(device) } }
    }
}

@Composable
fun ActivityScreen(model: WendyModel) {
    LazyColumn(contentPadding = PaddingValues(24.dp), verticalArrangement = Arrangement.spacedBy(20.dp)) {
        item { PageTitle("Activity", "The latest from your devices.") }
        if (model.activity.isEmpty()) item { EmptyMessage("A fresh start", "Connections and device actions will appear here.", Icons.Outlined.Bolt) }
        items(model.activity.size) { index ->
            Row(horizontalArrangement = Arrangement.spacedBy(14.dp)) {
                IconTile(if (model.activity[index].startsWith("Connected")) Icons.Outlined.Link else Icons.Outlined.Bolt)
                Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    Text(model.activity[index], style = MaterialTheme.typography.bodyMedium)
                    Text(if (index == 0) "Latest activity" else "This session", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    HorizontalDivider(Modifier.padding(top = 8.dp), color = MaterialTheme.colorScheme.outlineVariant)
                }
            }
        }
    }
}

@Composable
fun DeviceScreen(model: WendyModel, openURL: (String) -> Unit) {
    var pane by rememberSaveableState("Overview")
    val panes = if (model.lite) listOf("Overview", "Cameras", "Settings") else listOf("Overview", "Apps", "Cameras", "Campaigns", "Recordings", "Chat", "Models", "Settings")
    Column {
        Row(Modifier.horizontalScroll(rememberScrollState()).padding(horizontal = 20.dp, vertical = 8.dp), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            panes.forEach { name ->
                val selected = pane == name
                Surface(onClick = { pane = name }, color = if (selected) MaterialTheme.colorScheme.primaryContainer else Color.Transparent, shape = RoundedCornerShape(14.dp), modifier = Modifier.semantics { this.selected = selected }) {
                    Text(name, Modifier.padding(horizontal = 16.dp, vertical = 14.dp), style = MaterialTheme.typography.labelLarge, color = if (selected) MaterialTheme.colorScheme.onPrimaryContainer else MaterialTheme.colorScheme.onSurfaceVariant)
                }
            }
        }
        Box(Modifier.weight(1f)) { key(pane) {
            when (pane) {
                "Overview" -> OverviewScreen(model)
                "Apps" -> AppsScreen(model, openURL)
                "Cameras" -> CamerasScreen(model)
                "Campaigns" -> CampaignsScreen(model)
                "Recordings" -> RecordingsScreen(model)
                "Chat" -> ChatScreen(model)
                "Models" -> ModelsScreen(model)
                "Settings" -> SettingsScreen(model)
            }
        } }
    }
}

@Composable fun PageTitle(title: String, subtitle: String = "") {
    Column(Modifier.padding(bottom = 6.dp), verticalArrangement = Arrangement.spacedBy(7.dp)) {
        Text(title, style = MaterialTheme.typography.headlineLarge)
        if (subtitle.isNotEmpty()) Text(subtitle, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}
@Composable fun EmptyMessage(title: String, message: String, icon: androidx.compose.ui.graphics.vector.ImageVector = Icons.Outlined.Sensors) {
    Column(Modifier.fillMaxWidth().padding(horizontal = 24.dp, vertical = 32.dp), verticalArrangement = Arrangement.spacedBy(12.dp), horizontalAlignment = androidx.compose.ui.Alignment.CenterHorizontally) {
        IconTile(icon, Modifier.size(56.dp))
        Text(title, style = MaterialTheme.typography.titleLarge, textAlign = androidx.compose.ui.text.style.TextAlign.Center)
        Text(message, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant, textAlign = androidx.compose.ui.text.style.TextAlign.Center)
    }
}
