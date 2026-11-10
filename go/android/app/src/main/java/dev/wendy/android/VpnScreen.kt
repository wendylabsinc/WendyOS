package dev.wendy.android

import android.Manifest
import android.app.Activity
import android.content.Intent
import android.net.VpnService
import android.os.Build
import android.content.pm.PackageManager
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.Lan
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp
import androidx.core.content.ContextCompat
import kotlinx.coroutines.delay
import org.json.JSONObject

@Composable fun VpnScreen(model: WendyModel, done: () -> Unit) {
    val context = LocalContext.current
    val phase by WendyVpnService.phase.collectAsState()
    val failure by WendyVpnService.failure.collectAsState()
    val routes by WendyVpnService.activeRoutes.collectAsState()
    val internetBackup by WendyVpnService.internetBackup.collectAsState()
    val nearby = rememberRemote(model, "Nearby")
    val visible = LocalPageVisible.current
    LaunchedEffect(nearby, visible) { while (visible) { delay(5000); if (!nearby.loading) nearby.refresh() } }
    var pending by rememberSaveable { mutableStateOf<String?>(null) }
    var starting by remember { mutableStateOf(false) }
    var localError by rememberSaveable { mutableStateOf<String?>(null) }
    val changing = pending != null || starting || phase == VpnPhase.Connecting || phase == VpnPhase.Disconnecting

    fun start(request: String) {
        try {
            val value = JSONObject(request)
            starting = true
            ContextCompat.startForegroundService(context, Intent(context, WendyVpnService::class.java).setAction(WendyVpnService.START)
                .putExtra(WendyVpnService.ACCOUNT, value.getString("account"))
                .putExtra(WendyVpnService.ORGANIZATION, value.getString("organization")))
        } catch (e: Exception) { starting = false; localError = e.message ?: "Could not start VPN" }
    }
    LaunchedEffect(phase, failure) { if (phase != VpnPhase.Disconnected || failure != null) starting = false }
    val permission = rememberLauncherForActivityResult(ActivityResultContracts.StartActivityForResult()) { result ->
        val request = pending
        pending = null
        if (result.resultCode == Activity.RESULT_OK && request != null) start(request)
        else localError = "VPN permission was not granted. Turn VPN on to try again."
    }
    fun requestVpnPermission(request: String) {
        try {
            val intent = VpnService.prepare(context)
            if (intent == null) { pending = null; start(request) }
            else { pending = request; permission.launch(intent) }
        } catch (e: Exception) { pending = null; localError = e.message ?: "Could not request VPN permission" }
    }
    val bluetooth = rememberLauncherForActivityResult(ActivityResultContracts.RequestMultiplePermissions()) { grants ->
        if (grants.values.any { !it }) localError = "Bluetooth access is off. Mesh will use available network connections."
        pending?.let { requestVpnPermission(it) }
    }
    Column(Modifier.verticalScroll(rememberScrollState()).padding(20.dp), verticalArrangement = Arrangement.spacedBy(16.dp)) {
        PageTitle("Wendy VPN", "All your devices over mesh.")
        SectionCard("Mesh") {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text("VPN", Modifier.weight(1f))
                Switch(checked = starting || pending != null || phase == VpnPhase.Connecting || phase == VpnPhase.Connected,
                    enabled = !changing, onCheckedChange = { enabled ->
                        localError = null
                        if (!enabled) {
                            context.startService(Intent(context, WendyVpnService::class.java).setAction(WendyVpnService.STOP))
                        } else {
                            val request = json("account" to model.account, "organization" to model.organization).toString()
                            val permissions = if (Build.VERSION.SDK_INT >= 31)
                                arrayOf(Manifest.permission.BLUETOOTH_SCAN, Manifest.permission.BLUETOOTH_CONNECT)
                            else arrayOf(Manifest.permission.ACCESS_FINE_LOCATION)
                            if (permissions.any { context.checkSelfPermission(it) != PackageManager.PERMISSION_GRANTED }) {
                                pending = request
                                bluetooth.launch(permissions)
                            } else requestVpnPermission(request)
                        }
                    })
            }
            Text(when {
                pending != null -> "Waiting for VPN permission…"
                starting || phase == VpnPhase.Connecting -> "Connecting…"
                phase == VpnPhase.Disconnecting -> "Disconnecting…"
                phase == VpnPhase.Connected && internetBackup -> "Connected · Using mesh internet backup"
                phase == VpnPhase.Connected -> "Connected"
                else -> "Off"
            }, color = MaterialTheme.colorScheme.onSurfaceVariant)
            if (changing) LinearProgressIndicator(Modifier.fillMaxWidth())
            (localError ?: failure)?.let { FailureNote(it) }
        }
        SectionCard("Automatic internet backup") {
            Text("If your normal internet goes down, Wendy uses an available mesh uplink and switches back when it recovers.")
        }
        SectionCard("All devices over mesh") {
            val names = if (phase == VpnPhase.Connected) routes.map { DeviceDisplayName.title(it.name.ifBlank { "Device" }) }
                else nearby.value.items().map(Device::parse).filter { it.selector.startsWith("mesh:") }.map { it.title }
            names.forEach { name -> Row(horizontalArrangement = Arrangement.spacedBy(10.dp), verticalAlignment = Alignment.CenterVertically) { Icon(Icons.Outlined.Lan, null, Modifier.size(18.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant); Text(name) } }
            if (names.isEmpty()) Text("Mesh devices appear here automatically.", color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}
