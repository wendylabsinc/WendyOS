package dev.wendy.android

import android.content.Intent
import androidx.core.content.ContextCompat
import androidx.test.core.app.ActivityScenario
import androidx.test.core.app.ApplicationProvider
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import kotlinx.coroutines.*
import kotlinx.coroutines.flow.first
import org.junit.Assert.*
import org.junit.Test
import org.junit.runner.RunWith
import java.net.URL
import javax.net.ssl.HttpsURLConnection

@RunWith(AndroidJUnit4::class)
class AuthSessionServiceTest {
    @Test fun signInServiceKeepsNetworkAccessOutsideActivityAndStops(): Unit = runBlocking {
        val app = ApplicationProvider.getApplicationContext<WendyApplication>()
        val instrumentation = InstrumentationRegistry.getInstrumentation()
        ActivityScenario.launch(MainActivity::class.java).use { scenario ->
            try {
                scenario.onActivity {
                    ContextCompat.startForegroundService(it, Intent(it, AuthSessionService::class.java))
                }
                withTimeout(5_000) { AuthSessionService.running.first { it } }
                // Match the OAuth handoff: another app is foreground, Wendy still needs HTTPS.
                app.startActivity(Intent(Intent.ACTION_MAIN).addCategory(Intent.CATEGORY_HOME).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
                withTimeout(5_000) { while (app.visible) delay(50) }
                delay(2_000)
                assertTrue(AuthSessionService.running.value)
                withContext(Dispatchers.IO) {
                    val connection = URL("https://auth.wendy.dev/.well-known/openid-configuration").openConnection() as HttpsURLConnection
                    try {
                        connection.connectTimeout = 15_000
                        connection.readTimeout = 15_000
                        // The realm-less path may return 404; DNS and TLS must still succeed.
                        assertTrue(connection.responseCode in 200..499)
                    } finally { connection.disconnect() }
                }
                instrumentation.runOnMainSync { AuthSessionService.complete() }
                withTimeout(5_000) { AuthSessionService.running.first { !it } }
                app.startActivity(Intent(app, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
                withTimeout(5_000) { while (!app.visible) delay(50) }
                scenario.onActivity {
                    ContextCompat.startForegroundService(it, Intent(it, AuthSessionService::class.java))
                }
                withTimeout(5_000) { AuthSessionService.running.first { it } }
                instrumentation.runOnMainSync { AuthSessionService.cancelLogin() }
                withTimeout(5_000) { AuthSessionService.running.first { !it } }
            } finally {
                instrumentation.runOnMainSync { AuthSessionService.cancelLogin() }
                app.startActivity(Intent(app, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK))
            }
        }
    }
}
