package dev.wendy.android

import android.Manifest
import android.content.Context
import android.content.pm.PackageManager
import android.os.Build
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.material.icons.automirrored.outlined.ArrowForward
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.NonCancellable
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import org.json.JSONObject

data class SetupDevice(val address: String, val name: String, val rssi: Int) {
    val title get() = if (name.isEmpty()) "Wendy device" else DeviceDisplayName.title(name)
    companion object { fun parse(value: JSONObject) = SetupDevice(value.text("Address"), value.text("Name"), value.number("RSSI").toInt()) }
}

data class SetupSnapshot(
    val session: Long = 0, val connected: Boolean = false, val lite: Boolean = false, val enrolled: Boolean = false, val attempted: Boolean = false,
    val pending: Boolean = false, val canResume: Boolean = false, val failure: String = "", val completed: Boolean = false, val wifiConnected: Boolean = false,
    val ssid: String = "", val deviceID: String = "", val networks: List<String> = emptyList(), val error: String? = null,
) {
    enum class Observation { Discover, Reconnect, CheckEnrollment, Status, ScanWiFi, Idle }
    /** What the background loop should do next (iOS NearbySetupSnapshot.observation). */
    fun observation(hasSelection: Boolean, wifiExpanded: Boolean, statusAgeSeconds: Double, scanAgeSeconds: Double): Observation {
        if (completed) return Observation.Idle
        if (pending || attempted) {
            // Keep setup usable while Cloud is pending: an offline board must still accept Wi-Fi without re-enrollment.
            if (!enrolled) {
                if ((wifiExpanded || canResume) && !connected && statusAgeSeconds >= 25) return Observation.Reconnect
                if (connected && statusAgeSeconds >= 25) return Observation.Status
                if (connected && wifiExpanded && scanAgeSeconds >= 20) return Observation.ScanWiFi
            }
            return if (pending) Observation.CheckEnrollment else Observation.Idle
        }
        if (enrolled || attempted) return Observation.Idle
        if (!hasSelection) return Observation.Discover
        if (!connected) return Observation.Reconnect
        if (statusAgeSeconds >= 25) return Observation.Status
        if (wifiExpanded && scanAgeSeconds >= 20) return Observation.ScanWiFi
        return Observation.Idle
    }
    companion object {
        /** Reads the bridge result; a failed step still carries its state. */
        fun parse(result: JSONObject): SetupSnapshot {
            val s = result.child("State")
            val networks = s.optJSONArray("Networks")?.let { a -> (0 until a.length()).map(a::optString) } ?: emptyList()
            return SetupSnapshot(s.number("Session"), s.flag("Connected"), s.flag("Lite"), s.flag("Enrolled"), s.flag("Attempted"), s.flag("Pending"), s.flag("CanResume"),
                s.text("Failure"), s.flag("Completed"), s.flag("WiFiConnected"), s.text("SSID"), s.text("DeviceID"), networks, result.text("Error").ifEmpty { null })
        }
    }
}

fun bluetoothPermissions() = if (Build.VERSION.SDK_INT >= 31) arrayOf(Manifest.permission.BLUETOOTH_SCAN, Manifest.permission.BLUETOOTH_CONNECT) else arrayOf(Manifest.permission.ACCESS_FINE_LOCATION)
fun hasBluetoothPermission(context: Context) = bluetoothPermissions().all { context.checkSelfPermission(it) == PackageManager.PERMISSION_GRANTED }

private object SetupAction { const val Status = 0; const val ScanWiFi = 1; const val ConnectWiFi = 2; const val Enroll = 3; const val CheckEnrollment = 4 }

/** Bluetooth setup for a nearby device (iOS NearbySetupView). Discovery and status checks run on their own. */
@Composable
fun NearbySetupScreen(model: WendyModel, initial: SetupDevice? = null, done: () -> Unit) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    var permitted by remember { mutableStateOf(hasBluetoothPermission(context)) }
    val permissions = rememberLauncherForActivityResult(ActivityResultContracts.RequestMultiplePermissions()) { result -> permitted = result.values.all { it } }
    var devices by remember { mutableStateOf(emptyList<SetupDevice>()) }
    var selected by remember { mutableStateOf(initial) }
    var state by remember { mutableStateOf(SetupSnapshot()) }
    var organizations by remember { mutableStateOf(emptyList<JSONObject>()) }
    var organization by remember { mutableStateOf("") }
    var organizationsLoaded by remember { mutableStateOf(false) }
    var name by remember { mutableStateOf(initial?.let { EnrollmentForm.suggestedName(it.title) } ?: "") }
    var ssid by remember { mutableStateOf("") }
    var password by remember { mutableStateOf("") }
    var wifiExpanded by remember { mutableStateOf(false) }
    var customizing by remember { mutableStateOf(false) }
    var busy by remember { mutableStateOf(false) }
    var automatic by remember { mutableStateOf(false) }
    var opening by remember { mutableStateOf(false) }
    var progress by remember { mutableStateOf("") }
    var error by remember { mutableStateOf<String?>(null) }
    var observationError by remember { mutableStateOf<String?>(null) }
    var lastStatus by remember { mutableLongStateOf(0L) }
    var lastScan by remember { mutableLongStateOf(0L) }
    var nextReconnect by remember { mutableLongStateOf(0L) }
    var reconnectDelay by remember { mutableLongStateOf(3_000L) }
    val editingLocked = busy && !automatic
    val choice = organizations.firstOrNull { it.text("Account") == organization }
    val nameValid = EnrollmentForm.validName(name)
    val finished = state.completed || (state.enrolled && !state.attempted)
    val stage = when {
        finished -> "done"
        state.failure.isNotEmpty() && !editingLocked -> "attention"
        state.pending || state.attempted || editingLocked -> "working"
        selected == null -> "discovering"
        state.connected && organizationsLoaded -> "ready"
        else -> "connecting"
    }
    suspend fun waitIdle() { while (busy) delay(100) }
    suspend fun connect(device: SetupDevice) {
        if (busy) return
        busy = true; automatic = true; selected = device
        try {
            val value = SetupSnapshot.parse(model.backend.call("NearbySetupOpen", device.address).obj())
            state = value; observationError = value.error; lastStatus = System.currentTimeMillis()
        } catch (e: CancellationException) { throw e } catch (e: Exception) { observationError = e.message } finally { busy = false; automatic = false }
        nextReconnect = System.currentTimeMillis() + reconnectDelay
        reconnectDelay = if (state.connected) 3_000 else minOf(reconnectDelay * 2, 30_000)
    }
    suspend fun perform(action: Int, automatically: Boolean = false) {
        if (!automatically) waitIdle()
        if (busy || state.session == 0L) return
        busy = true; automatic = automatically
        if (!automatically) error = null
        progress = when (action) { SetupAction.Status -> "Checking connection…"; SetupAction.ScanWiFi -> "Finding networks…"; SetupAction.ConnectWiFi -> "Connecting to Wi-Fi…"; SetupAction.Enroll -> "Preparing enrollment…"; else -> "Checking enrollment…" }
        try {
            val value = SetupSnapshot.parse(model.backend.call("NearbySetupAction", state.session, action, ssid, password, choice?.text("Account") ?: "", choice?.text("ID") ?: "", name).obj())
            state = value
            val detail = value.error
            if (automatically || value.pending) observationError = detail
            else if (detail != null && ConnectionRecovery.canRetry(detail)) observationError = detail
            else { error = detail; if (detail?.contains("name is already in use") == true) customizing = true }
            if ((value.pending || value.canResume) && !value.wifiConnected && action == SetupAction.Enroll) wifiExpanded = true
            if (value.completed) { error = null; observationError = null }
        } catch (e: CancellationException) { throw e } catch (e: Exception) {
            if (automatically || ConnectionRecovery.canRetry(e.message ?: "")) observationError = e.message else error = e.message
        } finally {
            busy = false; automatic = false
            if (action == SetupAction.ConnectWiFi) password = ""
            if (action == SetupAction.ScanWiFi) lastScan = System.currentTimeMillis()
            if (action == SetupAction.Status) lastStatus = System.currentTimeMillis()
        }
    }
    fun submit(action: Int) = scope.launch { perform(action) }
    /** Opens the enrolled device by its identifier, never by a possibly duplicated display name. */
    fun finish() = scope.launch {
        if (opening) return@launch
        opening = true; error = null
        try {
            waitIdle()
            busy = true; automatic = true
            val id = state.deviceID
            val device = if (id.isEmpty()) null else model.lookupDevice { it.id == id || (it.selector.startsWith("cloud://") && it.selector.substringAfterLast('/') == id) }
            if (device != null) { done(); model.connect(device) }
            else error = "Your device is not in the device list yet. Keep it online, then open it again."
        } finally { busy = false; automatic = false; opening = false }
    }
    LaunchedEffect(permitted) {
        if (!permitted) return@LaunchedEffect
        try {
            organizations = model.backend.call("EnrollmentOrganizations").items()
            organization = EnrollmentForm.defaultOrganization(organizations, model.account)
        } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message }
        organizationsLoaded = true
        while (true) {
            if (!busy) {
                val now = System.currentTimeMillis()
                when (state.observation(selected != null, wifiExpanded, (now - lastStatus) / 1000.0, (now - lastScan) / 1000.0)) {
                    SetupSnapshot.Observation.Discover -> {
                        busy = true; automatic = true
                        try { devices = model.backend.call("NearbyDiscover").items().map(SetupDevice::parse); observationError = null }
                        catch (e: CancellationException) { throw e } catch (e: Exception) { observationError = e.message } finally { busy = false; automatic = false }
                    }
                    SetupSnapshot.Observation.Reconnect -> selected?.let { if (now >= nextReconnect) connect(it) }
                    SetupSnapshot.Observation.CheckEnrollment -> perform(SetupAction.CheckEnrollment, true)
                    SetupSnapshot.Observation.Status -> perform(SetupAction.Status, true)
                    SetupSnapshot.Observation.ScanWiFi -> perform(SetupAction.ScanWiFi, true)
                    SetupSnapshot.Observation.Idle -> {}
                }
            }
            delay(5_000)
        }
    }
    val session = state.session
    DisposableEffect(Unit) { onDispose { password = "" } }
    DisposableEffect(session) { onDispose { if (session != 0L) model.task { withContext(NonCancellable) { model.backend.call("NearbyClose", session) } } } }

    Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(horizontal = 28.dp, vertical = 24.dp), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(22.dp)) {
        if (finished) IconTile(Icons.Outlined.CheckCircle, Modifier.size(96.dp)) else DeviceIllustration(Modifier.size(150.dp), MaterialTheme.colorScheme.primary)
        if (!permitted) {
            Text("Find your Wendy.", style = MaterialTheme.typography.headlineMedium, textAlign = TextAlign.Center)
            Text("Wendy uses Bluetooth to find and set up nearby devices.", color = MaterialTheme.colorScheme.onSurfaceVariant, textAlign = TextAlign.Center)
            PrimaryAction("Allow Bluetooth") { permissions.launch(bluetoothPermissions()) }
            return@Column
        }
        val headline = when (stage) {
            "attention" -> "Setup needs attention."
            "done" -> if (state.completed) "You’re all set." else "Already part of Wendy."
            "discovering" -> "Find your Wendy."
            "connecting" -> "Connecting to your Wendy…"
            "working" -> if (!state.attempted && !state.pending) progress else if (state.attempted && !editingLocked) "Checking enrollment…" else "Making it yours…"
            else -> "Make it yours."
        }
        val explanation = when (stage) {
            "attention" -> state.failure
            "done" -> "Your device is enrolled and ready to open."
            "discovering" -> "Power on your device and keep it nearby. We’ll find it automatically."
            "connecting" -> "Keep your device nearby. We’re taking care of the connection."
            "working" -> when {
                !state.attempted && !state.pending && progress == "Connecting to Wi-Fi…" -> "We’ll continue when your device is connected."
                state.attempted && !state.pending && !editingLocked -> "We haven’t received confirmation yet. Your enrollment won’t be sent twice."
                state.wifiConnected -> "Your device is joining your organization. We’ll keep checking automatically."
                else -> "Your device needs internet to finish. Connect it to Wi-Fi below, or plug in Ethernet. We’ll keep checking automatically."
            }
            else -> choice?.let { "Add this device to ${it.text("Name")}." } ?: if (organizations.isEmpty()) "Sign in to add this device to your organization." else "Choose the organization this device belongs to."
        }
        Column(horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Text(headline, style = MaterialTheme.typography.headlineMedium, textAlign = TextAlign.Center)
            selected?.let { Text(name.ifEmpty { it.title }, style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onSurfaceVariant) }
            Text(explanation, color = MaterialTheme.colorScheme.onSurfaceVariant, textAlign = TextAlign.Center)
        }
        @Composable fun wifiControls() {
            Column(Modifier.fillMaxWidth()) {
                TextButton(onClick = { wifiExpanded = !wifiExpanded }, enabled = !editingLocked) {
                    Text(if (state.wifiConnected) "Wi-Fi connected" else "Connect to Wi-Fi"); Icon(if (wifiExpanded) Icons.Outlined.ExpandLess else Icons.Outlined.ExpandMore, null)
                }
                if (wifiExpanded) Column(verticalArrangement = Arrangement.spacedBy(10.dp)) {
                    if (state.wifiConnected) Row(horizontalArrangement = Arrangement.spacedBy(6.dp)) { Icon(Icons.Outlined.Wifi, null, tint = MaterialTheme.colorScheme.onSurfaceVariant); Text(state.ssid, color = MaterialTheme.colorScheme.onSurfaceVariant) }
                    if (state.networks.isNotEmpty()) ChoiceField("Network", listOf("" to "Choose network") + state.networks.toSortedSet().map { it to it }, ssid, !editingLocked) { ssid = it }
                    else LoadingRow("Finding networks…")
                    OutlinedTextField(ssid, { ssid = it }, Modifier.fillMaxWidth(), label = { Text("Network name") }, singleLine = true, enabled = !editingLocked, keyboardOptions = KeyboardOptions(autoCorrectEnabled = false))
                    OutlinedTextField(password, { password = it }, Modifier.fillMaxWidth(), label = { Text("Password") }, singleLine = true, enabled = !editingLocked,
                        visualTransformation = PasswordVisualTransformation(), keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password))
                    Button(onClick = { submit(SetupAction.ConnectWiFi) }, enabled = ssid.isNotEmpty() && !editingLocked) { Text("Connect") }
                }
            }
        }
        when (stage) {
            "done" -> if (opening) LoadingRow("Opening device…") else PrimaryAction("Open device") { finish() }
            "discovering" -> {
                CircularProgressIndicator()
                devices.forEach { device ->
                    Surface(onClick = { scope.launch { waitIdle(); if (selected == null) { name = EnrollmentForm.suggestedName(device.title); connect(device) } } },
                        shape = RoundedCornerShape(20.dp), color = MaterialTheme.colorScheme.surface, border = BorderStroke(1.dp, MaterialTheme.colorScheme.outlineVariant), modifier = Modifier.fillMaxWidth()) {
                        Row(Modifier.padding(16.dp), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(14.dp)) {
                            IconTile(Icons.Outlined.Memory); Text(device.title, Modifier.weight(1f), style = MaterialTheme.typography.titleMedium); Icon(Icons.Outlined.ChevronRight, null)
                        }
                    }
                }
            }
            "connecting" -> CircularProgressIndicator()
            "attention" -> {
                if (state.canResume) PrimaryAction("Continue setup", enabled = state.connected && !editingLocked) { submit(SetupAction.Enroll) }
                else if (opening) LoadingRow("Opening device…") else PrimaryAction("View device") { finish() }
                wifiControls()
            }
            "working" -> { CircularProgressIndicator(); if (!editingLocked) wifiControls() }
            else -> Column(Modifier.fillMaxWidth(), verticalArrangement = Arrangement.spacedBy(16.dp)) {
                val organizationChoices = organizations.map { it.text("Account") to "${it.text("Name")} · ${it.text("Email").ifEmpty { it.text("Account") }}" }
                if (organizations.isEmpty()) PrimaryAction("Sign in") { done(); model.signIn() }
                else {
                    if (choice == null) ChoiceField("Organization", organizationChoices, organization) { organization = it }
                    PrimaryAction(choice?.let { "Add to ${it.text("Name")}" } ?: "Add device", enabled = choice != null && nameValid && !editingLocked) { submit(SetupAction.Enroll) }
                    TextButton(onClick = { customizing = !customizing }) { Text("Name & organization"); Icon(if (customizing) Icons.Outlined.ExpandLess else Icons.Outlined.ExpandMore, null) }
                    if (customizing) {
                        OutlinedTextField(name, { name = it }, Modifier.fillMaxWidth(), label = { Text("Device name") }, singleLine = true, keyboardOptions = KeyboardOptions(autoCorrectEnabled = false))
                        if (!nameValid) SecondaryText("Use a lowercase name with letters, numbers, and hyphens.")
                        ChoiceField("Organization", organizationChoices, organization) { organization = it }
                    }
                }
                wifiControls()
            }
        }
        error?.let { if (stage in setOf("ready", "done", "attention")) Text(PresentedFailure(it).message, color = MaterialTheme.colorScheme.onSurfaceVariant, textAlign = TextAlign.Center) }
        (observationError ?: error)?.let { detail -> if (!finished) SetupDetails(PresentedFailure(detail).details) }
    }
}

@Composable
private fun SetupDetails(text: String) {
    var open by remember { mutableStateOf(false) }
    Column(Modifier.fillMaxWidth()) {
        Text(if (open) "Hide details" else "Details", Modifier.clickable { open = !open }, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
        if (open) SelectionContainer { Text(text, fontFamily = FontFamily.Monospace, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant) }
    }
}

@Composable
private fun PrimaryAction(title: String, enabled: Boolean = true, action: () -> Unit) {
    Button(onClick = action, enabled = enabled, modifier = Modifier.fillMaxWidth().heightIn(min = 52.dp)) {
        Text(title); Spacer(Modifier.width(8.dp)); Icon(Icons.AutoMirrored.Outlined.ArrowForward, null, Modifier.size(18.dp))
    }
}

/** Remembers which advertisements already produced an invitation (iOS NearbySetupPromptHistory). */
class SetupPromptHistory {
    private val prompted = mutableSetOf<String>()
    fun next(devices: List<SetupDevice>): SetupDevice? {
        val device = devices.filter { it.rssi in -80..-1 && it.address !in prompted }.sortedWith(compareByDescending<SetupDevice> { it.rssi }.thenBy { it.address }).firstOrNull() ?: return null
        prompted.add(device.address)
        return device
    }
}

/** Foreground-only discovery that invites setup of a strong nearby advertisement (iOS NearbySetupPrompt). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun NearbySetupPrompt(model: WendyModel, active: Boolean, open: (SetupDevice) -> Unit) {
    val context = LocalContext.current
    val history = remember { SetupPromptHistory() }
    var presented by remember { mutableStateOf<SetupDevice?>(null) }
    // Only scan once the person has granted Bluetooth access; never ask from a background prompt.
    LaunchedEffect(active) {
        while (active) {
            if (presented == null && hasBluetoothPermission(context) && model.error == null && !model.loginPending) {
                val devices = runCatching { model.backend.call("NearbyDiscover").items().map(SetupDevice::parse) }.getOrDefault(emptyList())
                if (presented == null) presented = history.next(devices)
            }
            delay(5_000)
        }
    }
    presented?.let { device ->
        ModalBottomSheet(onDismissRequest = { presented = null }, containerColor = MaterialTheme.colorScheme.background) {
            Column(Modifier.fillMaxWidth().padding(horizontal = 28.dp).padding(bottom = 28.dp), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(16.dp)) {
                DeviceIllustration(Modifier.size(150.dp), MaterialTheme.colorScheme.primary)
                Text("A Wendy is nearby", style = MaterialTheme.typography.headlineSmall)
                Text(device.title, style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                Text("Add it to your organization to make it yours.", color = MaterialTheme.colorScheme.onSurfaceVariant, textAlign = TextAlign.Center)
                PrimaryAction("Set up device") { presented = null; open(device) }
                TextButton(onClick = { presented = null }) { Text("Not now") }
            }
        }
    }
}
