package dev.wendy.android

import android.Manifest
import android.content.Context
import android.content.pm.PackageManager
import android.os.Build
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.background
import androidx.compose.foundation.shape.CircleShape
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

/** The four setup steps and the moves between them (iOS NearbySetupFlow). */
class SetupFlow {
    enum class Step(val number: Int, val title: String) { Device(1, "Device"), Network(2, "Connection"), Organization(3, "Organization"), Enroll(4, "Enrollment") }
    var step by mutableStateOf(Step.Device); private set
    fun choseDevice() { step = Step.Network }
    fun continueFromNetwork(connected: Boolean, useEthernet: Boolean = false) { if (step == Step.Network && (connected || useEthernet)) step = Step.Organization }
    fun backToNetwork() { if (step == Step.Organization) step = Step.Network }
    fun reset() { step = Step.Device }
    companion object {
        /** Protocol byte limits, not character counts. */
        fun validWiFi(ssid: String, password: String) = ssid.isNotBlank() && ssid.toByteArray().size <= 32 && password.toByteArray().size <= 64
    }
}

/** Bluetooth setup for a nearby device in four steps (iOS NearbySetupView). Discovery and status checks run on their own. */
@Composable
fun NearbySetupScreen(model: WendyModel, initial: SetupDevice? = null, done: () -> Unit) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val page = LocalNavPage.current
    var permitted by remember { mutableStateOf(hasBluetoothPermission(context)) }
    val permissions = rememberLauncherForActivityResult(ActivityResultContracts.RequestMultiplePermissions()) { result -> permitted = result.values.all { it } }
    var devices by remember { mutableStateOf(emptyList<SetupDevice>()) }
    var candidate by remember { mutableStateOf<SetupDevice?>(null) }
    var scanned by remember { mutableStateOf(false) }
    val flow = remember { SetupFlow().also { if (initial != null) it.choseDevice() } }
    var selected by remember { mutableStateOf(initial) }
    var state by remember { mutableStateOf(SetupSnapshot()) }
    var organizations by remember { mutableStateOf(emptyList<JSONObject>()) }
    var organization by remember { mutableStateOf("") }
    var organizationsLoaded by remember { mutableStateOf(false) }
    var organizationError by remember { mutableStateOf<String?>(null) }
    var name by remember { mutableStateOf(initial?.let { EnrollmentForm.suggestedName(it.title) } ?: "") }
    var ssid by remember { mutableStateOf("") }
    var password by remember { mutableStateOf("") }
    var wifiExpanded by remember { mutableStateOf(initial != null) }
    var busy by remember { mutableStateOf(false) }
    var automatic by remember { mutableStateOf(false) }
    var opening by remember { mutableStateOf(false) }
    var closing by remember { mutableStateOf(false) }
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
    // Closing while a request is out asks first; the request may still finish on the device.
    SideEffect { page?.interceptBack = { if (editingLocked || state.pending) { closing = true; true } else false } }
    androidx.activity.compose.BackHandler(editingLocked || state.pending) { closing = true }
    var scopeKey by remember { mutableStateOf(model.account to model.organization) }
    LaunchedEffect(model.account, model.organization) { if (scopeKey != (model.account to model.organization)) { password = ""; done() } }
    suspend fun waitIdle() { while (busy) delay(100) }
    suspend fun discover() {
        if (busy) return
        busy = true; automatic = true
        try {
            val found = model.backend.call("NearbyDiscover").items().map(SetupDevice::parse)
            devices = found; scanned = true
            if (candidate != null && found.none { it.address == candidate?.address }) candidate = null
            observationError = null
        } catch (e: CancellationException) { throw e } catch (e: Exception) { devices = emptyList(); candidate = null; scanned = true; observationError = e.message }
        finally { busy = false; automatic = false }
    }
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
    suspend fun choose(device: SetupDevice) {
        waitIdle()
        if (selected != null || devices.none { it.address == device.address }) return
        name = EnrollmentForm.suggestedName(device.title)
        flow.choseDevice(); wifiExpanded = true
        connect(device)
    }
    suspend fun chooseAnother() {
        if (busy || state.attempted || state.pending || editingLocked) return
        busy = true
        try { if (state.session != 0L) model.backend.call("NearbyClose", state.session) } catch (e: CancellationException) { throw e } catch (_: Exception) {} finally { busy = false }
        selected = null; candidate = null; state = SetupSnapshot(); flowReset(flow)
        password = ""; ssid = ""; error = null; observationError = null
        nextReconnect = 0L; reconnectDelay = 3_000L
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
            else error = detail
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
    suspend fun loadOrganizations() {
        try {
            organizations = model.backend.call("EnrollmentOrganizations").items()
            organizationError = null
            organization = EnrollmentForm.defaultOrganization(organizations, model.account)
        } catch (e: CancellationException) { throw e } catch (e: Exception) { organizationError = e.message }
        organizationsLoaded = true
    }
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
        loadOrganizations()
        while (true) {
            if (!busy) {
                val now = System.currentTimeMillis()
                if (!model.inForeground) {
                    // Bluetooth stays closed while Wendy is in the background.
                    if (state.connected) { try { model.backend.call("NearbyClose", state.session) } catch (e: CancellationException) { throw e } catch (_: Exception) {}; state = state.copy(connected = false) }
                } else when (state.observation(selected != null, wifiExpanded, (now - lastStatus) / 1000.0, (now - lastScan) / 1000.0)) {
                    SetupSnapshot.Observation.Discover -> discover()
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
    DisposableEffect(Unit) { onDispose { password = ""; page?.interceptBack = null } }
    DisposableEffect(session) { onDispose { if (session != 0L) model.task { withContext(NonCancellable) { model.backend.call("NearbyClose", session) } } } }

    Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(horizontal = 28.dp, vertical = 24.dp), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(24.dp)) {
        if (!permitted) {
            DeviceIllustration(Modifier.size(150.dp), MaterialTheme.colorScheme.primary)
            Text("Find your Wendy.", style = MaterialTheme.typography.headlineMedium, textAlign = TextAlign.Center)
            Text("Wendy uses Bluetooth to find and set up nearby devices.", color = MaterialTheme.colorScheme.onSurfaceVariant, textAlign = TextAlign.Center)
            PrimaryAction("Allow Bluetooth") { permissions.launch(bluetoothPermissions()) }
            return@Column
        }
        if (!finished) {
            val step = if (state.pending || state.attempted || (editingLocked && progress == "Preparing enrollment…")) SetupFlow.Step.Enroll else if (selected == null) SetupFlow.Step.Device else flow.step
            Column(Modifier.fillMaxWidth(), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text("Step ${step.number} of 4 · ${step.title}", style = MaterialTheme.typography.titleSmall)
                Row(horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                    (1..4).forEach { number -> Box(Modifier.weight(1f).height(4.dp).background(if (number <= step.number) LensStyle.green else MaterialTheme.colorScheme.onSurface.copy(alpha = .18f), CircleShape)) }
                }
            }
        }
        if (finished) IconTile(Icons.Outlined.CheckCircle, Modifier.size(96.dp)) else DeviceIllustration(Modifier.size(104.dp), MaterialTheme.colorScheme.primary)
        val headline = when (stage) {
            "attention" -> "Setup needs attention."
            "done" -> if (state.completed) "You’re all set." else "Already part of Wendy."
            "discovering" -> if (observationError == null) "Choose a device" else "Bluetooth unavailable"
            "connecting" -> "Connecting to your Wendy…"
            "working" -> if (!state.attempted && !state.pending) progress else if (state.attempted && !editingLocked) "Checking enrollment…" else "Making it yours…"
            else -> if (flow.step == SetupFlow.Step.Network) "Connect to Wi-Fi" else "Name & organization"
        }
        val explanation = when (stage) {
            "attention" -> state.failure
            "done" -> "Your device is enrolled and ready to open."
            "discovering" -> "Power on your device and keep it nearby. We’ll find it automatically."
            "connecting" -> "Keep your device nearby."
            "working" -> when {
                !state.attempted && !state.pending && progress == "Connecting to Wi-Fi…" -> "We’ll continue when your device is connected."
                state.attempted && !state.pending && !editingLocked -> "We haven’t received confirmation yet. Your enrollment won’t be sent twice."
                state.wifiConnected -> "Your device is joining your organization."
                else -> "Connect to Wi-Fi below or plug in Ethernet to finish."
            }
            else -> if (flow.step == SetupFlow.Step.Network) "Credentials are sent over Bluetooth. You can also use an Ethernet connection."
                else choice?.let { "Add this device to ${it.text("Name")}." } ?: if (organizations.isEmpty()) "Sign in to add this device to your organization." else "Choose the organization this device belongs to."
        }
        Column(horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(10.dp)) {
            Text(headline, style = MaterialTheme.typography.headlineMedium, textAlign = TextAlign.Center)
            selected?.let { Text(name.ifEmpty { it.title }, style = MaterialTheme.typography.titleMedium, color = MaterialTheme.colorScheme.onSurfaceVariant, textAlign = TextAlign.Center) }
            Text(explanation, color = MaterialTheme.colorScheme.onSurfaceVariant, textAlign = TextAlign.Center)
        }
        @Composable fun wifiControls() {
            Column(Modifier.fillMaxWidth()) {
                TextButton(onClick = { wifiExpanded = !wifiExpanded }, enabled = !editingLocked) {
                    Text(if (state.wifiConnected) "Wi-Fi connected" else "Connect to Wi-Fi"); Icon(if (wifiExpanded) Icons.Outlined.ExpandLess else Icons.Outlined.ExpandMore, null)
                }
                if (wifiExpanded) Column(verticalArrangement = Arrangement.spacedBy(12.dp)) {
                    if (state.wifiConnected) Row(horizontalArrangement = Arrangement.spacedBy(6.dp)) { Icon(Icons.Outlined.Wifi, null, tint = MaterialTheme.colorScheme.onSurfaceVariant); Text(state.ssid, color = MaterialTheme.colorScheme.onSurfaceVariant) }
                    when {
                        state.networks.isNotEmpty() -> {
                            val names = (state.networks.toSortedSet() + listOfNotNull(ssid.ifEmpty { null })).toSortedSet()
                            ChoiceField("Network", listOf("" to "Choose network") + names.map { it to it }, ssid, !editingLocked) { ssid = it }
                        }
                        lastScan == 0L -> LoadingRow("Finding networks…")
                        else -> SecondaryText("No networks found. Enter a network name or scan again.")
                    }
                    TextButton(onClick = { submit(SetupAction.ScanWiFi) }, enabled = state.connected && !editingLocked) { Text("Scan networks") }
                    OutlinedTextField(ssid, { ssid = it }, Modifier.fillMaxWidth(), label = { Text("Network name") }, singleLine = true, enabled = !editingLocked, keyboardOptions = KeyboardOptions(autoCorrectEnabled = false))
                    OutlinedTextField(password, { password = it }, Modifier.fillMaxWidth(), label = { Text("Password") }, singleLine = true, enabled = !editingLocked,
                        visualTransformation = PasswordVisualTransformation(), keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password))
                    Button(onClick = { submit(SetupAction.ConnectWiFi) }, enabled = SetupFlow.validWiFi(ssid, password) && state.connected && !editingLocked) { Text("Connect") }
                }
            }
        }
        when (stage) {
            "done" -> if (opening) LoadingRow("Opening device…") else PrimaryAction("Open device") { finish() }
            "discovering" -> Column(Modifier.fillMaxWidth(), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(16.dp)) {
                if (!scanned) LoadingRow("Scanning over Bluetooth…")
                observationError?.let { Text(PresentedFailure(it).message, color = MaterialTheme.colorScheme.onSurfaceVariant, textAlign = TextAlign.Center) }
                    ?: run { if (scanned && devices.isEmpty()) Text("No nearby devices found. Keep the device powered on and within a few metres, then scan again.", color = MaterialTheme.colorScheme.onSurfaceVariant, textAlign = TextAlign.Center) }
                devices.forEach { device ->
                    val picked = candidate?.address == device.address
                    Surface(onClick = { candidate = device }, shape = RoundedCornerShape(20.dp), color = MaterialTheme.colorScheme.surface, border = BorderStroke(1.dp, if (picked) LensStyle.green else MaterialTheme.colorScheme.outlineVariant), modifier = Modifier.fillMaxWidth()) {
                        Row(Modifier.padding(16.dp), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(14.dp)) {
                            IconTile(Icons.Outlined.Memory)
                            Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                                Text(device.title, style = MaterialTheme.typography.titleMedium)
                                SecondaryText(if (device.rssi < 0) "Bluetooth signal ${device.rssi} dBm" else "Bluetooth signal unavailable")
                            }
                            Icon(if (picked) Icons.Outlined.CheckCircle else Icons.Outlined.RadioButtonUnchecked, if (picked) "Selected" else null, tint = if (picked) LensStyle.accent else MaterialTheme.colorScheme.onSurfaceVariant)
                        }
                    }
                }
                TextButton(onClick = { scope.launch { discover() } }, enabled = !busy) { Text("Scan again") }
                PrimaryAction("Continue", enabled = candidate != null && !busy && devices.any { it.address == candidate?.address }) { candidate?.let { scope.launch { choose(it) } } }
            }
            "connecting" -> Column(horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(12.dp)) {
                CircularProgressIndicator()
                if (!busy) {
                    selected?.let { device -> TextButton(onClick = { scope.launch { connect(device) } }) { Text("Retry connection") } }
                    TextButton(onClick = { scope.launch { chooseAnother() } }) { Text("Choose another device") }
                }
            }
            "attention" -> {
                if (state.canResume) PrimaryAction("Continue setup", enabled = state.connected && !editingLocked) { submit(SetupAction.Enroll) }
                else if (opening) LoadingRow("Opening device…") else PrimaryAction("View device") { finish() }
                wifiControls()
            }
            "working" -> { CircularProgressIndicator(); if (!editingLocked) wifiControls() }
            else -> if (flow.step == SetupFlow.Step.Network) Column(Modifier.fillMaxWidth(), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(16.dp)) {
                wifiControls()
                if (state.wifiConnected) PrimaryAction("Continue") { flow.continueFromNetwork(connected = true) }
                else {
                    TextButton(onClick = { flow.continueFromNetwork(connected = false, useEthernet = true) }) { Text("Use Ethernet instead") }
                    SecondaryText("Ethernet connectivity will be checked when the device enrolls.")
                }
                TextButton(onClick = { scope.launch { chooseAnother() } }, enabled = !editingLocked) { Text("Choose another device") }
            } else Column(Modifier.fillMaxWidth(), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(16.dp)) {
                val organizationChoices = organizations.map { it.text("Account") to "${it.text("Name")} · ${it.text("Email").ifEmpty { it.text("Account") }}" }
                when {
                    organizationError != null -> { SecondaryText(organizationError!!); TextButton(onClick = { scope.launch { loadOrganizations() } }) { Text("Retry organizations") } }
                    organizations.isEmpty() -> PrimaryAction("Sign in") { done(); model.signIn() }
                    else -> {
                        OutlinedTextField(name, { name = it }, Modifier.fillMaxWidth(), label = { Text("Device name") }, singleLine = true, keyboardOptions = KeyboardOptions(autoCorrectEnabled = false))
                        if (!nameValid) SecondaryText("Use a lowercase name with letters, numbers, and hyphens.")
                        ChoiceField("Organization", listOf("" to "Choose organization") + organizationChoices, organization) { organization = it }
                        PrimaryAction(choice?.let { "Enroll in ${it.text("Name")}" } ?: "Enroll device", enabled = choice != null && nameValid && !editingLocked) { submit(SetupAction.Enroll) }
                    }
                }
                TextButton(onClick = { flow.backToNetwork(); wifiExpanded = true }, enabled = !editingLocked) { Text("Back to connection") }
            }
        }
        error?.let { if (stage in setOf("ready", "done", "attention")) Text(PresentedFailure(it).message, color = MaterialTheme.colorScheme.onSurfaceVariant, textAlign = TextAlign.Center) }
        (observationError ?: error)?.let { detail -> if (!finished) SetupDetails(PresentedFailure(detail).details) }
    }
    if (closing) ConfirmAction("Close setup?", "A request already sent to the device may still finish. Closing stops checks here; it does not undo enrollment or Wi-Fi changes. Reopen setup to check the same device.",
        "Close setup", onDismiss = { closing = false }) { closing = false; password = ""; done() }
}

/** Starts the flow over at the device step after choosing another device. */
private fun flowReset(flow: SetupFlow) { flow.reset() }

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
