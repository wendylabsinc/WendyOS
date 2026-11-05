package dev.wendy.android

import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.util.Base64
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import org.json.JSONObject
import java.nio.ByteBuffer

@Composable fun CamerasScreen(model: WendyModel) {
    val state = rememberRemote(model, "Cameras")
    LazyColumn(contentPadding = PaddingValues(20.dp), verticalArrangement = Arrangement.spacedBy(16.dp)) {
        item { PageTitle("Cameras", "Live video from your device"); RemoteStatus(state); TextButton(onClick = state::refresh) { Text("Refresh cameras") } }
        if (!state.loading && state.value.items().isEmpty()) item { EmptyMessage("No cameras found", "Connect a camera or pair an IP camera in device settings.") }
        items(state.value.items(), key = { it.number("id") }) { camera -> CameraTile(model, camera) }
    }
}
@Composable fun CameraTile(model: WendyModel, camera: JSONObject) {
    val stream = remember { model.nextID() }
    val session = model.session
    var player by remember { mutableStateOf<CameraPlayer?>(null) }
    var bitmap by remember { mutableStateOf<Bitmap?>(null) }
    var error by remember { mutableStateOf<String?>(null) }
    val event = model.cameraEvents[stream]
    LaunchedEffect(stream) { try { model.backend.call("StartCamera", camera.number("id"), stream, session, camera.text("stable_id")) } catch (e: Exception) { error = e.message } }
    DisposableEffect(stream) {
        model.videoConsumers[stream] = { value -> if (value.number("Kind") == 6L) player?.append(Base64.decode(value.text("Bytes"), Base64.DEFAULT)) }
        onDispose { model.videoConsumers.remove(stream); player?.close(); model.cameraEvents.remove(stream); model.task { model.backend.call("StopCamera", camera.number("id"), stream, session) } }
    }
    LaunchedEffect(event) {
        if (event == null) return@LaunchedEffect
        when (event.number("Kind").toInt()) {
            7 -> error = event.text("Text")
            8 -> bitmap = withContext(Dispatchers.Default) { val bytes = Base64.decode(event.text("Bytes"), Base64.DEFAULT); BitmapFactory.decodeByteArray(bytes, 0, bytes.size) }
            9 -> bitmap = withContext(Dispatchers.Default) {
                val thermal = event.child("Thermal"); val width = thermal.number("Width").toInt(); val height = thermal.number("Height").toInt()
                val bytes = Base64.decode(thermal.text("RGBA"), Base64.DEFAULT)
                if (width in 1..4096 && height in 1..4096 && bytes.size.toLong() == width.toLong() * height * 4) Bitmap.createBitmap(width, height, Bitmap.Config.ARGB_8888).apply { copyPixelsFromBuffer(ByteBuffer.wrap(bytes)) } else null
            }
        }
    }
    SectionCard(camera.text("name")) {
        Box(Modifier.fillMaxWidth().aspectRatio(4f / 3f).clip(RoundedCornerShape(16.dp)).background(Color(0xFF0B1713))) {
            AndroidView(factory = { context -> CameraPlayer(context) { error = it }.also { player = it } }, modifier = Modifier.fillMaxSize())
            bitmap?.let { Image(it.asImageBitmap(), contentDescription = camera.text("name"), modifier = Modifier.fillMaxSize()) }
        }
        if (event == null && error == null) Text("Waiting for video…")
        error?.let { Text(it, color = MaterialTheme.colorScheme.error) }
        if (event?.number("Kind") == 9L) {
            val thermal = event.child("Thermal")
            val summary = thermal.child("Summary")
            if (summary.has("ColdCelsius") && summary.has("HotCelsius")) SummaryLine("Temperature range", "%.1f to %.1f°C".format(summary.optDouble("ColdCelsius"), summary.optDouble("HotCelsius")))
            TechnicalDetails(thermal, "Camera diagnostics")
        }
    }
}
