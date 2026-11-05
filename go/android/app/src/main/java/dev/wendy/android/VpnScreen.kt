package dev.wendy.android
import android.content.Intent
import android.net.VpnService
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import androidx.core.content.ContextCompat
import org.json.JSONArray

@Composable fun VpnScreen(model: WendyModel, done: () -> Unit) {
    val context = LocalContext.current
    val running by WendyVpnService.running.collectAsState()
    val failure by WendyVpnService.failure.collectAsState()
    val devices = rememberRemote(model, "Cloud")
    val chosen = remember { mutableStateMapOf<String, Boolean>() }
    fun start() {
        val routes = devices.value.items().map(Device::parse).filter { chosen[it.selector] == true }.map { json("selector" to it.selector, "name" to it.name) }
        ContextCompat.startForegroundService(context, Intent(context, WendyVpnService::class.java).putExtra("routes", JSONArray(routes).toString()))
    }
    val permission = rememberLauncherForActivityResult(ActivityResultContracts.StartActivityForResult()) { if (it.resultCode == android.app.Activity.RESULT_OK) start() }
    Column(Modifier.verticalScroll(rememberScrollState()).padding(20.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
        TextButton(onClick = done) { Text("Back to devices") }
        PageTitle("Wendy network", "Make selected devices available to other apps through a private VPN.")
        failure?.let { Text(it, color = MaterialTheme.colorScheme.error) }
        if (running) Button(onClick = { context.startService(Intent(context, WendyVpnService::class.java).setAction(WendyVpnService.STOP)) }) { Text("Disconnect VPN") }
        else {
            RemoteStatus(devices)
            devices.value.items().map(Device::parse).forEach { device -> Row { Text(device.name, Modifier.weight(1f)); Checkbox(chosen[device.selector] == true, { chosen[device.selector] = it }) } }
            Button(enabled = chosen.values.any { it }, onClick = { val intent = VpnService.prepare(context); if (intent == null) start() else permission.launch(intent) }) { Text("Connect VPN") }
        }
    }
}
