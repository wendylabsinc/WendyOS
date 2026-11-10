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
