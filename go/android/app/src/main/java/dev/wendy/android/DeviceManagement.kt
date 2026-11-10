package dev.wendy.android

import androidx.compose.foundation.layout.*
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.launch
import org.json.JSONObject

/** Rename, remove from Cloud, reset local enrollment, and software updates (iOS DeviceManagementView). */
@Composable
fun DeviceManagementPage(model: WendyModel, device: Device) {
    val nav = LocalNavigator.current
    val session = model.session
    val scope = rememberCoroutineScope()
    var identity by remember { mutableStateOf<JSONObject?>(null) }
    var accounts by remember { mutableStateOf(emptyList<JSONObject>()) }
    var account by remember { mutableStateOf("") }
    var name by remember { mutableStateOf(EnrollmentForm.suggestedName(device.title)) }
    var busy by remember { mutableStateOf(false) }
    var removed by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    var message by remember { mutableStateOf<String?>(null) }
    var confirm by remember { mutableStateOf<String?>(null) }
    suspend fun refresh() {
        busy = true; error = null; identity = null; accounts = emptyList(); account = ""
        try {
            val current = model.backend.call("DeviceEnrollment", session).obj()
            val saved = model.backend.call("EnrollmentOrganizations").items()
            identity = current; accounts = saved.filter { it.text("ID") == current.text("OrganizationID") }
            if (accounts.size == 1) account = accounts[0].text("Account")
        } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { busy = false }
    }
    val deviceID = identity?.text("DeviceID") ?: ""
    val canManage = deviceID.isNotEmpty() && account.isNotEmpty() && !busy && !removed
    LaunchedEffect(session) { refresh() }
    PageColumn {
        formSection("Device") {
            LabeledRow("Name", device.title)
            identity?.let {
                val organization = it.text("OrganizationName").ifEmpty { it.text("OrganizationID") }
                Text(if (!it.flag("Enrolled")) "Not enrolled in Wendy Cloud" else if (organization.isEmpty()) "Enrolled in Wendy Cloud" else "Enrolled in $organization", Modifier.padding(vertical = 6.dp))
                if (deviceID.isNotEmpty()) LabeledRow("Device ID", deviceID, selectable = true)
            } ?: run { if (error == null) LoadingRow("Checking device identity…") }
            if (accounts.isNotEmpty()) ChoiceField("Signed-in account", accounts.map { it.text("Account") to "${it.text("Name")} · ${it.text("Email").ifEmpty { it.text("Account") }}" }, account, !busy && !removed) { account = it }
            if (identity != null && accounts.isEmpty()) SecondaryText("Sign in to this device’s organization to manage it.")
        }
        formSection("Rename", "Use lowercase letters, numbers and hyphens, starting with a letter. This changes its Cloud name and local hostname; reconnect from Devices afterward.") {
            OutlinedTextField(name, { name = it }, Modifier.fillMaxWidth(), label = { Text("New device name") }, singleLine = true, enabled = !busy && !removed,
                keyboardOptions = KeyboardOptions(autoCorrectEnabled = false, keyboardType = KeyboardType.Ascii))
            ActionRow("Rename device", enabled = canManage && EnrollmentForm.validName(name)) { confirm = "rename" }
        }
        formSection(footer = "Revokes Cloud access and removes the device from this organization. This does not reset its local enrollment, apps or Wi-Fi. Local recovery is required before enrolling it again.") {
            ActionRow("Remove from Wendy Cloud", Icons.Outlined.CloudOff, destructive = true, enabled = canManage) { confirm = "remove" }
        }
        formSection {
            NavRow("Reset local enrollment", Icons.Outlined.RestartAlt, enabled = canManage) {
                nav.push("Reset local enrollment") { LocalResetPage(model, device, account, identity?.text("OrganizationID") ?: "", deviceID) }
            }
        }
        formSection("Software and power") {
            NavRow("Agent update and device info", Icons.Outlined.Info) { nav.push("Device info") { DeviceInfoPage(model, device.title) } }
            NavRow("OS update", Icons.Outlined.SystemUpdate) { nav.push("OS update") { OSUpdatePage(model, device) } }
            SecondaryText("Remote restart is not supported yet.")
        }
        if (busy) item { LoadingRow("Applying to ${device.title}…") }
        message?.let { item { SecondaryText(it) } }
        error?.let { item { FormSection { FailureNote(it) } } }
        if (!busy && !removed) item { TextButton(onClick = { scope.launch { refresh() } }) { Text("Refresh identity") } }
    }
    confirm?.let { action ->
        val remove = action == "remove"
        ConfirmAction(if (remove) "Remove ${device.title} from Wendy Cloud?" else "Rename ${device.title} to $name?",
            if (remove) "Cloud connections will stop. The device is not factory reset. Local recovery will be needed to enroll again." else "The Cloud name and local hostname will both change. Reconnect from Devices afterward.",
            if (remove) "Revoke access and remove" else "Rename device", destructive = remove, onDismiss = { confirm = null }) {
            confirm = null
            scope.launch {
                busy = true; error = null; message = null
                try {
                    model.backend.call("ManageDevice", session, account, identity?.text("OrganizationID") ?: "", deviceID, name, remove)
                    removed = remove
                    message = if (remove) "Cloud access removed. Local enrollment was not reset. Return to Devices and refresh." else "Renamed to $name. Return to Devices and refresh before reconnecting."
                } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { busy = false }
            }
        }
    }
}

/** Two steps: the device proves its identity, then the person confirms the reset (iOS DeviceLocalResetView). */
@Composable
private fun LocalResetPage(model: WendyModel, device: Device, account: String, organization: String, deviceID: String) {
    val session = model.session
    val scope = rememberCoroutineScope()
    var plan by remember { mutableStateOf<JSONObject?>(null) }
    var busy by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    var done by remember { mutableStateOf<String?>(null) }
    var confirm by remember { mutableStateOf(false) }
    LaunchedEffect(session) {
        busy = true
        try { plan = model.backend.call("PrepareLocalReset", session, account, organization, deviceID).obj() } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { busy = false }
    }
    PageColumn {
        formSection(footer = "Resetting removes the device’s Cloud enrollment on the device itself, so it can be enrolled again. Apps and Wi-Fi stay. It requires a direct local connection.") {
            LabeledRow("Device", device.title)
            plan?.let { LabeledRow("Principal", it.text("Principal"), selectable = true); LabeledRow("Fingerprint", it.text("Fingerprint"), selectable = true) }
            if (busy) LoadingRow("Checking…")
            error?.let { FailureNote(it) }
            done?.let { Text(it, Modifier.padding(vertical = 8.dp)) }
            if (plan != null && done == null) ActionRow("Reset local enrollment", Icons.Outlined.RestartAlt, destructive = true, enabled = !busy) { confirm = true }
        }
    }
    if (confirm) ConfirmAction("Reset local enrollment?", "${device.title} will forget its Cloud enrollment. You will need to enroll it again to reach it through Wendy Cloud.", "Reset", onDismiss = { confirm = false }) {
        confirm = false
        val current = plan ?: return@ConfirmAction
        scope.launch {
            busy = true; error = null
            try {
                val reset = model.backend.call("ResetLocalEnrollment", session, account, organization, deviceID, current.text("Principal"), current.text("Fingerprint")) as? Boolean ?: false
                done = if (reset) "Local enrollment was reset. Enroll the device again from its settings." else "The device reported that nothing needed resetting."
            } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { busy = false }
        }
    }
}

/** Install an OS artifact and follow the agent's status (iOS DeviceOSUpdateView). */
@Composable
private fun OSUpdatePage(model: WendyModel, device: Device) {
    val session = model.session
    val scope = rememberCoroutineScope()
    var snapshot by remember { mutableStateOf<JSONObject?>(null) }
    var info by remember { mutableStateOf<JSONObject?>(null) }
    var artifact by remember { mutableStateOf("") }
    var busy by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    var confirm by remember { mutableStateOf(false) }
    var refresh by remember { mutableIntStateOf(0) }
    LaunchedEffect(session, refresh) {
        busy = true; error = null
        try { info = model.backend.call("DeviceInfo", session).obj(); snapshot = model.backend.call("MobileOSUpdate", session, "", false).obj() }
        catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { busy = false }
    }
    val status = snapshot?.optJSONObject("status")
    val connector = status?.optJSONObject("engineStatus")?.optString("connector").orEmpty()
    PageColumn {
        formSection("Operating system") {
            LabeledRow("Device", device.title)
            info?.let { LabeledRow("Installed", it.text("os_version", "os")) }
            status?.optString("newOsVersion")?.takeIf { it.isNotEmpty() }?.let { LabeledRow("Target version", it) }
            snapshot?.optJSONObject("install")?.let { TechnicalDetails(it, "Install progress") }
            status?.let { TechnicalDetails(it, "Agent status") }
        }
        formSection("Install update", "Use the OS artifact for this device’s hardware. The Agent checks compatibility before installation. The update is only verified after the device restarts and passes its health checks.") {
            OutlinedTextField(artifact, { artifact = it }, Modifier.fillMaxWidth(), label = { Text("OS artifact URL") }, singleLine = true, keyboardOptions = KeyboardOptions(autoCorrectEnabled = false, keyboardType = KeyboardType.Uri))
            ActionRow("Install OS update", Icons.Outlined.SystemUpdate, enabled = !busy && connector.isNotEmpty() && artifact.isNotBlank()) { confirm = true }
            if (connector.isEmpty() && snapshot != null) SecondaryText("This device’s agent does not report an OS update engine.")
        }
        if (busy) item { LoadingRow("Checking OS update status…") }
        error?.let { item { FormSection { FailureNote(it) } } }
        item { TextButton(onClick = { refresh++ }, enabled = !busy) { Text("Refresh status") } }
    }
    if (confirm) ConfirmAction("Install OS update?", "The operating system will be replaced. Keep power connected and plan for downtime while the device restarts. The update is only verified after post-boot health checks pass.", "Install OS update", onDismiss = { confirm = false }) {
        confirm = false
        scope.launch {
            busy = true; error = null
            try { snapshot = model.backend.call("MobileOSUpdate", session, artifact.trim(), true).obj() } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { busy = false }
        }
    }
}
