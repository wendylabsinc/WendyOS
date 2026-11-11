package dev.wendy.android

import android.annotation.SuppressLint
import android.graphics.Bitmap
import android.webkit.WebChromeClient
import android.webkit.WebResourceError
import android.webkit.WebResourceRequest
import android.webkit.WebView
import android.webkit.WebViewClient
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.Public
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties

/**
 * A device app's dashboard (iOS MobileAppBrowser). It stays in this Activity so the authenticated
 * forward stays alive; links to other origins open outside the app.
 */
@SuppressLint("SetJavaScriptEnabled")
@Composable fun AppBrowser(url: String, app: String?, device: String?, close: () -> Unit, openExternal: (String) -> Unit, openOutside: (String) -> Unit) {
    var browser by remember { mutableStateOf<WebView?>(null) }
    var address by remember(url) { mutableStateOf(url) }
    var progress by remember { mutableIntStateOf(0) }
    var loading by remember { mutableStateOf(true) }
    var error by remember { mutableStateOf<String?>(null) }
    DisposableEffect(url) { onDispose { browser?.stopLoading(); browser?.destroy() } }
    Dialog(onDismissRequest = close, properties = DialogProperties(usePlatformDefaultWidth = false)) {
        Surface(Modifier.fillMaxSize(), color = MaterialTheme.colorScheme.background) { Column(Modifier.safeDrawingPadding()) {
            Row(Modifier.fillMaxWidth().background(MaterialTheme.colorScheme.surface).padding(horizontal = 12.dp, vertical = 6.dp), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                TextButton(onClick = close) { Text("Done", fontWeight = FontWeight.SemiBold) }
                Text(address, Modifier.weight(1f).background(MaterialTheme.colorScheme.surfaceVariant, CircleShape).padding(horizontal = 12.dp, vertical = 8.dp),
                    style = MaterialTheme.typography.labelMedium, maxLines = 1, overflow = TextOverflow.MiddleEllipsis)
                IconButton(onClick = { openOutside(address) }) { Icon(Icons.Outlined.Public, "Open in browser") }
            }
            Column(Modifier.fillMaxWidth().padding(horizontal = 18.dp, vertical = 12.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                Text("Dashboard", style = MaterialTheme.typography.titleLarge, fontWeight = FontWeight.Bold)
                val subtitle = listOfNotNull(app, device).joinToString(" · ")
                if (subtitle.isNotEmpty()) Text(subtitle, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
            if (loading && error == null) LinearProgressIndicator(progress = { progress / 100f }, Modifier.fillMaxWidth().height(2.dp))
            Box(Modifier.weight(1f).fillMaxWidth()) {
                AndroidView(factory = { context -> WebView(context).apply {
                    browser = this
                    settings.javaScriptEnabled = true
                    settings.domStorageEnabled = true
                    settings.allowFileAccess = false
                    settings.allowContentAccess = false
                    settings.mixedContentMode = android.webkit.WebSettings.MIXED_CONTENT_NEVER_ALLOW
                    val origin = android.net.Uri.parse(url)
                    webChromeClient = object : WebChromeClient() { override fun onProgressChanged(view: WebView, value: Int) { progress = value } }
                    webViewClient = object : WebViewClient() {
                        override fun shouldOverrideUrlLoading(view: WebView, request: WebResourceRequest): Boolean {
                            val next = request.url
                            if (next.scheme == origin.scheme && next.host == origin.host && next.port == origin.port) return false
                            if (request.isForMainFrame) openExternal(next.toString())
                            return true
                        }
                        override fun onPageStarted(view: WebView, started: String, favicon: Bitmap?) { loading = true; address = started }
                        override fun onPageFinished(view: WebView, finished: String) { loading = false; address = finished }
                        override fun onReceivedError(view: WebView, request: WebResourceRequest, failure: WebResourceError) {
                            if (request.isForMainFrame) { error = failure.description?.toString()?.ifEmpty { null } ?: "The page could not be loaded."; loading = false }
                        }
                    }
                    loadUrl(url)
                } }, modifier = Modifier.fillMaxSize())
                error?.let { message ->
                    Surface(Modifier.fillMaxSize(), color = MaterialTheme.colorScheme.background) {
                        Column(Modifier.padding(24.dp), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(12.dp, Alignment.CenterVertically)) {
                            Icon(Icons.Outlined.Public, null, Modifier.size(40.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
                            Text("Couldn’t load web page", style = MaterialTheme.typography.titleMedium)
                            Text(message, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                            Button(onClick = { error = null; browser?.reload() }) { Text("Try Again") }
                        }
                    }
                }
            }
        } }
    }
}
