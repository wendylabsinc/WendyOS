package dev.wendy.android

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Intent
import android.net.VpnService
import android.os.ParcelFileDescriptor
import android.system.OsConstants
import android.util.Base64
import kotlinx.coroutines.*
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import org.json.JSONObject

internal enum class VpnPhase { Disconnected, Connecting, Connected, Disconnecting }
internal data class VpnRoute(val name: String, val address: String, val host: String)

/** The same automatic mesh controller and internet backup policy as iOS. */
class WendyVpnService : VpnService() {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    @Volatile private var job: Job? = null
    @Volatile private var nativeCancellation: Job? = null
    private val backend get() = (application as WendyApplication).backend

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == STOP) { disconnect(); return START_NOT_STICKY }
        if (job != null) return START_NOT_STICKY
        if (intent?.action != START) { stopSelf(); return START_NOT_STICKY }
        val account = intent.getStringExtra(ACCOUNT).orEmpty()
        val organization = intent.getStringExtra(ORGANIZATION).orEmpty()
        getSystemService(NotificationManager::class.java).createNotificationChannel(
            NotificationChannel("network", "Wendy network", NotificationManager.IMPORTANCE_LOW))
        startForeground(1, notification("Connecting to mesh"))
        job = scope.launch(start = CoroutineStart.UNDISPATCHED) {
            // A replacement Service cannot start its engine until the previous
            // instance has drained its packet workers and released the backend.
            sessionLock.withLock {
                phase.value = VpnPhase.Connecting
                failure.value = null
                var tunnel: ParcelFileDescriptor? = null
                try {
                    backend.initialize()
                    var config = backend.call("StartVPN", account, organization).obj()
                    while (isActive) {
                        ensureActive()
                        val settings = VpnNetworkSettings.parse(config)
                        val next = establishTunnel(settings)
                        tunnel?.close()
                        tunnel = next
                        activeRoutes.value = config.list("routes").map { VpnRoute(it.text("name"), it.text("address"), it.text("host")) }
                        internetBackup.value = settings.internetBackup
                        phase.value = VpnPhase.Connected
                        startForeground(1, notification(if (settings.internetBackup) "Connected · Using mesh internet backup" else "Connected to mesh"))
                        config = awaitConfiguration(next)
                    }
                } catch (e: CancellationException) { throw e }
                catch (e: Exception) { failure.value = e.message ?: "VPN connection failed" }
                finally {
                    phase.value = VpnPhase.Disconnecting
                    // Packet workers have stopped before their descriptor is
                    // closed, avoiding blocked reads and descriptor reuse races.
                    withContext(NonCancellable) {
                        try { tunnel?.close() } catch (_: Exception) {}
                        try {
                            nativeCancellation?.join()
                            backend.call("StopVPN")
                            if (!(application as WendyApplication).visible) backend.call("SetMobileState", false, "", "")
                        } catch (e: Exception) { failure.value = e.message ?: "VPN cleanup failed" }
                        activeRoutes.value = emptyList()
                        internetBackup.value = false
                        phase.value = VpnPhase.Disconnected
                        stopForeground(STOP_FOREGROUND_REMOVE)
                        stopSelf()
                    }
                }
            }
        }
        return START_NOT_STICKY
    }

    private fun establishTunnel(settings: VpnNetworkSettings): ParcelFileDescriptor {
        val configure = PendingIntent.getActivity(this, 1,
            Intent(this, MainActivity::class.java).setAction(OPEN), PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE)
        val builder = Builder().setSession("Wendy mesh").setMtu(settings.mtu)
            .addAddress(settings.address4, 32).addDnsServer("100.64.0.53")
            // The embedded broker, resolver and reachability probes must use
            // the physical network even while the mesh supplies internet.
            .addDisallowedApplication(packageName).setConfigureIntent(configure).setBlocking(false)
        if (settings.address6 != null) builder.addAddress(settings.address6, 128)
        else builder.allowFamily(OsConstants.AF_INET6)
        settings.routes.forEach { (address, prefix) -> builder.addRoute(address, prefix) }
        return builder.establish() ?: error("VPN permission was not granted")
    }

    private suspend fun awaitConfiguration(tunnel: ParcelFileDescriptor): JSONObject = coroutineScope {
        val pump = launch {
            pumpVpnPackets(tunnel.fileDescriptor,
                receive = { (backend.call("VPNRead") as? String)?.let { Base64.decode(it, Base64.DEFAULT) } },
                send = { backend.call("VPNWrite", Base64.encodeToString(it, Base64.NO_WRAP)); Unit })
        }
        try {
            var next: JSONObject? = null
            while (next == null) {
                delay(5000)
                next = backend.call("RefreshVPN") as? JSONObject
            }
            next
        } finally { withContext(NonCancellable) { pump.cancelAndJoin() } }
    }

    private fun notification(detail: String): Notification {
        val stop = PendingIntent.getService(this, 0, Intent(this, WendyVpnService::class.java).setAction(STOP), PendingIntent.FLAG_IMMUTABLE)
        val open = PendingIntent.getActivity(this, 1, Intent(this, MainActivity::class.java).setAction(OPEN), PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE)
        return Notification.Builder(this, "network").setSmallIcon(R.drawable.ic_wendy_foreground)
            .setContentTitle("Wendy mesh VPN").setContentText(detail).setContentIntent(open).setOngoing(true)
            .addAction(Notification.Action.Builder(null, "Disconnect", stop).build()).build()
    }

    private fun cancelNativeRefresh() {
        if (nativeCancellation == null && phase.value != VpnPhase.Disconnected) {
            nativeCancellation = scope.launch(NonCancellable) {
                try { backend.call("CancelVPN") } catch (_: Exception) {}
            }
        }
    }
    private fun disconnect() {
        cancelNativeRefresh()
        job?.cancel()
        stopSelf()
    }
    override fun onRevoke() { mainExecutor.execute { disconnect() } }
    override fun onDestroy() { cancelNativeRefresh(); scope.cancel(); super.onDestroy() }

    companion object {
        const val STOP = "dev.wendy.android.STOP_VPN"
        const val OPEN = "dev.wendy.android.OPEN_VPN"
        const val START = "dev.wendy.android.START_VPN"
        const val ACCOUNT = "account"
        const val ORGANIZATION = "organization"
        private val sessionLock = Mutex()
        internal val phase = MutableStateFlow(VpnPhase.Disconnected)
        internal val internetBackup = MutableStateFlow(false)
        internal val activeRoutes = MutableStateFlow<List<VpnRoute>>(emptyList())
        val engaged: Boolean get() = phase.value != VpnPhase.Disconnected
        val failure = MutableStateFlow<String?>(null)
    }
}
