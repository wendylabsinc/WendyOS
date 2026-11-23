package dev.wendy.android

import android.annotation.SuppressLint
import android.webkit.WebResourceRequest
import android.webkit.WebView
import android.webkit.WebViewClient
import androidx.compose.foundation.layout.*
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.viewinterop.AndroidView
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties

/** Device web apps stay in this Activity so their authenticated forward stays alive. */
@SuppressLint("SetJavaScriptEnabled")
@Composable fun AppBrowser(url: String, close: () -> Unit, openExternal: (String) -> Unit) {
    var browser by remember { mutableStateOf<WebView?>(null) }
    DisposableEffect(url) { onDispose { browser?.stopLoading(); browser?.destroy() } }
    Dialog(onDismissRequest = close, properties = DialogProperties(usePlatformDefaultWidth = false)) {
        Surface(Modifier.fillMaxSize()) { Column(Modifier.safeDrawingPadding()) {
            Row { TextButton(onClick = { browser?.goBack() }, enabled = browser?.canGoBack() == true) { Text("Back") }; TextButton(onClick = { browser?.reload() }) { Text("Reload") }; Spacer(Modifier.weight(1f)); TextButton(onClick = close) { Text("Close app") } }
            AndroidView(factory = { context -> WebView(context).apply {
                browser = this
                settings.javaScriptEnabled = true
                settings.domStorageEnabled = true
                settings.allowFileAccess = false
                settings.allowContentAccess = false
                settings.mixedContentMode = android.webkit.WebSettings.MIXED_CONTENT_NEVER_ALLOW
                val origin = android.net.Uri.parse(url)
                webViewClient = object : WebViewClient() {
                    override fun shouldOverrideUrlLoading(view: WebView, request: WebResourceRequest): Boolean {
                        val next = request.url
                        if (next.scheme == origin.scheme && next.host == origin.host && next.port == origin.port) return false
                        if (request.isForMainFrame) openExternal(next.toString())
                        return true
                    }
                }
                loadUrl(url)
            } }, modifier = Modifier.weight(1f).fillMaxWidth())
        } }
    }
}
