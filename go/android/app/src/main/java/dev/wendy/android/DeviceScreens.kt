package dev.wendy.android

import androidx.compose.foundation.layout.*
import androidx.compose.foundation.background
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.delay
import org.json.JSONObject

@OptIn(ExperimentalLayoutApi::class)
@Composable
fun OverviewScreen(model: WendyModel) {
    val info = rememberRemote(model, "DeviceInfo", model.session)
    val resources = if (!model.lite) rememberRemote(model, "Resources", model.session) else remember { RemoteState().apply { loading = false } }
    LaunchedEffect(resources) { while (true) { delay(4000); if (!resources.loading) resources.refresh() } }
    val hardware = info.value.obj()
    val stats = resources.value.obj()
    val host = stats.child("Host")
    val memoryTotal = host.number("mem_total_bytes")
    val memoryUsed = (memoryTotal - host.number("mem_available_bytes")).coerceAtLeast(0)
    val cpu = if (stats.flag("HasCPU")) stats.optDouble("CPUPercent").takeIf { it.isFinite() } else null
    val disk = stats.child("Storage")
    val diskTotal = disk.number("total_bytes")
    val gpu = host.list("gpus").firstOrNull()
    var details by remember { mutableStateOf(false) }
    LazyColumn(contentPadding = PaddingValues(20.dp), verticalArrangement = Arrangement.spacedBy(18.dp)) {
        item {
            Box(Modifier.fillMaxWidth().clip(RoundedCornerShape(28.dp)).background(Brush.linearGradient(listOf(Color(0xFF1C4838), Color(0xFF0B2923))))) {
                DeviceIllustration(Modifier.align(Alignment.CenterEnd).size(155.dp).offset(x = 28.dp, y = 8.dp))
                Column(Modifier.padding(24.dp).fillMaxWidth(.78f), verticalArrangement = Arrangement.spacedBy(14.dp)) {
                    StatusPill("Connected", dark = true)
                    Text(model.selected?.name ?: "Your device", style = MaterialTheme.typography.headlineLarge, color = Color(0xFFF3F8EA))
                    Text(hardware.text("board_model").ifBlank { if (model.lite) "Wendy Lite" else hardware.text("os_version").ifBlank { "Wendy device" } }, style = MaterialTheme.typography.bodySmall, color = WendyMint)
                }
            }
        }
        if (info.error != null) item { RemoteStatus(info) }
        if (!model.lite) {
            item {
                Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
                    Text("Live performance", style = MaterialTheme.typography.titleLarge, modifier = Modifier.weight(1f))
                    Text("Every 4 sec", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                }
            }
            if (resources.error != null) item { RemoteStatus(resources) }
            item {
                Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                    MetricTile("CPU", cpu?.let { "%.0f%%".format(it) } ?: "—", if (cpu == null) "Awaiting sample" else "${host.number("cpu_count")} cores", cpu?.toFloat()?.div(100f), Modifier.weight(1f))
                    MetricTile("Memory", if (memoryTotal > 0) formatBytes(memoryUsed) else "—", if (memoryTotal > 0) "of ${formatBytes(memoryTotal)}" else "Not available", if (memoryTotal > 0) memoryUsed.toFloat() / memoryTotal else null, Modifier.weight(1f), Icons.Outlined.Storage)
                }
            }
            item {
                Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                    MetricTile("Storage", if (diskTotal > 0) "%.0f%%".format(disk.number("used_bytes") * 100.0 / diskTotal) else "—", if (diskTotal > 0) "${formatBytes(diskTotal - disk.number("used_bytes"))} free" else "Not available", if (diskTotal > 0) disk.number("used_bytes").toFloat() / diskTotal else null, Modifier.weight(1f), Icons.Outlined.SdStorage)
                    if (gpu != null) {
                        MetricTile("GPU", "%.0f%%".format(gpu.optDouble("util_percent", 0.0)), if (gpu.has("temp_c")) "%.0f°C".format(gpu.optDouble("temp_c")) else gpu.text("name").ifBlank { "Utilization" }, (gpu.optDouble("util_percent", 0.0) / 100).toFloat(), Modifier.weight(1f), Icons.Outlined.DeveloperBoard)
                    } else {
                        val temperature = host.list("thermal_zones").firstOrNull()
                        MetricTile("Temperature", temperature?.let { "%.0f°C".format(it.optDouble("temp_c")) } ?: "—", temperature?.text("name") ?: "Not available", null, Modifier.weight(1f), Icons.Outlined.Thermostat)
                    }
                }
            }
        }
        item {
            SectionCard("At a glance") {
                InfoLine("System", hardware.text("os_version", "os").ifBlank { "Loading…" })
                if (hardware.text("board_model").isNotBlank()) InfoLine("Model", hardware.text("board_model"))
                val features = hardware.optJSONArray("featureset")
                if (features != null && features.length() > 0) {
                    Spacer(Modifier.height(4.dp))
                    FlowRow(horizontalArrangement = Arrangement.spacedBy(6.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
                        val names = mapOf("gpu" to "GPU", "audio" to "Audio", "bluetooth" to "Bluetooth", "video" to "Video", "data-inference-classification" to "Inference")
                        (0 until features.length()).map { features.optString(it) }.filter { it in names }.forEach { feature ->
                            Surface(color = MaterialTheme.colorScheme.secondaryContainer, shape = RoundedCornerShape(8.dp)) {
                                Text(names.getValue(feature), Modifier.padding(horizontal = 10.dp, vertical = 6.dp), style = MaterialTheme.typography.labelSmall)
                            }
                        }
                    }
                }
            }
        }
        val processes = stats.list("Rows").filter { !it.flag("IsService") }
        if (processes.isNotEmpty()) item {
            SectionCard("Workloads") {
                processes.forEach { process ->
                    Row(Modifier.fillMaxWidth().padding(vertical = 4.dp), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                        IconTile(Icons.Outlined.Widgets)
                        Column(Modifier.weight(1f)) { Text(process.text("Name", "App"), style = MaterialTheme.typography.titleSmall); Text(formatBytes(process.number("MemoryBytes")), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant) }
                        Text(if (process.flag("HasCPU")) "%.1f%%".format(process.optDouble("CPUPercent")) else "—", style = MaterialTheme.typography.labelLarge)
                    }
                }
            }
        }
        item {
            SettingsRow("Technical details", "Full hardware and resource information", Icons.Outlined.Info) { details = !details }
            androidx.compose.animation.AnimatedVisibility(details) {
                Column(Modifier.padding(top = 12.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
                    SectionCard("Hardware") { Details(info.value) }
                    if (!model.lite) SectionCard("Resource details") { Details(resources.value) }
                }
            }
        }
    }
}

@Composable
private fun InfoLine(label: String, value: String) {
    Row(Modifier.fillMaxWidth().padding(vertical = 4.dp), horizontalArrangement = Arrangement.spacedBy(16.dp)) {
        Text(label, Modifier.weight(1f), style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
        Text(value, Modifier.weight(1.3f), style = MaterialTheme.typography.titleSmall, textAlign = androidx.compose.ui.text.style.TextAlign.End)
    }
}
@Composable
fun AppsScreen(model: WendyModel, openURL: (String) -> Unit) {
    val state = rememberRemote(model, "Apps")
    var deletion by remember { mutableStateOf<String?>(null) }
    var logs by remember { mutableStateOf<String?>(null) }
    var ports by remember { mutableStateOf<String?>(null) }
    if (logs != null) { LogsScreen(model, logs!!) { logs = null }; return }
    if (ports != null) { Column { TextButton(onClick = { ports = null }) { Text("Back to apps") }; DataPage(model, "App ports", "Ports", ports!!, model.session) }; return }
    LazyColumn(contentPadding = PaddingValues(20.dp), verticalArrangement = Arrangement.spacedBy(16.dp)) {
        item { PageTitle("Apps", "Manage the applications running on this device"); RemoteStatus(state); TextButton(onClick = state::refresh) { Text("Refresh") } }
        if (!state.loading && state.value.items().isEmpty()) item { EmptyMessage("No apps installed", "Deploy an app with the Wendy CLI to see it here.") }
        items(state.value.items()) { app ->
            val name = app.text("app_name")
            SectionCard(name) {
                Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.SpaceBetween) {
                    StatusPill(when (app.number("running_state").toInt()) { 0 -> "Stopped"; 1 -> "Running"; 2 -> "Crash looping"; else -> "Unknown" }, active = app.number("running_state") == 1L)
                    Text(app.text("app_version"), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                }
                app.list("services").forEach { service -> Text("${service.text("name")} · ${when (service.number("running_state")) { 0L -> "Stopped"; 1L -> "Running"; 2L -> "Restarting after a crash"; else -> "Status unavailable" }}", style = MaterialTheme.typography.bodySmall) }
                Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    if (app.number("running_state") != 1L) Button(onClick = { model.task { model.backend.call("StartApp", name, model.session); model.record("Started $name"); state.refresh() } }) { Icon(Icons.Outlined.PlayArrow, null, Modifier.size(18.dp)); Spacer(Modifier.width(6.dp)); Text("Start app") }
                    else OutlinedButton(onClick = { model.task { model.backend.call("StopApp", name, model.session); model.record("Stopped $name"); state.refresh() } }) { Icon(Icons.Outlined.Stop, null, Modifier.size(18.dp)); Spacer(Modifier.width(6.dp)); Text("Stop app") }
                    TextButton(onClick = { logs = name }) { Text("Logs") }
                }
                Row {
                    if (app.number("http_port") != 0L) TextButton(onClick = { model.task { openURL(model.backend.call("OpenApp", name, model.session) as String) } }) { Text("Open app") }
                    TextButton(onClick = { ports = name }) { Text("Ports") }
                    TextButton(onClick = { deletion = name }) { Text("Delete", color = MaterialTheme.colorScheme.error) }
                }
            }
        }
    }
    deletion?.let { name -> ConfirmDialog("Delete $name?", "The app and its containers will be removed from this device.", { deletion = null }) { deletion = null; model.task { model.backend.call("DeleteApp", name, model.session); model.record("Deleted $name"); state.refresh() } } }
}
@Composable
fun LogsScreen(model: WendyModel, app: String, back: () -> Unit) {
    var filter by remember { mutableStateOf("") }
    val stream = remember { model.nextID() }
    LaunchedEffect(stream) { model.beginLogs(stream); try { model.backend.call("StartLogs", app, stream) } catch (e: Exception) { model.error = e.message } }
    DisposableEffect(stream) { onDispose { model.stopLogs(stream) } }
    Column(Modifier.padding(16.dp)) {
        TextButton(onClick = back) { Text("Back to apps") }
        PageTitle("$app logs")
        OutlinedTextField(filter, { filter = it }, label = { Text("Filter logs") }, modifier = Modifier.fillMaxWidth())
        LazyColumn(verticalArrangement = Arrangement.spacedBy(8.dp), modifier = Modifier.weight(1f)) {
            items(model.logs.filter { it.text("Body", "Service").contains(filter, true) }) { log -> SelectionContainer { Text("${log.text("Service")} ${log.text("Body")}", fontFamily = FontFamily.Monospace, style = MaterialTheme.typography.bodySmall) } }
        }
    }
}

@Composable
fun SitesScreen(model: WendyModel) {
    if (model.account.isEmpty() || model.organization.isEmpty()) {
        Column(Modifier.padding(24.dp)) { PageTitle("Your sites", "One place for every location."); EmptyMessage("The bigger picture", "Choose an account and organization above to explore your fleet.", Icons.Outlined.Language) }; return
    }
    val fleet = rememberRemote(model, "Fleet", model.account, model.organization)
    var organization by remember { mutableStateOf(false) }
    if (organization) { Column { TextButton(onClick = { organization = false }) { Text("Back to sites") }; OrganizationScreen(model) }; return }
    LazyColumn(contentPadding = PaddingValues(20.dp), verticalArrangement = Arrangement.spacedBy(14.dp)) {
        item { PageTitle("Your sites", "One place for every location."); RemoteStatus(fleet) }
        item { SettingsRow("Organization", model.organizations.firstOrNull { it.text("id") == model.organization }?.text("name") ?: "Manage your team and access", Icons.Outlined.Business) { organization = true } }
        item { Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
            Eyebrow("LOCATIONS"); Spacer(Modifier.weight(1f)); IconButton(onClick = fleet::refresh) { Icon(Icons.Outlined.Refresh, "Refresh sites", Modifier.size(20.dp)) }
        } }
        fleet.value.obj().list("Devices").groupBy { it.text("Site").ifBlank { "Unassigned" } }.forEach { (site, devices) ->
            item { SectionCard(site) {
                Text("${devices.size} devices · ${devices.count { it.flag("Online") }} online", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                devices.forEach { device ->
                    Surface(onClick = { model.connect(Device.parse(device).copy(transport = "Cloud")) }, color = MaterialTheme.colorScheme.surface) {
                        Row(Modifier.fillMaxWidth().padding(vertical = 12.dp), horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
                            IconTile(Icons.Outlined.Memory)
                            Text(device.text("Name"), Modifier.weight(1f), style = MaterialTheme.typography.titleSmall)
                            StatusPill(if (device.flag("Online")) "Online" else "Offline", active = device.flag("Online"))
                        }
                    }
                }
            } }
        }
        if (!fleet.loading && fleet.value.obj().list("Devices").isEmpty()) item { EmptyMessage("No sites yet", "Enrolled devices appear here when your organization has devices.") }
    }
}
