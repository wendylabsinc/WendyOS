package dev.wendy.android

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.Intent
import android.net.VpnService
import android.os.ParcelFileDescriptor
import android.util.Base64
import kotlinx.coroutines.*
import kotlinx.coroutines.flow.MutableStateFlow
import org.json.JSONArray
import java.io.FileInputStream
import java.io.FileOutputStream

/** An explicit split tunnel. Only selected Wendy mesh/cloud routes enter it. */
class WendyVpnService : VpnService() {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.IO)
    private val cleaned = java.util.concurrent.atomic.AtomicBoolean(false)
    private var tunnel: ParcelFileDescriptor? = null
    private var job: Job? = null
    private val backend get() = (application as WendyApplication).backend
    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == STOP) { stopSelf(); return START_NOT_STICKY }
        if (job != null) return START_NOT_STICKY
        val manager = getSystemService(NotificationManager::class.java)
        manager.createNotificationChannel(NotificationChannel("network", "Wendy network", NotificationManager.IMPORTANCE_LOW))
        val stop = PendingIntent.getService(this, 0, Intent(this, WendyVpnService::class.java).setAction(STOP), PendingIntent.FLAG_IMMUTABLE)
        startForeground(1, Notification.Builder(this, "network").setSmallIcon(R.drawable.ic_wendy_foreground).setContentTitle("Wendy network").setContentText("Connecting selected devices").addAction(Notification.Action.Builder(null, "Disconnect", stop).build()).build())
        engaged = true
        val routes = intent?.getStringExtra("routes") ?: "[]"
        job = scope.launch {
            try {
                val config = backend.call("StartVPN", JSONArray(routes)).obj()
                val builder = Builder().setSession("Wendy network").setMtu(1280).addAddress("100.64.0.54", 32).addDnsServer("100.64.0.53").addRoute("100.64.0.53", 32)
                    .addDisallowedApplication(packageName).setBlocking(true)
                config.list("routes").forEach { route -> builder.addRoute(route.text("address"), 32) }
                val descriptor = builder.establish() ?: error("VPN permission was not granted")
                tunnel = descriptor; running.value = true; failure.value = null
                startForeground(1, Notification.Builder(this@WendyVpnService, "network").setSmallIcon(R.drawable.ic_wendy_foreground).setContentTitle("Wendy network connected").setContentText("Routing selected Wendy devices").addAction(Notification.Action.Builder(null, "Disconnect", stop).build()).build())
                coroutineScope {
                    launch {
                        val input = FileInputStream(descriptor.fileDescriptor)
                        val buffer = ByteArray(65535)
                        try { while (isActive) { val size = input.read(buffer); if (size < 0) error("VPN interface closed"); if (size > 0) backend.call("VPNWrite", Base64.encodeToString(buffer, 0, size, Base64.NO_WRAP)) } } finally { descriptor.close() }
                    }
                    launch {
                        val output = FileOutputStream(descriptor.fileDescriptor)
                        try { while (isActive) { val encoded = backend.call("VPNRead") as? String; if (!encoded.isNullOrEmpty()) output.write(Base64.decode(encoded, Base64.DEFAULT)) } } finally { descriptor.close() }
                    }
                }
            } catch (e: CancellationException) { throw e }
            catch (e: Exception) { failure.value = e.message ?: "VPN connection failed" }
            finally { withContext(NonCancellable) { cleanup(); stopSelf() } }
        }
        return START_NOT_STICKY
    }
    private suspend fun cleanup() {
        if (!cleaned.compareAndSet(false, true)) return
        running.value = false; engaged = false
        try { tunnel?.close() } catch (_: Exception) {}; tunnel = null
        try { backend.call("StopVPN"); if (!(application as WendyApplication).visible) backend.call("SetMobileState", false, "", "") } catch (_: Exception) {}
    }
    override fun onRevoke() { stopSelf() }
    override fun onDestroy() {
        try { tunnel?.close() } catch (_: Exception) {}
        job?.cancel()
        scope.launch { job?.join(); cleanup(); scope.cancel() }
        super.onDestroy()
    }
    companion object {
        const val STOP = "dev.wendy.android.STOP_VPN"
        @Volatile var engaged = false
        val running = MutableStateFlow(false)
        val failure = MutableStateFlow<String?>(null)
    }
}
