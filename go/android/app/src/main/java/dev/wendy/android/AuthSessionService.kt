package dev.wendy.android

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Intent
import android.os.IBinder
import kotlinx.coroutines.*
import kotlinx.coroutines.flow.MutableStateFlow

/** Keeps the loopback OAuth callback and token exchange reachable while the browser is open. */
class AuthSessionService : Service() {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
    private var finished = false
    private var cancelling = false

    override fun onCreate() {
        super.onCreate()
        getSystemService(NotificationManager::class.java).createNotificationChannel(
            NotificationChannel("sign-in", "Wendy sign-in", NotificationManager.IMPORTANCE_LOW)
        )
        val open = PendingIntent.getActivity(this, 0, Intent(this, MainActivity::class.java), PendingIntent.FLAG_IMMUTABLE)
        val cancel = PendingIntent.getService(this, 0, Intent(this, AuthSessionService::class.java).setAction(CANCEL), PendingIntent.FLAG_IMMUTABLE)
        startForeground(2, Notification.Builder(this, "sign-in")
            .setSmallIcon(R.drawable.ic_wendy_foreground)
            .setContentTitle("Signing in to Wendy")
            .setContentText("Complete sign-in in your browser")
            .setContentIntent(open).setOngoing(true)
            .addAction(Notification.Action.Builder(null, "Cancel", cancel).build()).build())
        instance = this
        failure.value = null
        running.value = true
        scope.launch {
            delay(5 * 60 * 1000L)
            cancel("Sign-in timed out. Please try again.")
        }
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == CANCEL) cancel()
        return START_NOT_STICKY
    }

    private fun cancel(message: String? = null) {
        if (finished || cancelling) return
        cancelling = true
        scope.launch {
            try { (application as WendyApplication).backend.call("CancelLogin") }
            catch (e: Exception) { failure.value = e.message }
            finally { if (message != null) failure.value = message; finish() }
        }
    }

    private fun finish() {
        finished = true
        stopForeground(STOP_FOREGROUND_REMOVE)
        stopSelf()
    }

    override fun onDestroy() {
        instance = null
        running.value = false
        scope.cancel()
        if (!finished) {
            // The service can be destroyed independently of its activity.
            CoroutineScope(Dispatchers.IO).launch {
                try { (application as WendyApplication).backend.call("CancelLogin") }
                catch (_: Exception) { }
            }
        }
        super.onDestroy()
    }

    override fun onBind(intent: Intent?): IBinder? = null

    companion object {
        private const val CANCEL = "dev.wendy.android.CANCEL_SIGN_IN"
        private var instance: AuthSessionService? = null
        val running = MutableStateFlow(false)
        val failure = MutableStateFlow<String?>(null)
        fun complete() { instance?.finish() }
        fun cancelLogin() { instance?.cancel() }
    }
}
