package dev.wendy.android

import android.Manifest
import android.content.pm.PackageManager
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.camera.core.CameraSelector
import androidx.camera.core.ImageAnalysis
import androidx.camera.core.ImageProxy
import androidx.camera.core.Preview
import androidx.camera.lifecycle.ProcessCameraProvider
import androidx.camera.view.PreviewView
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.Path
import androidx.compose.ui.graphics.PathEffect
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.layout.onSizeChanged
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalLifecycleOwner
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.IntSize
import androidx.compose.ui.unit.dp
import androidx.compose.ui.viewinterop.AndroidView
import androidx.compose.ui.window.Dialog
import androidx.compose.ui.window.DialogProperties
import androidx.core.content.ContextCompat
import com.google.mlkit.vision.common.InputImage
import com.google.mlkit.vision.text.TextRecognition
import com.google.mlkit.vision.text.latin.TextRecognizerOptions
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import java.util.concurrent.Executors
import java.util.concurrent.atomic.AtomicReference

/** Waits for a Google Play services task without the coroutine adapter library. */
private suspend fun <T> com.google.android.gms.tasks.Task<T>.await(): T = kotlinx.coroutines.suspendCancellableCoroutine { continuation ->
    addOnSuccessListener { continuation.resumeWith(Result.success(it)) }
    addOnFailureListener { continuation.resumeWith(Result.failure(it)) }
    addOnCanceledListener { continuation.cancel() }
}

/** One upright grayscale camera frame. */
class RecognitionFrame(val gray: ByteArray, val width: Int, val height: Int, val timestamp: Double)

/** The upright luminance plane of a YUV frame, at most 1920 pixels on its long side. */
private fun ImageProxy.uprightGray(): RecognitionFrame {
    val plane = planes[0]
    val buffer = plane.buffer
    val rowStride = plane.rowStride; val pixelStride = plane.pixelStride
    val w = width; val h = height
    val step = maxOf(1, (maxOf(w, h) + 1919) / 1920)
    val sw = w / step; val sh = h / step
    val source = ByteArray(sw * sh)
    for (y in 0 until sh) for (x in 0 until sw) source[y * sw + x] = buffer.get((y * step) * rowStride + (x * step) * pixelStride)
    val rotation = imageInfo.rotationDegrees
    if (rotation == 0) return RecognitionFrame(source, sw, sh, imageInfo.timestamp / 1e9)
    val (ow, oh) = if (rotation == 90 || rotation == 270) sh to sw else sw to sh
    val out = ByteArray(ow * oh)
    for (y in 0 until sh) for (x in 0 until sw) {
        val (nx, ny) = when (rotation) { 90 -> (sh - 1 - y) to x; 180 -> (sw - 1 - x) to (sh - 1 - y); else -> y to (sw - 1 - x) }
        out[ny * ow + nx] = source[y * sw + x]
    }
    return RecognitionFrame(out, ow, oh, imageInfo.timestamp / 1e9)
}

/** Maps normalized frame coordinates into the preview, which fills its view and crops the overflow. */
private class PreviewMapping(val frameWidth: Int, val frameHeight: Int, val view: IntSize) {
    private val scale = maxOf(view.width.toDouble() / frameWidth, view.height.toDouble() / frameHeight)
    private val dx = (frameWidth * scale - view.width) / 2; private val dy = (frameHeight * scale - view.height) / 2
    fun toView(p: SightPoint) = Offset((p.x * frameWidth * scale - dx).toFloat(), (p.y * frameHeight * scale - dy).toFloat())
    fun toFrame(x: Double, y: Double) = SightPoint((x * view.width + dx) / (frameWidth * scale), (y * view.height + dy) / (frameHeight * scale))
}

/**
 * Recognize a device by pointing the phone at it (iOS DeviceViewfinder): saved appearances, bundled hardware
 * photos and visible device names. A match is a suggestion; choosing it opens the normal connection.
 */
@Composable
fun DeviceViewfinder(model: WendyModel, dismiss: () -> Unit, open: (Device) -> Unit) {
    val context = LocalContext.current
    val lifecycle = LocalLifecycleOwner.current
    val scope = rememberCoroutineScope()
    val engine = remember { DeviceRecognitionEngine(context.applicationContext) }
    var allowed by remember { mutableStateOf(ContextCompat.checkSelfPermission(context, Manifest.permission.CAMERA) == PackageManager.PERMISSION_GRANTED) }
    val permission = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) { allowed = it }
    var error by remember { mutableStateOf<String?>(null) }
    var tracking by remember { mutableStateOf("Point at a device and move closer.") }
    var diagnostics by remember { mutableStateOf("Waiting for the first image…") }
    var appearances by remember { mutableStateOf(emptyList<DeviceSight>()) }
    var labels by remember { mutableStateOf(emptyList<DeviceSight>()) }
    var appearanceTime by remember { mutableLongStateOf(0L) }
    var labelTime by remember { mutableLongStateOf(0L) }
    var now by remember { mutableLongStateOf(System.currentTimeMillis()) }
    var viewSize by remember { mutableStateOf(IntSize.Zero) }
    var frameSize by remember { mutableStateOf(1 to 1) }
    val latest = remember { AtomicReference<RecognitionFrame?>(null) }
    var remembering by remember { mutableStateOf(false) }
    var registering by remember { mutableStateOf<RecognitionFrame?>(null) }
    var selected by remember { mutableStateOf<DeviceSight?>(null) }
    var showDiagnostics by remember { mutableStateOf(false) }
    var library by remember { mutableStateOf<List<DeviceVisualReference>?>(null) }
    LaunchedEffect(Unit) { if (!allowed) permission.launch(Manifest.permission.CAMERA) }
    LaunchedEffect(Unit) { while (true) { now = System.currentTimeMillis(); delay(250) } }
    LaunchedEffect(Unit) { while (true) { try { model.refreshNearby() } catch (e: CancellationException) { throw e } catch (_: Exception) {}; delay(10_000) } }
    val mapping = PreviewMapping(frameSize.first, frameSize.second, viewSize)
    val visible = (if (now - appearanceTime < 1500) appearances else emptyList()) + (if (now - labelTime < 1500) labels else emptyList())
    // Appearance matching every 120 ms and label reading every 500 ms, on their own threads, newest frame only.
    LaunchedEffect(allowed, model.inForeground) {
        if (!allowed || !model.inForeground) return@LaunchedEffect
        try { withContext(Dispatchers.Default) { engine.load() } } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message; return@LaunchedEffect }
        launch(Dispatchers.Default) {
            val confirmation = DeviceSightConfirmation()
            var last = -1.0
            while (true) {
                val frame = latest.get()
                if (frame == null || frame.timestamp <= last) { delay(200); continue }
                last = frame.timestamp
                try {
                    val started = System.nanoTime()
                    val report = engine.inspect(frame.gray, frame.width, frame.height)
                    val ms = (System.nanoTime() - started) / 1e6
                    val age = (latest.get()?.timestamp ?: frame.timestamp) - frame.timestamp
                    withContext(Dispatchers.Main) {
                        diagnostics = report.summary + " · %.0f ms".format(ms)
                        if (age < 1.5) {
                            appearances = confirmation.accept(DeviceSight.group(report.matches), frame.timestamp); appearanceTime = System.currentTimeMillis()
                            tracking = if (appearances.isEmpty()) report.guidance else "Tap a device card to open it."
                        } else { appearances = emptyList(); confirmation.reset(); tracking = "Recognition is running slowly. Keep the board centred and hold steady." }
                        error = null
                    }
                } catch (e: CancellationException) { throw e } catch (e: Exception) { withContext(Dispatchers.Main) { error = e.message } }
                delay(120)
            }
        }
        launch(Dispatchers.Default) {
            val recognizer = TextRecognition.getClient(TextRecognizerOptions.DEFAULT_OPTIONS)
            val confirmation = DeviceSightConfirmation()
            try {
                while (true) {
                    val devices = model.nearbyDevices
                    val frame = latest.get()
                    if (devices.isNotEmpty() && frame != null) {
                        try {
                            val nv21 = ByteArray(frame.width * frame.height * 3 / 2) { if (it < frame.width * frame.height) frame.gray[it] else 128.toByte() }
                            val result = recognizer.process(InputImage.fromByteArray(nv21, frame.width, frame.height, 0, InputImage.IMAGE_FORMAT_NV21)).await()
                            val names = devices.map { it.title }
                            val occurrences = HashMap<String, Int>()
                            val sights = result.textBlocks.flatMap { it.lines }.mapNotNull { line ->
                                if ((line.confidence) < 0.8f) return@mapNotNull null
                                val indices = DeviceLabelMatcher.matches(line.text, names)
                                if (indices.isEmpty()) return@mapNotNull null
                                val corners = line.cornerPoints?.map { SightPoint(it.x.toDouble() / frame.width, it.y.toDouble() / frame.height) } ?: return@mapNotNull null
                                val key = indices.map { devices[it].id }.sorted().joinToString("|")
                                val n = occurrences.getOrDefault(key, 0); occurrences[key] = n + 1
                                DeviceSight("label:$key:$n", line.text, indices.map { devices[it].id }, corners, "Visible device name")
                            }
                            val age = (latest.get()?.timestamp ?: frame.timestamp) - frame.timestamp
                            val accepted = if (age < 1.5) confirmation.accept(sights, frame.timestamp) else { confirmation.reset(); emptyList() }
                            withContext(Dispatchers.Main) { labels = accepted; labelTime = System.currentTimeMillis() }
                        } catch (e: CancellationException) { throw e } catch (_: Exception) {
                            // Label reading failing must not stop appearance matching.
                            confirmation.reset(); withContext(Dispatchers.Main) { labels = emptyList() }
                        }
                    } else { confirmation.reset(); withContext(Dispatchers.Main) { labels = emptyList() } }
                    delay(500)
                }
            } finally { recognizer.close() }
        }
    }
    Dialog(onDismissRequest = dismiss, properties = DialogProperties(usePlatformDefaultWidth = false)) {
        Box(Modifier.fillMaxSize().background(Color.Black).onSizeChanged { viewSize = it }) {
            if (allowed) {
                val analysis = remember { Executors.newSingleThreadExecutor() }
                DisposableEffect(Unit) { onDispose { analysis.shutdown() } }
                AndroidView(factory = { ctx ->
                    PreviewView(ctx).also { view ->
                        view.scaleType = PreviewView.ScaleType.FILL_CENTER
                        val providerFuture = ProcessCameraProvider.getInstance(ctx)
                        providerFuture.addListener({
                            runCatching {
                                val provider = providerFuture.get()
                                val preview = Preview.Builder().build().also { it.surfaceProvider = view.surfaceProvider }
                                val frames = ImageAnalysis.Builder().setBackpressureStrategy(ImageAnalysis.STRATEGY_KEEP_ONLY_LATEST).build()
                                frames.setAnalyzer(analysis) { image -> try { val f = image.uprightGray(); latest.set(f); if (frameSize != f.width to f.height) frameSize = f.width to f.height } finally { image.close() } }
                                provider.unbindAll()
                                provider.bindToLifecycle(lifecycle, CameraSelector.DEFAULT_BACK_CAMERA, preview, frames)
                            }.onFailure { error = "The camera could not start. ${it.message ?: ""}".trim() }
                        }, ContextCompat.getMainExecutor(ctx))
                    }
                }, modifier = Modifier.fillMaxSize(), onRelease = { runCatching { ProcessCameraProvider.getInstance(it.context).get().unbindAll() } })
            }
            if (viewSize.width > 0) Canvas(Modifier.fillMaxSize()) {
                if (remembering) {
                    val a = Offset(size.width * 0.2f, size.height * 0.3f)
                    drawRect(Color.White, a, androidx.compose.ui.geometry.Size(size.width * 0.6f, size.height * 0.4f), style = Stroke(2.dp.toPx(), pathEffect = PathEffect.dashPathEffect(floatArrayOf(8.dp.toPx(), 4.dp.toPx()))))
                }
                visible.forEach { sight ->
                    val points = sight.corners.map(mapping::toView)
                    if (points.isEmpty()) return@forEach
                    val path = Path().apply { moveTo(points[0].x, points[0].y); points.drop(1).forEach { lineTo(it.x, it.y) }; close() }
                    drawPath(path, Color(0xFF9FE2BF), style = Stroke(2.dp.toPx(), pathEffect = PathEffect.dashPathEffect(floatArrayOf(6.dp.toPx(), 3.dp.toPx()))))
                }
            }
            Column(Modifier.fillMaxSize().windowInsetsPadding(WindowInsets.systemBars).padding(16.dp), verticalArrangement = Arrangement.spacedBy(10.dp)) {
                Surface(shape = RoundedCornerShape(20.dp), color = Color.Black.copy(alpha = .55f), contentColor = Color.White) {
                    Row(Modifier.fillMaxWidth().padding(horizontal = 8.dp), verticalAlignment = Alignment.CenterVertically) {
                        TextButton(onClick = dismiss) { Icon(Icons.Outlined.Close, null, tint = Color.White); Spacer(Modifier.width(4.dp)); Text("Close", color = Color.White) }
                        Spacer(Modifier.weight(1f))
                        IconButton(onClick = { showDiagnostics = true }) { Icon(Icons.Outlined.Info, "Recognition diagnostics", tint = Color.White) }
                        IconButton(onClick = { scope.launch { library = withContext(Dispatchers.Default) { runCatching { engine.load(); engine.registered() }.getOrDefault(emptyList()) } } }) { Icon(Icons.Outlined.Layers, "Saved appearances", tint = Color.White) }
                    }
                }
                if (visible.isNotEmpty()) LazyColumn(Modifier.heightIn(max = 240.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                    items(visible.sortedBy { it.id }, key = { it.id }) { sight ->
                        Surface(onClick = { selected = sight }, shape = RoundedCornerShape(16.dp), color = MaterialTheme.colorScheme.surface.copy(alpha = .92f)) {
                            Row(Modifier.fillMaxWidth().heightIn(min = 68.dp).padding(14.dp), horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
                                Icon(Icons.Outlined.Memory, null, tint = Color(0xFF9FE2BF))
                                Column(Modifier.weight(1f)) {
                                    Text(sight.label, fontWeight = FontWeight.Bold, maxLines = 1, overflow = TextOverflow.Ellipsis)
                                    Text(sight.evidence, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1)
                                }
                                Icon(Icons.Outlined.ChevronRight, null)
                            }
                        }
                    }
                }
                Spacer(Modifier.weight(1f))
                Surface(shape = RoundedCornerShape(20.dp), color = Color.Black.copy(alpha = .6f), contentColor = Color.White) {
                    Column(Modifier.fillMaxWidth().padding(16.dp), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(10.dp)) {
                        if (!allowed) Text("Allow camera access in Settings to recognize devices.", color = Color(0xFFFF9500), textAlign = TextAlign.Center)
                        (error)?.let { Text(it, color = Color(0xFFFF9500), textAlign = TextAlign.Center) }
                        Text(if (remembering) "Fill the frame with the device. Include distinctive details and keep the background outside." else tracking, textAlign = TextAlign.Center)
                        if (remembering) Row(Modifier.fillMaxWidth()) {
                            TextButton(onClick = { remembering = false }) { Text("Cancel", color = Color.White) }
                            Spacer(Modifier.weight(1f))
                            Button(onClick = { registering = latest.get() }, enabled = latest.get() != null, colors = ButtonDefaults.buttonColors(containerColor = LensStyle.green)) { Icon(Icons.Outlined.PhotoCamera, null); Spacer(Modifier.width(6.dp)); Text("Capture appearance") }
                        } else {
                            TextButton(onClick = { remembering = true }, enabled = allowed) { Icon(Icons.Outlined.CenterFocusWeak, null, tint = Color.White); Spacer(Modifier.width(6.dp)); Text("Remember a device’s appearance", color = Color.White) }
                            Text("Appearance can be shared by identical devices. Tap a match to choose and connect.", style = MaterialTheme.typography.bodySmall, color = Color.White.copy(alpha = .7f), textAlign = TextAlign.Center)
                        }
                    }
                }
            }
        }
    }
    selected?.let { sight ->
        val candidates = model.nearbyDevices.filter { it.id in sight.deviceIDs }
        ChooserSheet(sight.label, { selected = null }) {
            SecondaryText(sight.evidence)
            Text("Choose the device to open its state and cameras. Matching appearances are not unique identifiers.")
            if (candidates.isNotEmpty()) { Text("MATCHING NEARBY DEVICES", style = MaterialTheme.typography.labelMedium); candidates.forEach { d -> ActionRow(d.title, if (d.lite) Icons.Outlined.Memory else Icons.Outlined.Lan) { selected = null; dismiss(); open(d) } } }
            Text(if (candidates.isEmpty()) "NEARBY DEVICES" else "OTHER NEARBY DEVICES", style = MaterialTheme.typography.labelMedium)
            model.nearbyDevices.filter { it.id !in sight.deviceIDs }.forEach { d -> ActionRow(d.title, if (d.lite) Icons.Outlined.Memory else Icons.Outlined.Lan) { selected = null; dismiss(); open(d) } }
            if (model.nearbyDevices.isEmpty()) SecondaryText("No nearby devices found.")
            model.nearbyError?.let { Text(it, color = Color(0xFFFF9500)) }
        }
    }
    registering?.let { frame ->
        var busy by remember { mutableStateOf(false) }
        ChooserSheet("Remember appearance", { if (!busy) registering = null }) {
            Text("Choose which device you photographed. This saves an appearance on this phone, not a camera calibration. Identical devices can still match.")
            model.nearbyDevices.forEach { device ->
                ActionRow(device.title, enabled = !busy) {
                    busy = true
                    scope.launch {
                        try {
                            if (!model.connectAndWait(device)) throw RecognitionException(model.detailError ?: "Could not connect to the selected device.")
                            // The reticle, mapped from the view back into the captured frame, in pixels.
                            val reticle = listOf(0.2 to 0.3, 0.8 to 0.3, 0.8 to 0.7, 0.2 to 0.7).map { (x, y) -> mapping.toFrame(x, y) }
                            val quad = DoubleArray(8) { i -> val p = reticle[i / 2]; if (i % 2 == 0) (p.x * frame.width).coerceIn(0.0, frame.width - 1.0) else (p.y * frame.height).coerceIn(0.0, frame.height - 1.0) }
                            val reference = DeviceVisualReference(label = device.title, deviceID = device.id, width = frame.width, height = frame.height, gray = frame.gray, quad = quad)
                            withContext(Dispatchers.Default) { engine.remember(reference) }
                            registering = null; remembering = false; error = null
                        } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message; registering = null } finally { busy = false }
                    }
                }
            }
            if (model.nearbyDevices.isEmpty()) SecondaryText("No nearby devices found. Connect the device to the same network first.")
            if (busy) LoadingRow("Connecting…")
        }
    }
    if (showDiagnostics) {
        var saving by remember { mutableStateOf(false) }
        var saved by remember { mutableStateOf(false) }
        ChooserSheet("Recognition diagnostics", { showDiagnostics = false }) {
            Text(tracking); Text(diagnostics, style = MaterialTheme.typography.bodySmall)
            Text("Save one camera frame on this phone so we can diagnose this miss. It is not uploaded. A new capture replaces the previous diagnostic frame.")
            ActionRow(if (saving) "Saving…" else "Save diagnostic frame", Icons.Outlined.PhotoCamera, enabled = latest.get() != null && !saving) {
                val frame = latest.get() ?: return@ActionRow
                saving = true
                scope.launch {
                    try { withContext(Dispatchers.Default) { val report = engine.inspect(frame.gray, frame.width, frame.height); engine.saveDiagnostic(frame.gray, frame.width, frame.height, report.summary) }; saved = true }
                    catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { saving = false }
                }
            }
            if (saved) Row(horizontalArrangement = Arrangement.spacedBy(6.dp)) { Icon(Icons.Outlined.CheckCircle, null, tint = LensStyle.accent); Text("Diagnostic frame saved on this phone") }
        }
    }
    library?.let { saved ->
        ChooserSheet("Saved appearances", { library = null }) {
            if (saved.isEmpty()) SecondaryText("No saved device appearances.")
            saved.forEach { reference ->
                Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
                    Text(reference.label, Modifier.weight(1f))
                    IconButton(onClick = { scope.launch { try { withContext(Dispatchers.Default) { engine.forget(reference.id) }; library = engine.registered() } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } } }) {
                        Icon(Icons.Outlined.Delete, "Remove", tint = MaterialTheme.colorScheme.error)
                    }
                }
            }
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun ChooserSheet(title: String, dismiss: () -> Unit, content: @Composable ColumnScope.() -> Unit) {
    ModalBottomSheet(onDismissRequest = dismiss) {
        Column(Modifier.fillMaxWidth().padding(horizontal = 20.dp).padding(bottom = 24.dp), verticalArrangement = Arrangement.spacedBy(10.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) { Text(title, Modifier.weight(1f), style = MaterialTheme.typography.titleLarge, fontWeight = FontWeight.Bold); TextButton(onClick = dismiss) { Text("Done") } }
            content()
        }
    }
}
