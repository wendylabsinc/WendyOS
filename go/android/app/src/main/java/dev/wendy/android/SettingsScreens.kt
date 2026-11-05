@file:OptIn(androidx.compose.foundation.layout.ExperimentalLayoutApi::class)

package dev.wendy.android

import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.*
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import org.json.JSONObject

@Composable
fun SettingsScreen(model: WendyModel) {
    var page by remember { mutableStateOf("Settings") }
    if (page != "Settings") {
        Column { TextButton(onClick = { page = "Settings" }) { Text("Back to settings") }
            when (page) {
                "Network" -> NetworkScreen(model)
                "Bluetooth" -> BluetoothScreen(model)
                "Audio" -> AudioScreen(model)
                "Sensors" -> PairingScreen(model, false)
                "IP cameras" -> PairingScreen(model, true)
                "Enrollment" -> EnrollmentScreen(model)
                "OS updates" -> UpdatesScreen(model)
                "Network traffic" -> DataPage(model, page, "NetworkTraffic", model.session)
                "Robots" -> RobotPairingScreen(model)
            }
        }
        return
    }
    LazyColumn(contentPadding = PaddingValues(20.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
        item { PageTitle("Settings", "Make this device work your way.") }
        val pages = if (model.lite) listOf("Network") else listOf("Network", "Bluetooth", "Audio", "Sensors", "IP cameras", "Robots", "Enrollment", "OS updates", "Network traffic")
        pages.forEach { name -> item {
            val (icon, subtitle) = when (name) {
                "Network" -> Icons.Outlined.Wifi to "Wi-Fi, mesh and connectivity"
                "Bluetooth" -> Icons.Outlined.Bluetooth to "Pair and manage peripherals"
                "Audio" -> Icons.Outlined.GraphicEq to "Inputs and outputs"
                "Sensors" -> Icons.Outlined.Sensors to "Connect the physical world"
                "IP cameras" -> Icons.Outlined.Videocam to "Network camera connections"
                "Robots" -> Icons.Outlined.SmartToy to "Manage paired robots"
                "Enrollment" -> Icons.Outlined.CloudQueue to "Connect to your organization"
                "OS updates" -> Icons.Outlined.SystemUpdate to "Keep WendyOS up to date"
                else -> Icons.Outlined.SwapVert to "Inspect device network usage"
            }
            SettingsRow(name, subtitle, icon) { page = name }
        } }
    }
}
@Composable
fun NetworkScreen(model: WendyModel) {
    val state = rememberRemote(model, "NetworkSettings", model.session)
    var join by remember { mutableStateOf<String?>(null) }
    var forget by remember { mutableStateOf<String?>(null) }
    val network = state.value.obj()
    Column(Modifier.verticalScroll(rememberScrollState()).padding(20.dp), verticalArrangement = Arrangement.spacedBy(14.dp)) {
        PageTitle("Network"); RemoteStatus(state)
        SectionCard("Wi-Fi") {
            val wifi = network.child("WiFi")
            if (!state.loading && network.optJSONObject("WiFi") != null) {
                StatusPill(if (wifi.flag("connected")) "Connected" else "Not connected", active = wifi.flag("connected"))
                if (wifi.flag("connected")) Text(wifi.text("ssid").ifBlank { "Wi-Fi network" }, style = MaterialTheme.typography.titleLarge)
            }
            network.text("WiFiError").takeIf { it.isNotEmpty() }?.let { Text(it, color = MaterialTheme.colorScheme.error) }
            Row { TextButton(onClick = { join = "" }) { Text("Join network") }; TextButton(onClick = { model.task { model.backend.call("DisconnectWiFi", model.session); state.refresh() } }) { Text("Disconnect") } }
            network.list("Networks").forEach { wifi -> TextButton(onClick = { join = wifi.text("ssid") }) { Text(wifi.text("ssid")) } }
            network.list("Known").forEach { wifi -> Row { Text(wifi.text("ssid"), Modifier.weight(1f)); TextButton(onClick = { forget = wifi.text("ssid") }) { Text("Forget") } } }
        }
        SectionCard("Wendy Net") {
            val mesh = network.child("Mesh")
            if (network.optJSONObject("Mesh") != null) {
                Text(mesh.text("state").replaceFirstChar { it.uppercase() }, style = MaterialTheme.typography.titleMedium)
                OptionalMessage(mesh.text("detail"))
                SummaryLine("Connected peers", mesh.number("authenticated_peers").toString())
            }
            OptionalMessage(network.text("MeshError"))
            val configuration = mesh.child("configured")
            listOf("participate" to 1L, "ble" to 16L, "infrastructure_wifi" to 64L, "nan" to 8L).forEach { (key, bit) ->
                Row { Text(when (key) { "participate" -> "Participate in mesh"; "ble" -> "Bluetooth links"; "nan" -> "Wi-Fi Aware"; else -> "Wi-Fi links" }, Modifier.weight(1f)); Switch(checked = configuration.flag(key), enabled = mesh.has("configured"), onCheckedChange = { enabled -> model.task { model.backend.call("ConfigureMesh", model.session, bit, if (enabled) bit else 0); state.refresh() } }) }
            }
        }
        TechnicalDetails(network, "Network diagnostics")
        TextButton(onClick = state::refresh) { Text("Refresh") }
    }
    join?.let { ssid -> InputDialog("Join Wi-Fi", listOf(Field("ssid", "Network name", ssid), Field("password", "Password", secret = true)), { join = null }) { fields -> join = null; model.task { model.backend.call("JoinWiFi", model.session, fields.getValue("ssid"), fields.getValue("password"), 0, false); state.refresh() } } }
    forget?.let { ssid -> ConfirmDialog("Forget $ssid?", "The device will remove its saved credentials for this network.", { forget = null }) { forget = null; model.task { model.backend.call("ForgetWiFi", model.session, ssid); state.refresh() } } }
}
@Composable
fun BluetoothScreen(model: WendyModel) {
    val state = rememberRemote(model, "BluetoothScan", model.session)
    Column(Modifier.verticalScroll(rememberScrollState()).padding(20.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
        PageTitle("Device Bluetooth", "Bluetooth peripherals visible to the connected device"); RemoteStatus(state)
        TextButton(onClick = state::refresh) { Text("Scan again") }
        state.value.items().forEach { device -> SectionCard(device.text("name", "address")) {
            StatusPill(if (device.flag("connected")) "Connected" else if (device.flag("paired")) "Paired" else "Not paired", active = device.flag("connected"))
            TechnicalDetails(device)
            FlowRow { listOf("Connect" to 0, "Disconnect" to 2, "Pair" to 1, "Forget" to 3).forEach { (label, action) -> TextButton(onClick = { model.task { model.backend.call("BluetoothAction", model.session, device.text("address"), action); state.refresh() } }) { Text(label) } } }
        } }
    }
}
@Composable
fun AudioScreen(model: WendyModel) {
    val state = rememberRemote(model, "AudioDevices", model.session)
    Column(Modifier.verticalScroll(rememberScrollState()).padding(20.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
        PageTitle("Audio"); RemoteStatus(state)
        state.value.items().forEach { audio -> SectionCard(audio.text("description", "name")) {
            val kind = when (audio.number("type")) { 1L -> "Microphone"; 2L -> "Speaker"; else -> "Audio device" }
            StatusPill(if (audio.flag("is_default")) "Default ${kind.lowercase()}" else kind, active = audio.flag("is_default"))
            if (audio.has("volume_percent")) SummaryLine("Volume", "${audio.number("volume_percent")}%")
            FlowRow {
            TextButton(onClick = { model.task { model.backend.call("SetDefaultAudio", model.session, audio.number("device_id"), 1); state.refresh() } }) { Text("Default input") }
            TextButton(onClick = { model.task { model.backend.call("SetDefaultAudio", model.session, audio.number("device_id"), 2); state.refresh() } }) { Text("Default output") }
        } } }
    }
}
@Composable
fun PairingScreen(model: WendyModel, cameras: Boolean) {
    val state = rememberRemote(model, "Pairings", model.session, cameras)
    var credentials by remember { mutableStateOf<JSONObject?>(null) }
    var remove by remember { mutableStateOf<JSONObject?>(null) }
    fun pair(item: JSONObject, unpair: Boolean, username: String = "", password: String = "") = model.task {
        if (cameras) model.backend.call("PairIPCamera", model.session, item.number("CameraID"), username, password, unpair)
        else model.backend.call("PairSensor", model.session, item.text("ID"), unpair)
        state.refresh()
    }
    Column(Modifier.verticalScroll(rememberScrollState()).padding(20.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
        PageTitle(if (cameras) "IP cameras" else "Sensors"); RemoteStatus(state); Text(state.value.obj().text("Warning")); TextButton(onClick = state::refresh) { Text("Scan again") }
        state.value.obj().list("Items").forEach { item -> SectionCard(item.text("Name", "ID")) {
            Text(item.text("Detail")); Text(item.text("Address"))
            if (item.flag("Paired")) TextButton(onClick = { remove = item }) { Text("Unpair") }
            else Button(enabled = item.flag("CanPair"), onClick = { if (cameras) credentials = item else pair(item, false) }) { Text("Pair") }
        } }
    }
    credentials?.let { item -> InputDialog("Camera credentials", listOf(Field("username", "Username"), Field("password", "Password", secret = true)), { credentials = null }) { fields -> credentials = null; pair(item, false, fields.getValue("username"), fields.getValue("password")) } }
    remove?.let { item -> ConfirmDialog("Unpair ${item.text("Name")}?", "The device will stop using this source.", { remove = null }) { remove = null; pair(item, true) } }
}
@Composable
fun EnrollmentScreen(model: WendyModel) {
    val state = rememberRemote(model, "DeviceEnrollment", model.session)
    val orgs = rememberRemote(model, "EnrollmentOrganizations")
    var target by remember { mutableStateOf<JSONObject?>(null) }
    Column(Modifier.verticalScroll(rememberScrollState()).padding(20.dp), verticalArrangement = Arrangement.spacedBy(14.dp)) {
        PageTitle("Cloud enrollment"); RemoteStatus(state)
        if (!state.loading && state.error == null) SectionCard(if (state.value.obj().flag("Enrolled")) "Connected to Wendy Cloud" else "Add this device to your organization") {
            if (state.value.obj().flag("Enrolled")) {
                OptionalMessage(state.value.obj().text("OrganizationName"))
                Text("This device is enrolled and can be accessed through your organization.")
            } else Text("Choose an organization below to enroll this device.")
            TechnicalDetails(state.value)
        }
        if (!state.loading && state.error == null && !state.value.obj().flag("Enrolled")) {
            RemoteStatus(orgs); orgs.value.items().forEach { org -> OutlinedButton(onClick = { target = org }) { Text("Enroll in ${org.text("Name")}") } }
        }
    }
    target?.let { org -> InputDialog("Enroll device", listOf(Field("name", "Device name", model.selected?.name ?: "")), { target = null }, "Enroll") { fields -> target = null; model.task { model.backend.call("EnrollDevice", model.session, org.text("Account"), org.text("ID"), fields.getValue("name")); state.refresh() } } }
}
@Composable
fun UpdatesScreen(model: WendyModel) {
    val state = rememberRemote(model, "OnlineUpdate", model.session, 0, "")
    var target by remember { mutableStateOf(false) }
    Column(Modifier.verticalScroll(rememberScrollState()).padding(20.dp), verticalArrangement = Arrangement.spacedBy(14.dp)) {
        PageTitle("OS updates"); RemoteStatus(state)
        if (!state.loading && state.error == null) SectionCard(when (state.value.obj().text("phase")) {
            "current" -> "You're up to date"
            "available" -> "An update is available"
            "downloading" -> "Downloading update"
            "installing" -> "Installing update"
            "restarting" -> "Restarting device"
            "checking" -> "Checking for updates"
            "failed" -> "Update needs attention"
            else -> "Device software"
        }) {
            val update = state.value.obj()
            SummaryLine("Installed", update.text("installed_version"))
            if (update.flag("update_available")) SummaryLine("Available", update.text("latest_version"))
            OptionalMessage(update.text("detail"))
            if (update.number("total_bytes") > 0) LinearProgressIndicator(progress = { (update.number("downloaded_bytes").toFloat() / update.number("total_bytes")).coerceIn(0f, 1f) }, modifier = Modifier.fillMaxWidth())
        }
        TextButton(onClick = { model.task { model.backend.call("OnlineUpdate", model.session, 1, ""); state.refresh() } }) { Text("Check for updates") }
        Button(onClick = { target = true }) { Text("Install update") }
        TextButton(onClick = state::refresh) { Text("Refresh status") }
    }
    if (target) InputDialog("Install OS update", listOf(Field("target", "Target version", hint = "Version returned by check")), { target = false }, "Install") { fields -> target = false; model.task { model.backend.call("OnlineUpdate", model.session, 2, fields.getValue("target")); state.refresh() } }
}
@Composable
fun RobotPairingScreen(model: WendyModel) {
    val state = rememberRemote(model, "UnitreePairings", model.session)
    var add by remember { mutableStateOf(false) }
    Column(Modifier.verticalScroll(rememberScrollState()).padding(20.dp), verticalArrangement = Arrangement.spacedBy(14.dp)) {
        PageTitle("Robots"); RemoteStatus(state)
        OptionalMessage(state.value.obj().text("Warning"))
        state.value.obj().list("Robots").forEach { robot -> SectionCard(robot.text("Name").ifBlank { "Unitree robot" }) {
            StatusPill(if (robot.flag("Connected")) "Connected" else if (robot.flag("Paired")) "Paired" else "Not paired", active = robot.flag("Connected"))
            TechnicalDetails(robot)
        } }
        if (!state.loading && state.error == null && state.value.obj().list("Robots").isEmpty()) EmptyMessage("No robots paired", "Pair a robot to manage its connection here.")
        Button(onClick = { add = true }) { Text("Pair Unitree robot") }
    }
    if (add) InputDialog("Pair Unitree", listOf(Field("serial", "Serial number"), Field("name", "Name"), Field("address", "Robot address"), Field("region", "Region", "global"), Field("email", "Unitree email"), Field("password", "Password", secret = true)), { add = false }, "Pair") { f -> add = false; model.task { model.backend.call("PairUnitree", model.session, f["serial"], f["name"], f["address"], f["region"], f["email"], f["password"]); state.refresh() } }
}
@Composable
fun OrganizationScreen(model: WendyModel) {
    val state = rememberRemote(model, "Organization", model.account, model.organization)
    var member by remember { mutableStateOf(false) }
    var invite by remember { mutableStateOf(false) }
    var access by remember { mutableStateOf(false) }
    Column(Modifier.verticalScroll(rememberScrollState()).padding(20.dp), verticalArrangement = Arrangement.spacedBy(14.dp)) {
        PageTitle("Organization"); RemoteStatus(state)
        FlowRow { TextButton(onClick = { member = true }) { Text("Add member") }; TextButton(onClick = { invite = true }) { Text("Create invite") }; TextButton(onClick = { access = true }) { Text("Grant access") } }
        if (!state.loading && state.error == null) {
            val data = state.value.obj()
            val roles = data.list("Roles").associate { it.text("id") to it.text("name") }
            SectionCard(data.child("Organization").text("name").ifBlank { "Your organization" }) {
                if (data.text("DevicesError").isEmpty()) SummaryLine("Devices", data.list("Devices").size.toString())
                if (data.text("TeamError").isEmpty()) SummaryLine("Members", data.list("Members").size.toString())
                if (data.text("InvitesError").isEmpty()) SummaryLine("Invitations", data.list("Invites").size.toString())
            }
            SectionCard("People") {
                OptionalMessage(data.text("TeamError"))
                data.list("Members").forEach { person ->
                    val role = roles[person.child("Member").text("role_id")]
                    Column(Modifier.padding(vertical = 8.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                        Text(person.text("Name", "Email").ifBlank { "Organization member" }, style = MaterialTheme.typography.titleMedium)
                        if (person.text("Name").isNotBlank()) OptionalMessage(person.text("Email"))
                        role?.let { Text(it, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.primary) }
                        OptionalMessage(person.text("ProfileError"))
                    }
                }
            }
            listOf("RolesError", "DevicesError", "AccessError", "InvitesError", "PermissionsError").forEach { OptionalMessage(data.text(it)) }
            TechnicalDetails(data, "Organization diagnostics")
        }
        TextButton(onClick = state::refresh) { Text("Refresh") }
    }
    val data = state.value.obj()
    val roleChoices = data.list("Roles").map { it.text("id") to it.text("name").ifBlank { "Custom role" } }.filter { it.first.isNotBlank() }
    val peopleChoices = data.list("Members").map { it.child("Member").text("user_id") to it.text("Name", "Email").ifBlank { "Organization member" } }.filter { it.first.isNotBlank() }
    val deviceChoices = data.list("Devices").filter { it.flag("is_compute_device") }.map { it.text("id") to it.text("name").ifBlank { "Unnamed device" } }.filter { it.first.isNotBlank() }
    if (member) InputDialog("Add member", listOf(Field("email", "Email"), Field("role", "Role", choices = roleChoices)), { member = false }) { f -> member = false; model.task { model.backend.call("AddMember", model.account, model.organization, f["email"], f["role"]); state.refresh() } }
    if (invite) InputDialog("Create invite", listOf(Field("role", "Role", choices = roleChoices), Field("days", "Expires after, in days", "7", numeric = true)), { invite = false }) { f -> model.task {
        val days = f.getValue("days").toLongOrNull()
        require(days != null && days in 1..365) { "Enter an expiry between 1 and 365 days." }
        val expires = System.currentTimeMillis() / 1000 + days * 86400
        invite = false; val url = model.backend.call("CreateInvite", model.account, model.organization, f["role"], expires); model.record("Invite: $url"); state.refresh()
    } }
    if (access) InputDialog("Grant device access", listOf(Field("user", "Person", choices = peopleChoices), Field("target", "Device", choices = deviceChoices)), { access = false }) { f -> access = false; model.task { model.backend.call("CreateAccess", model.account, model.organization, f["user"], f["target"], 1); state.refresh() } }
}
