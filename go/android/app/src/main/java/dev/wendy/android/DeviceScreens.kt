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
