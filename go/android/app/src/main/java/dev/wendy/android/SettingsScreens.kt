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
