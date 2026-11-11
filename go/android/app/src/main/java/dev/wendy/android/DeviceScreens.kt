package dev.wendy.android

import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.material3.*
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.launch
import java.text.DateFormat
import java.util.Date

data class FleetDevice(val id: String, val name: String, val selector: String, val siteID: String, val site: String, val hardware: String, val address: String, val online: Boolean, val lite: Boolean = false) {
    val siteKey get() = siteID.ifEmpty { site }
    val device get() = Device(selector, name, selector, "Cloud", lite)
}

/** The organization's fleet grouped by site (iOS SitesView on iPhone). */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun SitesScreen(model: WendyModel) {
    val context = LocalContext.current
    val preferences = remember { context.getSharedPreferences("sites", 0) }
    val scope = rememberCoroutineScope()
    var devices by remember { mutableStateOf<List<FleetDevice>?>(null) }
    var presenceError by remember { mutableStateOf("") }
    var error by remember { mutableStateOf<String?>(null) }
    var loading by remember { mutableStateOf(false) }
    var updated by remember { mutableStateOf<Long?>(null) }
    var search by remember { mutableStateOf("") }
    var onlineOnly by remember { mutableStateOf(preferences.getBoolean("onlineOnly", true)) }
    var refreshing by remember { mutableStateOf(false) }
    suspend fun refresh() {
        if (model.account.isEmpty() || model.organization.isEmpty()) { devices = null; return }
        val account = model.account; val organization = model.organization
        loading = true; error = null
        try {
            val value = model.backend.call("Fleet", account, organization).obj()
            if (account != model.account || organization != model.organization) return
            devices = value.list("Devices").map { FleetDevice(it.text("ID"), it.text("Name"), it.text("Selector"), it.text("SiteID"), it.text("Site"), it.text("Hardware"), it.text("Address"), it.flag("Online"), it.flag("Lite")) }
            presenceError = value.text("PresenceError"); updated = System.currentTimeMillis()
        } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { loading = false }
    }
    LaunchedEffect(model.account, model.organization) { devices = null; updated = null; refresh() }
    val presenceKnown = presenceError.isEmpty()
    val visible = devices.orEmpty().filter { (!onlineOnly || !presenceKnown || it.online) && (search.isEmpty() || listOf(it.name, it.site, it.hardware).any { v -> v.contains(search, true) }) }
        .sortedWith(compareBy(String.CASE_INSENSITIVE_ORDER, FleetDevice::name).thenBy { it.id })
    val groups = visible.groupBy { it.siteKey }.map { (key, values) -> Triple(key, values.first().site.ifEmpty { "No site reported" }, values) }.sortedBy { it.second.lowercase() }
    PullToRefreshBox(refreshing, onRefresh = { scope.launch { refreshing = true; refresh(); refreshing = false } }) {
        PageColumn {
            item { SecondaryText("Your fleet, across every site in your organization.") }
            if (model.accounts.isEmpty()) {
                item { EmptyMessage("Sign in to view your fleet", "Your organization's devices appear here, grouped by site.", Icons.Outlined.Business) }
                item { Button(onClick = { model.signIn() }, Modifier.fillMaxWidth(), enabled = !model.loginPending) { Text("Sign in") } }
                return@PageColumn
            }
            devices?.let { all ->
                formSection {
                    Row(Modifier.fillMaxWidth().padding(vertical = 8.dp)) {
                        Metric("Devices", all.size, MaterialTheme.colorScheme.onSurface, Modifier.weight(1f))
                        Metric("Sites", all.filter { it.siteKey.isNotEmpty() }.map { it.siteKey }.toSet().size, MaterialTheme.colorScheme.secondary, Modifier.weight(1f))
                        if (presenceKnown) Metric("Online", all.count { it.online }, MaterialTheme.colorScheme.primary, Modifier.weight(1f))
                    }
                    ToggleRow("Online only", onlineOnly, enabled = presenceKnown) { onlineOnly = it; preferences.edit().putBoolean("onlineOnly", it).apply() }
                }
            }
            item { OutlinedTextField(search, { search = it }, Modifier.fillMaxWidth(), singleLine = true, placeholder = { Text("Filter fleet") }, leadingIcon = { Icon(Icons.Outlined.Search, null) }) }
            error?.let { item { FormSection { FailureNote(it) } } }
            if (!presenceKnown) item { FormSection { FailureNote("Device presence is unavailable. $presenceError") } }
            if (loading && devices == null) item { LoadingRow("Loading your fleet…") }
            if (devices != null && visible.isEmpty() && !loading && error == null) item {
                EmptyMessage(if (search.isEmpty()) "No matching devices" else "No results for “$search”", "Devices enrolled in this organization appear here, including offline devices.", Icons.Outlined.Memory)
            }
            groups.forEach { (_, name, members) ->
                formSection("$name · ${members.size}") {
                    members.forEach { device ->
                        val connecting = model.connecting && model.selected?.selector == device.selector
                        Row(Modifier.fillMaxWidth().clickable { model.connect(device.device) }.padding(vertical = 8.dp), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                            Icon(Icons.Outlined.Memory, null, tint = MaterialTheme.colorScheme.onSurfaceVariant)
                            Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(2.dp)) {
                                Text(device.name, style = MaterialTheme.typography.titleSmall)
                                SecondaryText(device.hardware.ifEmpty { "Wendy device" })
                                Text(if (presenceKnown) (if (device.online) "Online" else "Offline") else "Status unknown", style = MaterialTheme.typography.bodySmall,
                                    color = if (presenceKnown && device.online) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurfaceVariant)
                            }
                            if (connecting) CircularProgressIndicator(Modifier.size(18.dp), strokeWidth = 2.dp) else Icon(Icons.Outlined.ChevronRight, null, tint = MaterialTheme.colorScheme.onSurfaceVariant)
                        }
                    }
                }
            }
            updated?.let { item { SecondaryText("Updated ${DateFormat.getTimeInstance(DateFormat.SHORT).format(Date(it))}") } }
        }
    }
}

@Composable
private fun Metric(title: String, count: Int, color: Color, modifier: Modifier) {
    Column(modifier, verticalArrangement = Arrangement.spacedBy(2.dp)) {
        Text(count.toString(), style = MaterialTheme.typography.headlineSmall, color = color)
        Text(title, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}
