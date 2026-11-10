package dev.wendy.android

import android.text.format.DateUtils
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.semantics.selected
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import org.json.JSONObject

/** Wendy Lite boards: SensorLink cameras, temperature, console logs, Wi-Fi and enrollment (iOS LiteDeviceView). */
@Composable
fun LiteDeviceScreen(model: WendyModel) {
    var page by rememberSaveableString("Cameras")
    Column(Modifier.fillMaxSize()) {
        Row(Modifier.horizontalScroll(rememberScrollState()).padding(horizontal = 20.dp, vertical = 8.dp), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            listOf("Cameras", "Sensors", "Device", "Wi-Fi", "Logs", "Settings").forEach { name -> PaneChip(name, page == name) { page = name } }
        }
        Box(Modifier.weight(1f)) {
            key(page, model.session) {
                when (page) {
                    "Cameras" -> CamerasScreen(model)
                    "Sensors" -> LiteTemperaturePage(model)
                    "Logs" -> DeviceLogsPage(model)
                    "Wi-Fi" -> LiteWifiPage(model)
                    "Settings" -> PageColumn { formSection("Enrollment") { EnrollmentSettings(model, model.selected?.title ?: "") } }
                    else -> LiteInfoPage(model)
                }
            }
        }
    }
}

@Composable
fun PaneChip(name: String, selected: Boolean, onClick: () -> Unit) {
    Surface(onClick = onClick, color = if (selected) MaterialTheme.colorScheme.primaryContainer else Color.Transparent, shape = RoundedCornerShape(14.dp), modifier = Modifier.semantics { this.selected = selected }) {
        Text(name, Modifier.padding(horizontal = 16.dp, vertical = 12.dp), style = MaterialTheme.typography.labelLarge,
            color = if (selected) MaterialTheme.colorScheme.onPrimaryContainer else MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

@Composable fun rememberSaveableString(initial: String) = androidx.compose.runtime.saveable.rememberSaveable { mutableStateOf(initial) }

@Composable
private fun LiteInfoPage(model: WendyModel) {
    val session = model.session
    val scope = rememberCoroutineScope()
    var info by remember { mutableStateOf<JSONObject?>(null) }
    var error by remember { mutableStateOf<String?>(null) }
    suspend fun refresh() { try { info = model.backend.call("DeviceInfo", session).obj(); error = null } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } }
    LaunchedEffect(session) { refresh() }
    PageColumn {
        formSection("Wendy Lite") {
            LabeledRow("Name", model.selected?.title ?: "")
            info?.let { LabeledRow("Board", it.text("board_model")); LabeledRow("Architecture", it.text("cpu_architecture")); LabeledRow("Firmware", it.text("os_version")) }
                ?: run { if (error == null) LoadingRow("Loading device information…") }
            error?.let { FailureNote(it) }
            ActionRow("Refresh") { scope.launch { refresh() } }
            ActionRow("Reconnect") { model.reconnect() }
        }
        item { SecondaryText("View SensorLink cameras, read the device console, and configure Wi-Fi. For containers, campaigns, and recorded data, pair this source with a WendyOS device.") }
    }
}

@Composable
private fun LiteWifiPage(model: WendyModel) {
    val session = model.session
    val scope = rememberCoroutineScope()
    var ssid by remember { mutableStateOf("") }
    var password by remember { mutableStateOf("") }
    var saving by remember { mutableStateOf(false) }
    var notice by remember { mutableStateOf<String?>(null) }
    var error by remember { mutableStateOf<String?>(null) }
    var confirm by remember { mutableStateOf(false) }
    DisposableEffect(Unit) { onDispose { password = "" } }
    PageColumn {
        formSection("Wi-Fi network") {
            OutlinedTextField(ssid, { ssid = it }, Modifier.fillMaxWidth(), label = { Text("Network name") }, singleLine = true, keyboardOptions = KeyboardOptions(autoCorrectEnabled = false))
            OutlinedTextField(password, { password = it }, Modifier.fillMaxWidth(), label = { Text("Password") }, singleLine = true,
                visualTransformation = PasswordVisualTransformation(), keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Password))
            SecondaryText("Leave the password empty for an open network.")
            ActionRow("Save and restart…", enabled = !saving && ssid.isNotBlank()) { confirm = true }
            if (saving) LoadingRow("Saving Wi-Fi…")
        }
        notice?.let { formSection { Text(it, Modifier.padding(vertical = 8.dp)); ActionRow("Reconnect") { model.reconnect() } } }
        error?.let { formSection { FailureNote(it) } }
    }
    if (confirm) ConfirmAction("Save Wi-Fi and restart?", "This replaces the device's saved Wi-Fi networks with $ssid and restarts it. Camera and console streams will disconnect.", "Save and restart", destructive = false, onDismiss = { confirm = false }) {
        confirm = false
        scope.launch {
            saving = true; error = null; notice = null
            try { model.backend.call("JoinWiFi", session, ssid, password, 0, false); password = ""; notice = "Wi-Fi configuration sent. Reconnect after the device restarts." }
            catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { saving = false }
        }
    }
}

@Composable
private fun LiteTemperaturePage(model: WendyModel) {
    val session = model.session
    var sample by remember { mutableStateOf<JSONObject?>(null) }
    var received by remember { mutableStateOf<Long?>(null) }
    var error by remember { mutableStateOf<String?>(null) }
    var live by remember { mutableStateOf(true) }
    var retry by remember { mutableIntStateOf(0) }
    LaunchedEffect(session, live, retry) {
        if (!live) return@LaunchedEffect
        try {
            while (true) {
                sample = model.backend.call("SensorTemperature", session).obj(); received = System.currentTimeMillis(); error = null
                delay(2000)
            }
        } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message }
    }
    PageColumn {
        formSection("SensorLink temperature") {
            ToggleRow("Live readings", live) { live = it }
            val value = sample; val time = received
            if (value != null && time != null) {
                LabeledRow(value.text("Name").ifEmpty { "Temperature" }, "%.1f °C".format(value.optDouble("Celsius")))
                LabeledRow("Last received", DateUtils.getRelativeTimeSpanString(time).toString(), valueColor = MaterialTheme.colorScheme.onSurfaceVariant)
            } else if (error == null && live) LoadingRow("Waiting for a temperature sample…")
            error?.let { FailureNote(it) }
            ActionRow("Retry", enabled = live) { retry++ }
        }
        item { SecondaryText("Reads the device’s advertised SensorLink temperature channel over the selected connection. A reading is the source’s temperature, not the phone’s.") }
    }
}
