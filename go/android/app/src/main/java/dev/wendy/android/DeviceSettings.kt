package dev.wendy.android

import androidx.compose.foundation.Canvas
import androidx.compose.foundation.clickable
import androidx.compose.foundation.gestures.rememberTransformableState
import androidx.compose.foundation.gestures.transformable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.material.icons.automirrored.outlined.*
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.graphics.PathEffect
import androidx.compose.ui.graphics.graphicsLayer
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import org.json.JSONObject
import java.text.DateFormat
import java.util.Date

/** Runs one user action at a time and keeps its failure next to the control that caused it. */
class ActionRunner(private val scope: CoroutineScope) {
    var busy by mutableStateOf<String?>(null); private set
    var error by mutableStateOf<String?>(null)
    fun run(key: String = "action", block: suspend () -> Unit) {
        if (busy != null) return
        busy = key; error = null
        scope.launch {
            try { block() } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message ?: "Request failed" } finally { busy = null }
        }
    }
}
@Composable fun rememberActionRunner(): ActionRunner { val scope = rememberCoroutineScope(); return remember { ActionRunner(scope) } }

/** Network settings shared by the settings menu and its Wi-Fi and mesh pages, as on iOS. */
class NetworkSettingsState(private val model: WendyModel, val session: Long) {
    var snapshot by mutableStateOf<NetworkSnapshot?>(null); private set
    var loading by mutableStateOf(false); private set
    var saving by mutableStateOf(false); private set
    var error by mutableStateOf<String?>(null)
    var notice by mutableStateOf<String?>(null)
    var draft by mutableIntStateOf(0)
    val dirty get() = snapshot?.let { it.meshConfig != draft } ?: false
    val participates get() = draft and 1 != 0
    suspend fun refresh() {
        if (loading) return
        loading = true
        try {
            val value = NetworkSnapshot.parse(model.backend.call("NetworkSettings", session).obj())
            if (model.session != session) return
            if (!dirty) draft = value.meshConfig
            snapshot = value; error = null
            if (notice != null && value.wifiAvailable) notice = if (value.wifiConnected) "Connected to ${value.ssid}." else null
        } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { loading = false }
    }
    suspend fun perform(work: suspend () -> Unit) {
        if (saving) return
        saving = true; error = null; notice = null
        try { work(); refresh() } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { saving = false }
    }
    suspend fun saveMesh() {
        val current = snapshot ?: return
        val changed = current.meshConfig xor draft
        val enabled = draft
        perform { model.backend.call("ConfigureMesh", session, changed, enabled) }
    }
}

@Composable
fun DeviceSettingsScreen(model: WendyModel) {
    val nav = LocalNavigator.current
    val session = model.session
    val scope = rememberCoroutineScope()
    val network = remember(session) { NetworkSettingsState(model, session) }
    val name = model.selected?.title ?: "This device"
    LaunchedEffect(session) { network.refresh() }
    PageColumn {
        formSection("Enrollment") { EnrollmentSettings(model, name) }
        formSection {
            NavRow("Device info", Icons.Outlined.Info) { nav.push("Device info") { DeviceInfoPage(model, name) } }
            val wifiValue = network.snapshot?.let { if (it.wifiConnected) it.ssid else if (it.wifiError.isEmpty()) "Not connected" else "Unavailable" } ?: ""
            NavRow("Wi-Fi", Icons.Outlined.Wifi, wifiValue) { nav.push("Wi-Fi") { WifiPage(model, network, name) } }
            NavRow("Bluetooth", Icons.Outlined.SettingsInputAntenna) { nav.push("Bluetooth") { BluetoothPage(model, name) } }
            NavRow("Audio", Icons.AutoMirrored.Outlined.VolumeUp) { nav.push("Audio") { AudioPage(model, name) } }
            NavRow("Wendy Mesh", Icons.Outlined.Hub, network.snapshot?.meshState?.replaceFirstChar { it.uppercase() } ?: "") { nav.push("Wendy Mesh") { MeshPage(model, network, name) } }
            NavRow("Notifications", Icons.Outlined.NotificationsActive) { nav.push("Notifications") { NotificationsPage(model) } }
        }
        formSection {
            NavRow("Pairing", Icons.Outlined.Sensors) { nav.push("Pairing") { PairingPage(model, name) } }
            NavRow("Logs", Icons.AutoMirrored.Outlined.Article) { nav.push("Logs") { DeviceLogsPage(model) } }
        }
        if (network.error != null && network.snapshot == null) item { FailureView("Could not load settings", network.error!!) { scope.launch { network.refresh() } } }
    }
}

// MARK: Enrollment

@Composable
fun EnrollmentSettings(model: WendyModel, deviceName: String) {
    val nav = LocalNavigator.current
    val session = model.session
    var snapshot by remember(session) { mutableStateOf<JSONObject?>(null) }
    var error by remember(session) { mutableStateOf<String?>(null) }
    var loading by remember(session) { mutableStateOf(false) }
    var revision by remember { mutableIntStateOf(0) }
    LaunchedEffect(session, revision, model.foregroundGeneration) {
        if (loading) return@LaunchedEffect
        loading = true
        try { snapshot = model.backend.call("DeviceEnrollment", session).obj(); error = null }
        catch (e: CancellationException) { throw e } catch (e: Exception) { snapshot = null; error = e.message } finally { loading = false }
    }
    val value = snapshot
    when {
        value != null -> {
            val enrolled = value.flag("Enrolled")
            val organization = value.text("OrganizationName").ifEmpty { value.text("OrganizationID") }
            val title = if (!enrolled) "Not enrolled in Wendy Cloud" else if (organization.isEmpty()) "Enrolled in Wendy Cloud" else "Enrolled in $organization"
            Row(Modifier.padding(vertical = 8.dp), horizontalArrangement = Arrangement.spacedBy(10.dp), verticalAlignment = Alignment.CenterVertically) {
                Icon(if (enrolled) Icons.Outlined.VerifiedUser else Icons.Outlined.WarningAmber, null, Modifier.size(20.dp), tint = if (enrolled) MaterialTheme.colorScheme.onSurfaceVariant else MaterialTheme.colorScheme.error)
                Text(title, style = MaterialTheme.typography.titleSmall)
            }
            if (!enrolled) {
                if (model.lite) {
                    SecondaryText("Enroll this Wendy Lite device over Bluetooth.")
                    ActionRow("Set up nearby device…", Icons.Outlined.Bluetooth) { model.requestNearbySetup = true }
                } else {
                    SecondaryText("Choose an organization to manage this device in Wendy Cloud.")
                    ActionRow("Enroll device…", Icons.Outlined.CloudUpload) {
                        nav.push("Enroll device") { EnrollmentPage(model, session, deviceName) { revision++ } }
                    }
                }
            }
        }
        loading -> LoadingRow("Checking enrollment…")
        else -> {
            Text("Enrollment status unavailable", Modifier.padding(vertical = 8.dp), style = MaterialTheme.typography.titleSmall)
            error?.let { SecondaryText(it) }
        }
    }
    if (!loading && value?.flag("Enrolled") != true) TextButton(onClick = { revision++ }) { Text("Check enrollment again", style = MaterialTheme.typography.bodySmall) }
}

@Composable
private fun EnrollmentPage(model: WendyModel, session: Long, deviceName: String, finished: () -> Unit) {
    val nav = LocalNavigator.current
    val scope = rememberCoroutineScope()
    var organizations by remember { mutableStateOf(emptyList<JSONObject>()) }
    var selected by remember { mutableStateOf("") }
    var name by remember { mutableStateOf(EnrollmentForm.suggestedName(deviceName)) }
    var loading by remember { mutableStateOf(false) }
    var enrolling by remember { mutableStateOf(false) }
    var completed by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    var canRetry by remember { mutableStateOf(true) }
    var notice by remember { mutableStateOf<String?>(null) }
    var wifi by remember { mutableStateOf<List<WifiNetwork>?>(null) }
    var resumeAfterWiFi by remember { mutableStateOf(false) }
    val trimmed = name.trim()
    val choice = organizations.firstOrNull { it.text("Account") == selected }
    suspend fun loadOrganizations() {
        loading = true; error = null
        try {
            organizations = model.backend.call("EnrollmentOrganizations").items()
            if (organizations.none { it.text("Account") == selected }) selected = EnrollmentForm.defaultOrganization(organizations, model.account)
        } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { loading = false }
    }
    suspend fun enroll(checkWiFi: Boolean) {
        val org = choice ?: return
        if (enrolling || !canRetry || !EnrollmentForm.validName(trimmed) || model.session != session) return
        enrolling = true; error = null
        try {
            if (checkWiFi) {
                val settings = NetworkSnapshot.parse(model.backend.call("NetworkSettings", session).obj())
                if (model.session != session) return
                if (settings.wifiAvailable && settings.wifiError.isEmpty() && !settings.wifiConnected) { wifi = settings.networks; return }
            }
            try {
                model.backend.call("EnrollDevice", session, org.text("Account"), org.text("ID"), trimmed)
                completed = true; finished()
            } catch (e: BackendFailure) {
                val message = e.message ?: ""
                // Unknown outcomes stay blocked; only a confirmed pre-mint refusal is safe to retry.
                canRetry = EnrollmentForm.canRetry(message)
                if (EnrollmentForm.needsWiFiRecovery(message)) {
                    notice = "Connect the device to the internet, then go back and check enrollment again. The device name is already reserved in Wendy Cloud."
                    wifi = emptyList()
                } else error = message
            }
        } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { enrolling = false }
    }
    LaunchedEffect(Unit) { loadOrganizations() }
    PageColumn {
        formSection {
            Text(deviceName, Modifier.padding(vertical = 8.dp), style = MaterialTheme.typography.titleMedium)
            if (completed) {
                Row(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically, modifier = Modifier.padding(vertical = 8.dp)) {
                    Icon(Icons.Outlined.CheckCircle, null, tint = MaterialTheme.colorScheme.primary)
                    Text("Enrolled in ${choice?.text("Name")?.ifEmpty { null } ?: "Wendy Cloud"}")
                }
                Button(onClick = { nav.pop() }, Modifier.fillMaxWidth()) { Text("Done") }
                return@formSection
            }
            SecondaryText("Enroll this device in an organization to access it through Wendy Cloud.")
            if (loading) LoadingRow("Loading organizations…")
            else if (organizations.isEmpty()) SecondaryText("Sign in to the organization you want to use.")
            else {
                ChoiceField("Organization", organizations.map { it.text("Account") to "${it.text("Name")} · ${it.text("Email").ifEmpty { it.text("Account") }}" }, selected, enabled = !enrolling) { selected = it }
                OutlinedTextField(name, { name = it }, Modifier.fillMaxWidth(), label = { Text("Device name") }, singleLine = true, enabled = !enrolling,
                    keyboardOptions = KeyboardOptions(autoCorrectEnabled = false, keyboardType = KeyboardType.Ascii))
                if (!EnrollmentForm.validName(trimmed)) SecondaryText("Use 1–63 lowercase letters, numbers or hyphens. Start with a letter and end with a letter or number.")
            }
            ActionRow(if (organizations.isEmpty()) "Sign in…" else "Sign in to another organization…", enabled = !enrolling) { nav.pop(); model.signIn() }
            ActionRow("Refresh organizations", enabled = !enrolling) { scope.launch { loadOrganizations() } }
            if (enrolling) LoadingRow("Preparing enrollment…")
            notice?.let { SecondaryText(it) }
            if (!canRetry && notice != null) ActionRow("Connect to Wi-Fi…") { wifi = emptyList() }
            error?.let { FailureNote(it) }
            if (!canRetry && notice == null) SecondaryText("Go back and check the device's enrollment status before trying again.")
            Button(onClick = { scope.launch { enroll(true) } }, Modifier.fillMaxWidth().padding(vertical = 8.dp),
                enabled = !loading && !enrolling && choice != null && EnrollmentForm.validName(trimmed) && canRetry && model.session == session) { Text("Enroll") }
        }
    }
    wifi?.let { networks ->
        WifiJoinDialog(model, session, deviceName, null, networks,
            explanation = if (canRetry) "Connect this device to the internet before enrolling in Wendy Cloud. If it already has internet through Ethernet or another connection, you can continue."
                else "The device could not reach Wendy Cloud. Connect it to Wi-Fi, then check its enrollment status. Its Cloud name has already been reserved.",
            skip = if (canRetry) { { resumeAfterWiFi = true; wifi = null } } else null,
            dismiss = { wifi = null; if (resumeAfterWiFi) { resumeAfterWiFi = false; scope.launch { enroll(false) } } }) { resumeAfterWiFi = canRetry }
    }
}

// MARK: Device info and agent updates

@Composable
private fun DeviceInfoPage(model: WendyModel, deviceName: String) {
    val session = model.session
    val scope = rememberCoroutineScope()
    var info by remember { mutableStateOf<JSONObject?>(null) }
    var update by remember { mutableStateOf<UpdateSnapshot?>(null) }
    var error by remember { mutableStateOf<String?>(null) }
    var updateError by remember { mutableStateOf<String?>(null) }
    var checking by remember { mutableStateOf(false) }
    var starting by remember { mutableStateOf(false) }
    var expected by remember { mutableStateOf("") }
    var verified by remember { mutableStateOf(false) }
    var waiting by remember { mutableStateOf(false) }
    suspend fun refresh() {
        if (checking) return
        checking = true; updateError = null
        try {
            try { info = model.backend.call("DeviceInfo", session).obj(); error = null } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message }
            try {
                val status = UpdateSnapshot.parse(model.backend.call("OnlineUpdate", session, 0, "").obj())
                if (status.busy) { expected = status.latest; update = status } else update = UpdateSnapshot.parse(model.backend.call("OnlineUpdate", session, 1, "").obj())
            } catch (e: CancellationException) { throw e } catch (e: Exception) { updateError = e.message }
        } finally { checking = false }
    }
    LaunchedEffect(session) { refresh() }
    // Follows a running update until the device reports the expected version.
    LaunchedEffect(update?.busy == true) {
        if (update?.busy != true) return@LaunchedEffect
        val deadline = System.currentTimeMillis() + 12 * 60_000
        try {
            while (System.currentTimeMillis() < deadline) {
                delay(2000)
                try {
                    val status = UpdateSnapshot.parse(model.backend.call("OnlineUpdate", session, 0, "").obj())
                    waiting = false
                    if (status.phase == "failed") { update = status; return@LaunchedEffect }
                    if (!status.busy) {
                        if (status.installed == expected) { verified = true; update = status; info = model.backend.call("DeviceInfo", session).obj() }
                        else { update = status; updateError = "The device is reachable, but the new version has not been verified. Check again." }
                        return@LaunchedEffect
                    }
                    update = status
                } catch (e: CancellationException) { throw e } catch (e: Exception) { waiting = true }
            }
            updateError = "The update has not been verified. Reopen Device info when the device reconnects."; update = null
        } finally { waiting = false }
    }
    val value = info
    PageColumn {
        formSection("Device") {
            LabeledRow("Name", deviceName)
            if (value != null) {
                LabeledRow("Model", value.text("board_model"), true)
                LabeledRow("Serial number", value.text("serial_number"), true)
                LabeledRow("Architecture", value.text("cpu_architecture"), true)
                if (value.number("cpu_count") > 0) LabeledRow("CPU cores", value.number("cpu_count").toString())
                if (value.number("mem_total_bytes") > 0) LabeledRow("Memory", formatBytes(value.number("mem_total_bytes")))
            } else if (error == null) LoadingRow("Loading device info…")
            error?.let { FailureNote(it) }
        }
        if (value != null) formSection("Software") {
            LabeledRow("Wendy Agent", value.text("version"), true)
            LabeledRow("Operating system", value.text("os"), true)
            LabeledRow("OS version", value.text("os_version"), true)
            LabeledRow("Kernel", value.text("kernel_version"), true)
            LabeledRow("JetPack", value.text("jetpack_version"), true)
            LabeledRow("CUDA", value.text("cuda_version"), true)
            LabeledRow("L4T", value.text("l4t_version"), true)
        }
        formSection("Agent update", "The device downloads and installs the update. Its connection will briefly restart.") {
            val current = update
            when {
                checking -> LoadingRow("Checking for updates…")
                starting -> LoadingRow("Starting update…")
                waiting || current?.busy == true -> {
                    LoadingRow(if (waiting) "Waiting for device to restart…" else when (current?.phase) { "downloading" -> "Downloading on device…"; "installing" -> "Installing on device…"; else -> "Restarting agent…" })
                    if (current != null && current.total > 0) LinearProgressIndicator(progress = { (current.downloaded.toFloat() / current.total).coerceIn(0f, 1f) }, Modifier.fillMaxWidth().padding(vertical = 6.dp))
                }
                verified -> StatusPill("Updated successfully")
                current != null && current.phase == "failed" -> FailureNote(current.detail)
                current != null && current.available -> {
                    StatusPill("Update available")
                    LabeledRow("Version", current.latest)
                    ActionRow("Update Wendy Agent", Icons.Outlined.SystemUpdate) {
                        if (starting) return@ActionRow
                        scope.launch {
                            starting = true; updateError = null; verified = false
                            try { expected = current.latest; update = UpdateSnapshot.parse(model.backend.call("OnlineUpdate", session, 2, expected).obj()) }
                            catch (e: CancellationException) { throw e } catch (e: Exception) { updateError = e.message } finally { starting = false }
                        }
                    }
                }
                current != null && current.phase == "current" -> {
                    StatusPill(if (current.installed == "dev" || current.installed.endsWith("-dev")) "Development build" else "Up to date")
                    LabeledRow("Latest release", current.latest)
                }
            }
            updateError?.let { FailureNote(it) }
            if (!checking && !starting && !waiting && update?.busy != true) ActionRow("Check again") { scope.launch { refresh() } }
        }
    }
}

// MARK: Wi-Fi

@Composable
private fun WifiPage(model: WendyModel, state: NetworkSettingsState, deviceName: String) {
    val scope = rememberCoroutineScope()
    var joining by remember { mutableStateOf<WifiNetwork?>(null) }
    var joinOther by remember { mutableStateOf(false) }
    var disconnecting by remember { mutableStateOf(false) }
    var forgetting by remember { mutableStateOf<WifiNetwork?>(null) }
    LaunchedEffect(state.session) { if (state.snapshot == null) state.refresh() }
    val snapshot = state.snapshot
    PageColumn {
        notices(state)
        if (snapshot == null) {
            if (state.error == null) item { LoadingRow("Loading settings…") }
            return@PageColumn
        }
        val available = snapshot.visibleNetworks()
        val saved = snapshot.savedOnly()
        formSection("Available networks", "Changes apply to $deviceName and may interrupt its connection to Wendy.") {
            if (snapshot.wifiError.isNotEmpty()) FailureNote(snapshot.wifiError)
            if (snapshot.scanError.isNotEmpty()) FailureNote(snapshot.scanError)
            if (available.isEmpty() && snapshot.scanError.isEmpty()) SecondaryText("No networks found")
            available.forEach { network ->
                NetworkRow(network, enabled = !state.saving, onClick = if (network.connected) null else ({ joining = network }),
                    menu = buildList {
                        if (network.connected) add(MenuItem("Disconnect", destructive = true) { disconnecting = true })
                        if (network.known) add(MenuItem("Forget network", destructive = true) { forgetting = network })
                    })
            }
            ActionRow("Join another network…", Icons.Outlined.Add, enabled = !state.saving) { joinOther = true }
        }
        if (saved.isNotEmpty() || snapshot.knownError.isNotEmpty()) formSection("Saved networks") {
            if (snapshot.knownError.isNotEmpty()) FailureNote(snapshot.knownError)
            saved.forEach { network -> NetworkRow(network, !state.saving, { joining = network }, listOf(MenuItem("Forget network", destructive = true) { forgetting = network })) }
        }
        item { NetworkTrafficSection(model, state.session) }
        item { TextButton(onClick = { scope.launch { state.refresh() } }, enabled = !state.loading && !state.saving) { Text("Scan for Wi-Fi networks") } }
    }
    val joined = { state.notice = "Wi-Fi connection requested. Checking the device’s connection…"; scope.launch { state.refresh() }; Unit }
    joining?.let { network -> WifiJoinDialog(model, state.session, deviceName, network, dismiss = { joining = null }, joined = joined) }
    if (joinOther) WifiJoinDialog(model, state.session, deviceName, null, dismiss = { joinOther = false }, joined = joined)
    if (disconnecting) ConfirmAction("Disconnect from Wi-Fi?", "$deviceName may become unreachable until it connects through another network.", "Disconnect", onDismiss = { disconnecting = false }) {
        disconnecting = false; scope.launch { state.perform { model.backend.call("DisconnectWiFi", state.session) } }
    }
    forgetting?.let { network ->
        ConfirmAction("Forget ${network.ssid}?", "The saved Wi-Fi profile will be removed from $deviceName.", "Forget network", onDismiss = { forgetting = null }) {
            forgetting = null; scope.launch { state.perform { model.backend.call("ForgetWiFi", state.session, network.ssid) } }
        }
    }
}

private fun androidx.compose.foundation.lazy.LazyListScope.notices(state: NetworkSettingsState) {
    state.error?.let { error -> item { FormSection { FailureNote(error) } } }
    state.notice?.let { notice -> item { FormSection { SecondaryText(notice) } } }
}

@Composable
private fun NetworkRow(network: WifiNetwork, enabled: Boolean, onClick: (() -> Unit)?, menu: List<MenuItem>) {
    Row(Modifier.fillMaxWidth().let { if (onClick != null) it.clickable(enabled = enabled, onClick = onClick) else it }.padding(vertical = 6.dp),
        verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
        Icon(if (network.connected) Icons.Outlined.Check else Icons.Outlined.Wifi, null, Modifier.size(22.dp),
            tint = if (network.connected) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurfaceVariant)
        Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(2.dp)) {
            Text(network.ssid, style = MaterialTheme.typography.bodyLarge)
            Text(network.securityLabel + if (network.connected) " · Connected" else if (network.known) " · Saved" else "", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        network.signal?.let { Text("$it%", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant) }
        if (menu.isNotEmpty()) OverflowMenu("Actions for ${network.ssid}", menu, enabled = enabled) else Spacer(Modifier.width(8.dp))
    }
}

@Composable
fun WifiJoinDialog(model: WendyModel, session: Long, deviceName: String, network: WifiNetwork?, networks: List<WifiNetwork> = emptyList(),
                   explanation: String? = null, skip: (() -> Unit)? = null, dismiss: () -> Unit, joined: () -> Unit) {
    val scope = rememberCoroutineScope()
    var selectedID by remember { mutableStateOf("") }
    val selectedNetwork = network ?: networks.firstOrNull { it.id == selectedID }
    var ssid by remember { mutableStateOf(network?.ssid ?: "") }
    var password by remember { mutableStateOf("") }
    var security by remember { mutableIntStateOf(network?.security ?: 4) }
    var hidden by remember { mutableStateOf(false) }
    var saving by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    DisposableEffect(Unit) { onDispose { password = "" } }
    AlertDialog(onDismissRequest = { if (!saving) { password = ""; dismiss() } }, title = { Text("Join Wi-Fi") }, text = {
        Column(Modifier.verticalScroll(rememberScrollState()), verticalArrangement = Arrangement.spacedBy(10.dp)) {
            explanation?.let { Text(it, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant) }
            if (network == null && networks.isNotEmpty()) ChoiceField("Network", listOf("" to "Other network…") + networks.map { it.id to it.ssid }, selectedID, enabled = !saving) { id ->
                selectedID = id; val chosen = networks.firstOrNull { it.id == id }
                ssid = chosen?.ssid ?: ""; security = chosen?.security ?: 4; password = ""; hidden = false
            }
            OutlinedTextField(ssid, { ssid = it }, Modifier.fillMaxWidth(), label = { Text("Network name") }, singleLine = true, enabled = selectedNetwork == null && !saving,
                keyboardOptions = KeyboardOptions(autoCorrectEnabled = false))
            if (selectedNetwork == null) {
                ChoiceField("Security", listOf("1" to "Open", "4" to "WPA2", "5" to "WPA3"), security.toString(), enabled = !saving) { security = it.toInt() }
                ToggleRow("Hidden network", hidden, enabled = !saving) { hidden = it }
            } else LabeledRow("Security", selectedNetwork.securityLabel)
            if (security != 1 && security != 6) OutlinedTextField(password, { password = it }, Modifier.fillMaxWidth(), singleLine = true, enabled = !saving,
                label = { Text(if (selectedNetwork?.known == true) "Password (leave blank to use saved)" else "Password") },
                visualTransformation = PasswordVisualTransformation(), keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password))
            if (security == 6) SecondaryText("Enterprise authentication is not supported by this connection form.")
            SecondaryText("Connects $deviceName to this network. The device may disconnect from Wendy while switching networks.")
            error?.let { FailureNote(it) }
            if (saving) LoadingRow("Connecting…")
            if (skip != null) TextButton(onClick = { password = ""; skip() }, enabled = !saving) { Text("Continue without Wi-Fi") }
        }
    }, confirmButton = {
        TextButton(enabled = !saving && ssid.isNotBlank() && security != 6, onClick = {
            saving = true; error = null
            scope.launch {
                try { model.backend.call("JoinWiFi", session, ssid, password, security, hidden); password = ""; joined(); dismiss() }
                catch (e: CancellationException) { throw e } catch (e: Exception) { password = ""; error = e.message } finally { saving = false }
            }
        }) { Text("Join") }
    }, dismissButton = { TextButton(onClick = { password = ""; dismiss() }, enabled = !saving) { Text("Cancel") } })
}

/** A labelled dropdown for a fixed set of choices. */
@Composable
fun ChoiceField(label: String, choices: List<Pair<String, String>>, selected: String, enabled: Boolean = true, onSelect: (String) -> Unit) {
    var expanded by remember { mutableStateOf(false) }
    Column(Modifier.padding(vertical = 4.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
        Text(label, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
        Box {
            OutlinedButton(onClick = { expanded = true }, enabled = enabled && choices.isNotEmpty(), modifier = Modifier.fillMaxWidth()) {
                Text(choices.firstOrNull { it.first == selected }?.second ?: if (choices.isEmpty()) "No choices available" else "Choose ${label.lowercase()}", Modifier.weight(1f))
                Icon(Icons.Outlined.ExpandMore, null, Modifier.size(18.dp))
            }
            DropdownMenu(expanded, onDismissRequest = { expanded = false }) {
                choices.forEach { (id, title) -> DropdownMenuItem(text = { Text(title) }, onClick = { expanded = false; onSelect(id) },
                    trailingIcon = if (id == selected) ({ Icon(Icons.Outlined.Check, null) }) else null) }
            }
        }
    }
}

@Composable
private fun NetworkTrafficSection(model: WendyModel, session: Long) {
    var snapshot by remember(session) { mutableStateOf<TrafficSnapshot?>(null) }
    var error by remember(session) { mutableStateOf<String?>(null) }
    var updated by remember(session) { mutableStateOf<Long?>(null) }
    var virtualOpen by remember { mutableStateOf(false) }
    LaunchedEffect(session, model.foregroundGeneration) {
        while (true) {
            try { snapshot = TrafficSnapshot(model.backend.call("NetworkTraffic", session).obj()); updated = System.currentTimeMillis(); error = null }
            catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message }
            delay(2000)
        }
    }
    val footer = "Received and sent from the device’s perspective. Interface totals include all apps and reset when the interface resets or the device restarts.\n" +
        "Wendy agent service counts IP traffic, including its helpers, relayed traffic and loopback, since accounting began. App containers are excluded. Viewing statistics also generates traffic. This is not a cellular or internet usage meter." +
        (updated?.let { "\nUpdated ${DateFormat.getTimeInstance(DateFormat.MEDIUM).format(Date(it))}" } ?: "")
    FormSection("Network traffic", footer) {
        val value = snapshot
        if (value != null) {
            value.rows.firstOrNull { it.agent }?.let { TrafficRowView(it) } ?: LabeledRow("Wendy agent service", "Unavailable")
            value.rows.filter { !it.virtual && !it.loopback && !it.agent }.forEach { TrafficRowView(it) }
            SecondaryText(value.interfacesError)
            val virtual = value.rows.filter { it.virtual || it.loopback }
            if (virtual.isNotEmpty()) {
                ActionRow(if (virtualOpen) "Hide virtual and loopback interfaces" else "Virtual and loopback interfaces") { virtualOpen = !virtualOpen }
                if (virtualOpen) {
                    virtual.forEach { TrafficRowView(it) }
                    SecondaryText("These may count the same traffic as a physical interface. Do not add them together.")
                }
            }
        } else if (error == null) LoadingRow("Reading network traffic…")
        error?.let {
            FailureNote(it)
            if (snapshot != null) SecondaryText("Showing the last received sample.")
        }
    }
}

@Composable
private fun TrafficRowView(row: TrafficRow) {
    Column(Modifier.padding(vertical = 6.dp)) {
        Text(row.name.ifEmpty { if (row.agent) "Wendy agent service" else "Interface" }, style = MaterialTheme.typography.titleSmall)
        LabeledRow("Receiving", row.receiveRate?.let { binarySize(it) + "/s" } ?: "Measuring…")
        LabeledRow("Sending", row.sendRate?.let { binarySize(it) + "/s" } ?: "Measuring…")
        LabeledRow("Total received", binarySize(row.received.toDouble()))
        LabeledRow("Total sent", binarySize(row.sent.toDouble()))
    }
}

// MARK: Wendy Mesh

@Composable
private fun MeshPage(model: WendyModel, state: NetworkSettingsState, deviceName: String) {
    val nav = LocalNavigator.current
    val scope = rememberCoroutineScope()
    LaunchedEffect(state.session) { if (state.snapshot == null) state.refresh() }
    val snapshot = state.snapshot
    fun bit(index: Int) = state.draft and (1 shl index) != 0
    fun set(index: Int, value: Boolean) { state.draft = if (value) state.draft or (1 shl index) else state.draft and (1 shl index).inv() }
    PageColumn {
        notices(state)
        if (snapshot == null) {
            if (state.error == null) item { LoadingRow("Loading settings…") }
            return@PageColumn
        }
        formSection {
            ActionRow("Show mesh graph", Icons.Outlined.Hub) { nav.push("Mesh graph") { MeshGraphPage(snapshot.topology, deviceName) } }
            LabeledRow("Runtime", if (snapshot.runtimeAvailable) snapshot.meshState.ifEmpty { "Available" }.replaceFirstChar { it.uppercase() } else "Unavailable")
            if (snapshot.runtimeAvailable) LabeledRow("Authenticated links", snapshot.peers.toString())
            LabeledRow("Internet gateway", snapshot.gateway)
            SecondaryText(snapshot.meshDetail)
            if (snapshot.meshError.isNotEmpty()) FailureNote(snapshot.meshError)
        }
        if (snapshot.meshAvailable) {
            formSection(footer = "Participation lets this device join and relay the mesh. Internet sharing offers its own uplink to other mesh devices.") {
                ToggleRow("Participate in mesh", bit(0), enabled = !state.saving) { set(0, it) }
                ToggleRow("Use mesh internet when offline", bit(1), enabled = !state.saving && state.participates) { set(1, it) }
                ToggleRow("Share internet connection", bit(2), enabled = !state.saving && state.participates) { set(2, it) }
            }
            formSection("Transports") {
                listOf("Ethernet" to 5, "Wi-Fi network" to 6, "Wi-Fi Aware" to 3, "Bluetooth" to 4).forEach { (title, index) ->
                    ToggleRow(title, bit(index), enabled = !state.saving && state.participates) { set(index, it) }
                }
            }
            if (state.dirty || state.saving) formSection {
                ActionRow("Save changes", enabled = !state.saving && !state.loading) { scope.launch { state.saveMesh() } }
                ActionRow("Discard changes", enabled = !state.saving) { state.draft = snapshot.meshConfig }
                if (state.saving) LoadingRow("Saving…")
            }
        }
    }
}

@Composable
private fun MeshGraphPage(topology: MeshTopology, deviceName: String) {
    val graph = remember(topology) { MeshGraph(topology) }
    var selected by remember { mutableStateOf<String?>(null) }
    var scale by remember { mutableFloatStateOf(1f) }
    var offset by remember { mutableStateOf(Offset.Zero) }
    val transform = rememberTransformableState { zoom, pan, _ -> scale = (scale * zoom).coerceIn(0.5f, 8f); offset += pan }
    Column(Modifier.fillMaxSize()) {
        when {
            topology.error.isNotEmpty() -> EmptyMessage("Mesh graph unavailable", topology.error, Icons.Outlined.PortableWifiOff)
            !topology.available -> EmptyMessage("Mesh graph unavailable", "This device has not returned a topology snapshot.", Icons.Outlined.PortableWifiOff)
            else -> {
                val primary = MaterialTheme.colorScheme.primary
                val muted = MaterialTheme.colorScheme.onSurfaceVariant
                val selectedColor = MaterialTheme.colorScheme.tertiary
                Box(Modifier.weight(1f).fillMaxWidth().transformable(transform)) {
                    Canvas(Modifier.fillMaxSize().graphicsLayer(scaleX = scale, scaleY = scale, translationX = offset.x, translationY = offset.y)) {
                        val columns = listOf(size.width * 0.12f, size.width * 0.5f, size.width * 0.88f)
                        val positions = mutableMapOf(topology.principal to Offset(columns[0], size.height / 2))
                        var row = 0
                        val rows = graph.direct.sumOf { peer -> maxOf(1, graph.indirect.count { it.nextHop == peer.id }) } + graph.unrouted.size
                        val step = size.height / (rows + 1).coerceAtLeast(2)
                        graph.direct.forEach { peer ->
                            val children = graph.indirect.filter { it.nextHop == peer.id }
                            val count = maxOf(1, children.size)
                            positions[peer.id] = Offset(columns[1], step * (row + 1) + step * (count - 1) / 2)
                            children.forEachIndexed { i, child -> positions[child.id] = Offset(columns[2], step * (row + i + 1)) }
                            row += count
                        }
                        graph.unrouted.forEach { positions[it.id] = Offset(columns[2], step * (row + 1)); row++ }
                        val center = positions.getValue(topology.principal)
                        graph.direct.forEach { drawLine(primary, center, positions.getValue(it.id), 3f) }
                        graph.indirect.forEach { node -> positions[node.nextHop]?.let { hop -> drawLine(muted, hop, positions.getValue(node.id), 2f, pathEffect = PathEffect.dashPathEffect(floatArrayOf(12f, 10f))) } }
                        drawCircle(primary, 22f, center)
                        graph.nodes.forEach { node -> drawCircle(if (node.id == selected) selectedColor else if (node.direct) primary else muted, 16f, positions.getValue(node.id)) }
                    }
                }
                Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                    Text("${graph.direct.size} direct · ${graph.indirect.size} indirect · $deviceName", style = MaterialTheme.typography.titleSmall)
                    SecondaryText("Solid: direct link · Dashed: route via peer, possibly multiple hops")
                    if (graph.unrouted.isNotEmpty()) SecondaryText("Devices without a usable route: ${graph.unrouted.size}")
                    SecondaryText("Pinch to zoom · Tap a device below for details")
                    if (topology.capturedMs > 0) SecondaryText("Snapshot ${DateFormat.getTimeInstance(DateFormat.MEDIUM).format(Date(topology.capturedMs))}")
                    if (topology.truncated) Text("The device returned a partial topology.", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error)
                }
            }
        }
        if (topology.available && topology.error.isEmpty()) Column(Modifier.heightIn(max = 280.dp).verticalScroll(rememberScrollState()).padding(horizontal = 16.dp)) {
            graph.nodes.forEach { node ->
                Column(Modifier.fillMaxWidth().clickable { selected = if (selected == node.id) null else node.id }.padding(vertical = 8.dp)) {
                    Text(node.title, style = MaterialTheme.typography.titleSmall)
                    SecondaryText(listOf(node.subtitle, node.address).filter { it.isNotEmpty() }.joinToString(" · "))
                    if (selected == node.id) SecondaryText(node.id)
                }
                HorizontalDivider(color = MaterialTheme.colorScheme.outlineVariant)
            }
        }
    }
}

// MARK: Bluetooth and audio

@Composable
private fun BluetoothPage(model: WendyModel, deviceName: String) {
    val session = model.session
    val scope = rememberCoroutineScope()
    var devices by remember { mutableStateOf(emptyList<JSONObject>()) }
    var scanning by remember { mutableStateOf(false) }
    var busy by remember { mutableStateOf<String?>(null) }
    var error by remember { mutableStateOf<String?>(null) }
    var forgetting by remember { mutableStateOf<JSONObject?>(null) }
    suspend fun scan() {
        if (scanning || busy != null) return
        scanning = true; error = null
        try { devices = model.backend.call("BluetoothScan", session).items() } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { scanning = false }
    }
    fun act(device: JSONObject, action: Int) = scope.launch {
        busy = device.text("address"); error = null
        try { model.backend.call("BluetoothAction", session, device.text("address"), action); busy = null; scan() }
        catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message; busy = null }
    }
    LaunchedEffect(session) { scan() }
    PageColumn {
        error?.let { item { FormSection { FailureNote(it) } } }
        formSection("Nearby devices", "Bluetooth devices near $deviceName. Put your accessory in discovery mode to find it.") {
            devices.forEach { device ->
                val title = device.text("name").ifEmpty { device.text("address") }
                Row(Modifier.fillMaxWidth().padding(vertical = 6.dp), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                    Icon(Icons.Outlined.SettingsInputAntenna, null, Modifier.size(22.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
                    Column(Modifier.weight(1f)) {
                        Text(title, style = MaterialTheme.typography.titleSmall)
                        Text(if (device.flag("connected")) "Connected" else if (device.flag("paired")) "Paired" else "Not paired", style = MaterialTheme.typography.bodySmall,
                            color = if (device.flag("connected")) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurfaceVariant)
                        if (device.text("name").isNotEmpty()) Text(device.text("address"), style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                    if (busy == device.text("address")) CircularProgressIndicator(Modifier.size(20.dp), strokeWidth = 2.dp)
                    else OverflowMenu("Options for $title", buildList {
                        if (device.flag("connected")) add(MenuItem("Disconnect") { act(device, 2) })
                        else {
                            add(MenuItem("Connect") { act(device, 0) })
                            if (!device.flag("paired")) add(MenuItem("Pair and connect") { act(device, 1) })
                        }
                        if (device.flag("paired")) add(MenuItem("Forget device", destructive = true) { forgetting = device })
                    }, enabled = !scanning && busy == null)
                }
            }
            if (devices.isEmpty() && !scanning && error == null) SecondaryText("No Bluetooth devices found.")
            if (scanning) LoadingRow("Scanning…")
            ActionRow("Scan again", enabled = !scanning && busy == null) { scope.launch { scan() } }
        }
    }
    forgetting?.let { device ->
        ConfirmAction("Forget ${device.text("name").ifEmpty { device.text("address") }}?", "$deviceName will remove this Bluetooth pairing.", "Forget device", onDismiss = { forgetting = null }) { forgetting = null; act(device, 3) }
    }
}

@Composable
private fun AudioPage(model: WendyModel, deviceName: String) {
    val session = model.session
    val scope = rememberCoroutineScope()
    var devices by remember { mutableStateOf(emptyList<JSONObject>()) }
    var loading by remember { mutableStateOf(true) }
    var saving by remember { mutableStateOf<Long?>(null) }
    var error by remember { mutableStateOf<String?>(null) }
    suspend fun refresh() {
        loading = true; error = null
        try { devices = model.backend.call("AudioDevices", session).items() } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { loading = false }
    }
    LaunchedEffect(session) { refresh() }
    PageColumn {
        error?.let { item { FormSection { FailureNote(it) } } }
        if (loading && devices.isEmpty()) { item { LoadingRow("Loading audio devices…") }; return@PageColumn }
        listOf(Triple("Default microphone", 1L, Icons.Outlined.Mic), Triple("Default speaker", 2L, Icons.AutoMirrored.Outlined.VolumeUp)).forEach { (title, direction, icon) ->
            formSection(title) {
                val choices = devices.filter { it.number("type") == direction }
                if (choices.isEmpty()) SecondaryText(if (direction == 1L) "No microphones found" else "No speakers found")
                choices.forEach { device ->
                    val id = device.number("device_id")
                    val label = device.text("description").ifEmpty { device.text("name") }
                    Row(Modifier.fillMaxWidth().clickable(enabled = !loading && saving == null && !device.flag("is_default")) {
                        scope.launch {
                            saving = id; error = null
                            try {
                                model.backend.call("SetDefaultAudio", session, id, direction)
                                refresh()
                                if (error == null && devices.none { it.number("device_id") == id && it.number("type") == direction && it.flag("is_default") })
                                    error = "The change was accepted, but the device has not reported this default yet. Refresh to check again."
                            } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { saving = null }
                        }
                    }.padding(vertical = 8.dp), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                        Icon(icon, null, Modifier.size(22.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
                        Column(Modifier.weight(1f)) {
                            Text(label, style = MaterialTheme.typography.bodyLarge)
                            if (device.text("name").isNotEmpty() && device.text("name") != label) Text(device.text("name"), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 2)
                        }
                        if (saving == id) CircularProgressIndicator(Modifier.size(20.dp), strokeWidth = 2.dp)
                        else if (device.flag("is_default")) Icon(Icons.Outlined.Check, "Default", tint = MaterialTheme.colorScheme.primary)
                    }
                }
            }
        }
        item { SecondaryText("These defaults apply to $deviceName. Apps with an explicit audio device keep their own selection.") }
        item { TextButton(onClick = { scope.launch { refresh() } }, enabled = !loading && saving == null) { Text("Refresh audio devices") } }
    }
}

// MARK: Notifications

@Composable
private fun NotificationsPage(model: WendyModel) {
    val session = model.session
    val scope = rememberCoroutineScope()
    var permissions by remember { mutableStateOf(emptyList<JSONObject>()) }
    var loading by remember { mutableStateOf(false) }
    var loaded by remember { mutableStateOf(false) }
    var saving by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    var pending by remember { mutableStateOf<Pair<JSONObject, Boolean>?>(null) }
    suspend fun refresh() {
        if (loading || saving) return
        loading = true
        try { permissions = model.backend.call("CampaignNotifications", session).items(); error = null }
        catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { loading = false; loaded = true }
    }
    LaunchedEffect(session) { refresh() }
    PageColumn {
        formSection("Receive notifications") {
            // Cloud registers push tokens for APNs only. Android needs an FCM contract first.
            Text("Push notifications are not available on Android yet.", Modifier.padding(vertical = 8.dp), style = MaterialTheme.typography.bodyMedium)
            SecondaryText("Wendy Cloud can only deliver campaign alerts to Apple devices today. You can still choose which campaigns send alerts.")
        }
        if ((!loaded || loading) && permissions.isEmpty() && error == null) formSection("Campaign notifications") { LoadingRow("Loading permissions…") }
        else if (permissions.isEmpty() && error == null) formSection("Campaign notifications") { SecondaryText("Create a campaign in chat to choose which alerts to send.") }
        error?.let { formSection { FailureNote(it); ActionRow("Try again", enabled = !loading && !saving) { scope.launch { refresh() } } } }
        if (permissions.isNotEmpty()) formSection("Campaign notifications", "Cloud permissions apply across devices using the same campaign in this organization. Changing them requires an owner or admin. Recordings are not uploaded by this permission.") {
            permissions.forEach { permission ->
                val subtitle = if (!permission.flag("Registered")) "Cloud registration will be created after approval" else if (!permission.flag("RequestsNotifications")) "Notifications are off in the campaign’s settings" else ""
                ToggleRow(permission.text("Name"), permission.flag("Enabled"), enabled = !loading && !saving && (permission.flag("RequestsNotifications") || permission.flag("Enabled")), subtitle = subtitle) { pending = permission to it }
            }
            if (saving) LoadingRow("Saving permission…")
        }
    }
    pending?.let { (campaign, enabled) ->
        ConfirmAction(if (enabled) "Allow notifications?" else "Turn off notifications?",
            "This changes the Cloud notification permission for “${campaign.text("Name")}” across all devices using that campaign in this organization.",
            if (enabled) "Allow notifications" else "Turn off", destructive = !enabled, onDismiss = { pending = null }) {
            pending = null
            scope.launch {
                saving = true; error = null
                try {
                    model.backend.call("SetCampaignNotifications", session, campaign.text("Name"), enabled)
                    permissions = permissions.map { if (it.text("Name") == campaign.text("Name")) JSONObject(it.toString()).put("Enabled", enabled) else it }
                } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { saving = false }
            }
        }
    }
}

// MARK: Pairing

@Composable
private fun PairingPage(model: WendyModel, deviceName: String) {
    var kind by remember { mutableStateOf("Wendy Lite") }
    Column(Modifier.fillMaxSize()) {
        ChoiceTabs(listOf("Wendy Lite", "IP cameras", "Robots"), kind, { kind = it }, Modifier.padding(horizontal = 20.dp, vertical = 8.dp))
        key(kind) { if (kind == "Robots") RobotPairingScreen(model) else SourcePairingList(model, deviceName, kind == "IP cameras") }
    }
}

@Composable
private fun SourcePairingList(model: WendyModel, deviceName: String, cameras: Boolean) {
    val session = model.session
    val scope = rememberCoroutineScope()
    var devices by remember { mutableStateOf(emptyList<JSONObject>()) }
    var warning by remember { mutableStateOf("") }
    var loading by remember { mutableStateOf(false) }
    var busy by remember { mutableStateOf<String?>(null) }
    var error by remember { mutableStateOf<String?>(null) }
    var login by remember { mutableStateOf<JSONObject?>(null) }
    var forgetting by remember { mutableStateOf<JSONObject?>(null) }
    suspend fun refresh() {
        loading = true; error = null
        try { val result = model.backend.call("Pairings", session, cameras).obj(); devices = result.list("Items"); warning = result.text("Warning") }
        catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { loading = false }
    }
    fun change(item: JSONObject, work: suspend () -> Unit) = scope.launch {
        busy = item.text("ID"); error = null
        try { work(); refresh() } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { busy = null }
    }
    LaunchedEffect(session) { refresh() }
    PageColumn {
        if (warning.isNotEmpty()) item { FormSection { Row(Modifier.padding(vertical = 8.dp), horizontalArrangement = Arrangement.spacedBy(8.dp)) { Icon(Icons.Outlined.WarningAmber, null, tint = MaterialTheme.colorScheme.error); Text(warning, style = MaterialTheme.typography.bodyMedium) } } }
        error?.let { item { FormSection { FailureNote(it) } } }
        if (devices.isEmpty() && error == null && warning.isEmpty()) {
            item { if (loading) LoadingRow("Finding sources…") else EmptyMessage("No devices found", "Connect a source to the same network, then scan again.", if (cameras) Icons.Outlined.Videocam else Icons.Outlined.Sensors) }
        } else formSection(footer = if (devices.isNotEmpty()) "Sources on $deviceName’s network." else null) {
            devices.forEach { item ->
                Row(Modifier.fillMaxWidth().padding(vertical = 8.dp), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                    Icon(if (cameras) Icons.Outlined.Videocam else Icons.Outlined.Sensors, null, Modifier.size(22.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
                    Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(2.dp)) {
                        Text(item.text("Name", "ID"), style = MaterialTheme.typography.titleSmall)
                        SecondaryText(item.text("Address"))
                        if (!item.flag("CanPair") && !item.flag("Paired")) SecondaryText(item.text("Detail"))
                        else Text(if (item.flag("Paired")) (if (item.flag("Connected")) "Connected" else "Paired") else "Not paired", style = MaterialTheme.typography.bodySmall,
                            color = if (item.flag("Connected")) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                    when {
                        busy == item.text("ID") -> CircularProgressIndicator(Modifier.size(20.dp), strokeWidth = 2.dp)
                        item.flag("Paired") -> OverflowMenu("Options for ${item.text("Name")}", buildList {
                            if (cameras) add(MenuItem("Edit login", Icons.Outlined.Key) { login = item })
                            add(MenuItem("Forget pairing", Icons.Outlined.Delete, destructive = true) { forgetting = item })
                        }, enabled = busy == null && !loading)
                        item.flag("CanPair") -> OutlinedButton(onClick = { if (cameras) login = item else change(item) { model.backend.call("PairSensor", session, item.text("ID"), false) } }, enabled = busy == null && !loading) { Text("Pair") }
                    }
                }
            }
        }
        item { TextButton(onClick = { scope.launch { refresh() } }, enabled = !loading && busy == null) { Text("Scan again") } }
    }
    login?.let { item -> CameraLoginDialog(model, session, item, deviceName, { login = null }) { scope.launch { refresh() } } }
    forgetting?.let { item ->
        ConfirmAction("Forget pairing?", "Remove ${item.text("Name")} from $deviceName? Its sensors will no longer be available to apps on this device.", "Forget", onDismiss = { forgetting = null }) {
            forgetting = null
            change(item) { if (cameras) model.backend.call("PairIPCamera", session, item.number("CameraID"), "", "", true) else model.backend.call("PairSensor", session, item.text("ID"), true) }
        }
    }
}

@Composable
private fun CameraLoginDialog(model: WendyModel, session: Long, item: JSONObject, deviceName: String, dismiss: () -> Unit, saved: () -> Unit) {
    val scope = rememberCoroutineScope()
    var username by remember { mutableStateOf("admin") }
    var password by remember { mutableStateOf("") }
    var saving by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    DisposableEffect(Unit) { onDispose { password = "" } }
    AlertDialog(onDismissRequest = { if (!saving) { password = ""; dismiss() } }, title = { Text("Camera login") }, text = {
        Column(verticalArrangement = Arrangement.spacedBy(10.dp)) {
            Text(item.text("Name"), style = MaterialTheme.typography.titleSmall)
            OutlinedTextField(username, { username = it }, Modifier.fillMaxWidth(), label = { Text("Username") }, singleLine = true, keyboardOptions = KeyboardOptions(autoCorrectEnabled = false))
            OutlinedTextField(password, { password = it }, Modifier.fillMaxWidth(), label = { Text("Password") }, singleLine = true,
                visualTransformation = PasswordVisualTransformation(), keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password))
            SecondaryText("The camera login is saved on $deviceName.")
            error?.let { FailureNote(it) }
            if (saving) LoadingRow("Saving…")
        }
    }, confirmButton = {
        TextButton(enabled = !saving && username.isNotBlank(), onClick = {
            saving = true
            scope.launch {
                try { model.backend.call("PairIPCamera", session, item.number("CameraID"), username, password, false); password = ""; saved(); dismiss() }
                catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message; password = "" } finally { saving = false }
            }
        }) { Text("Save") }
    }, dismissButton = { TextButton(onClick = { password = ""; dismiss() }, enabled = !saving) { Text("Cancel") } })
}
