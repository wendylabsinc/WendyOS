package dev.wendy.android

import android.Manifest
import android.os.Build
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import org.json.JSONObject

@Composable
fun NearbySetupScreen(model: WendyModel, done: () -> Unit) {
    var devices by remember { mutableStateOf(emptyList<JSONObject>()) }
    var setup by remember { mutableStateOf(JSONObject()) }
    var wifi by remember { mutableStateOf(false) }
    var networkName by remember { mutableStateOf("") }
    var name by remember { mutableStateOf("Wendy device") }
    val session = setup.number("Session")
    fun scan() = model.task { devices = model.backend.call("NearbyDiscover").items() }
    val permissions = rememberLauncherForActivityResult(ActivityResultContracts.RequestMultiplePermissions()) { result -> if (result.values.all { it }) scan() else model.error = "Nearby device access is needed for Bluetooth setup." }
    DisposableEffect(session) { onDispose { if (session != 0L) model.task { model.backend.call("NearbyClose", session) } } }
    fun action(code: Int, ssid: String = "", password: String = "") = model.task {
        setup = model.backend.call("NearbyAction", session, code, ssid, password, model.account, model.organization, name).obj()
    }
    Column(Modifier.verticalScroll(rememberScrollState()).padding(20.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
        TextButton(onClick = done) { Text("Back to devices") }
        PageTitle("Set up a nearby device", "Turn on the device and keep it close to your phone.")
        if (session == 0L) {
            Button(onClick = { permissions.launch(if (Build.VERSION.SDK_INT >= 31) arrayOf(Manifest.permission.BLUETOOTH_SCAN, Manifest.permission.BLUETOOTH_CONNECT) else arrayOf(Manifest.permission.ACCESS_FINE_LOCATION)) }) { Text("Scan with Bluetooth") }
            devices.forEach { device -> OutlinedButton(onClick = { model.task { setup = model.backend.call("NearbyOpen", device.text("Address")).obj(); name = device.text("Name").ifBlank { "Wendy device" } } }) { Text(device.text("Name", "Address")) } }
        } else {
            TextButton(onClick = { model.task { model.backend.call("NearbyClose", session); setup = JSONObject() } }) { Text("Restart setup") }
            SectionCard("Setup progress") {
                SummaryLine("Device", if (setup.flag("Connected")) "Connected" else "Waiting for connection")
                SummaryLine("Wi-Fi", if (setup.flag("WiFiConnected")) setup.text("SSID").ifBlank { "Connected" } else "Not connected")
                SummaryLine("Wendy Cloud", when { setup.flag("Enrolled") || setup.flag("Completed") -> "Enrolled"; setup.flag("Pending") -> "Enrollment in progress"; else -> "Not enrolled" })
                OptionalMessage(setup.text("Failure"))
            }
            setup.optJSONArray("Networks")?.let { networks ->
                if (networks.length() > 0) SectionCard("Available Wi-Fi") {
                    for (index in 0 until networks.length()) {
                        val ssid = networks.optString(index)
                        TextButton(onClick = { networkName = ssid; wifi = true }) { Text(ssid) }
                    }
                }
            }
            TextButton(onClick = { action(0) }) { Text("Refresh status") }
            TextButton(onClick = { action(1) }) { Text("Scan Wi-Fi networks") }
            Button(onClick = { wifi = true }) { Text("Connect Wi-Fi") }
            OutlinedTextField(name, { name = it }, label = { Text("Device name") })
            if (model.account.isBlank()) Text("Sign in with the account button to enroll this device.")
            else Button(onClick = { action(3) }, enabled = !setup.flag("Enrolled") && !setup.flag("Pending")) { Text("Enroll in selected organization") }
            if (setup.flag("Pending") || setup.flag("CanResume")) Button(onClick = { action(4) }) { Text("Check enrollment") }
        }
    }
    if (wifi) InputDialog("Connect device Wi-Fi", listOf(Field("ssid", "Network name", networkName), Field("password", "Password", secret = true)), { wifi = false }) { values -> wifi = false; action(2, values.getValue("ssid"), values.getValue("password")) }
}
