package dev.wendy.android

import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.os.Handler
import android.os.HandlerThread
import android.util.Base64
import android.view.WindowManager
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.Canvas
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.hapticfeedback.HapticFeedbackType
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalHapticFeedback
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.NonCancellable
import kotlinx.coroutines.currentCoroutineContext
import kotlinx.coroutines.delay
import kotlinx.coroutines.ensureActive
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import org.json.JSONObject
import kotlin.math.floor
import kotlin.math.min
import kotlin.math.pow

/*
 * Camera alignment for Sites (iOS MobileSiteCalibration, SiteCameraAlignment, SiteAutomaticAlignment and
 * PhoneLensCalibrationView). A camera is matched to the scan's reference map and its lens and position are saved on
 * its gateway; the person can instead adjust it manually, or measure the lens with this phone's screen.
 */

/** One model camera on the connected gateway (iOS SiteAlignmentRequest). */
class SiteAlignmentRequest(val site: StoredSite, val camera: SiteCamera, val gateway: SiteGatewayIdentity, val session: Long) {
    fun isCurrent(model: WendyModel) = session != 0L && model.session == session
}

private fun connectionChanged() = SiteException("The camera connection changed. Reopen the camera and try again.")

private suspend fun freshFrame(frames: CameraCalibrationFrames, after: CameraCalibrationFrame?, check: () -> Unit): CameraCalibrationFrame {
    val deadline = System.currentTimeMillis() + 8000
    while (System.currentTimeMillis() < deadline) {
        check()
        val frame = frames.latest
        if (frame != null && frame.isFresh(System.currentTimeMillis()) && (after == null || (frame.sequence != after.sequence && frame.received > after.received))) return frame
        delay(150)
    }
    throw SiteException("Waiting for fresh camera video.")
}

/** Manual and automatic alignment use exactly the same solve and save checks (iOS SiteCameraAlignment.align). */
suspend fun WendyModel.alignCamera(request: SiteAlignmentRequest, frames: CameraCalibrationFrames, engine: SiteLocalizationEngine, automatic: Boolean = false, persist: Boolean = true,
                                   lensOverride: SiteCameraLens? = null, expectedRevision: Long? = null, isCurrent: () -> Boolean = { true }): SiteLocalizationResult {
    val source = request.camera.sourceID
    val context = currentCoroutineContext()
    fun check() {
        context.ensureActive()
        if (!isCurrent() || !request.isCurrent(this) || cameras.count { it.text("stable_id") == source } != 1) throw connectionChanged()
    }
    check()
    val records = cameraMetadata(request.session).filter { it.sourceID == source }
    check()
    if (records.size > 1) throw SiteLocalizationException(SiteLocalizationException.Kind.InvalidResult)
    val record = records.firstOrNull()
    if (expectedRevision != null && expectedRevision != (record?.revision ?: 0)) throw SiteException("Camera calibration changed. Reopen calibration and try again.")
    // An earlier estimate is not a lens measurement: reusing it could lock every retry to the same wrong focal
    // length and distance. The second frame still verifies the selected lens below.
    val measured = (lensOverride ?: record?.lens)?.takeIf { it.measured }
    if (automatic && (request.camera.position != null || record?.pose(request.site.site, System.currentTimeMillis())?.method == 1)) throw SiteException("Manual placement retained.")
    val revision = request.site.site.modelRevision
    val first = freshFrame(frames, null, ::check)
    var solution = try { withContext(Dispatchers.Default) { engine.localize(first.gray, first.width, first.height, revision, measured) } }
    catch (e: SiteLocalizationException) {
        if (e.kind != SiteLocalizationException.Kind.NoSolution) throw e
        check()
        withContext(Dispatchers.Default) { engine.localize(first.gray, first.width, first.height, revision, measured, estimated = true) }
    }
    if (solution.fittedLens) solution = solution.copy(lens = solution.lens.copy(captureMode = "${first.codec} ${first.width}x${first.height}"))
    check()
    delay(800)
    val second = freshFrame(frames, first, ::check)
    if (first.width != second.width || first.height != second.height || first.codec != second.codec) throw SiteLocalizationException(SiteLocalizationException.Kind.InvalidImage)
    val verification = withContext(Dispatchers.Default) { engine.localize(second.gray, second.width, second.height, revision, solution.lens, estimated = !solution.lens.measured) }
    solution = SiteAlignmentValidation.verified(solution, verification, first, second)
    check()
    if (persist) saveCameraAlignment(request.session, source, record?.revision ?: 0, solution)
    check()
    return solution
}

/** Plays one camera without a view, only to feed calibration frames (iOS playOwnedCamera for alignment). */
class CalibrationStream(private val model: WendyModel, private val camera: JSONObject, val frames: CameraCalibrationFrames = CameraCalibrationFrames()) {
    private val thread = HandlerThread("WendyCalibrationStream").apply { start() }
    private val worker = Handler(thread.looper)
    private val parser = AnnexBParser()
    private val units = H264AccessUnits()
    private var sps: ByteArray? = null
    private var pps: ByteArray? = null

    suspend fun <T> use(block: suspend () -> T): T {
        val session = model.session; val id = camera.number("id"); val stream = model.nextID()
        frames.acquire()
        model.videoConsumers[stream] = { event ->
            when (event.number("Kind").toInt()) {
                6 -> { val bytes = Base64.decode(event.text("Bytes"), Base64.DEFAULT); worker.post { runCatching { feed(bytes) } } }
                8 -> { val bytes = Base64.decode(event.text("Bytes"), Base64.DEFAULT); worker.post { BitmapFactory.decodeByteArray(bytes, 0, bytes.size)?.let(frames::acceptJpeg) } }
            }
        }
        try {
            model.backend.call("StartCamera", id, stream, session, camera.text("stable_id"))
            return block()
        } finally {
            model.videoConsumers.remove(stream); model.cameraEvents.remove(stream)
            withContext(NonCancellable) { runCatching { model.backend.call("StopCamera", id, stream, session) } }
            frames.release(); thread.quitSafely()
        }
    }

    private fun feed(bytes: ByteArray) {
        for (nal in parser.append(bytes)) {
            if (nal.isEmpty()) continue
            units.append(nal)?.let(::frame)
            when (nal[0].toInt() and 31) { 7 -> sps = nal; 8 -> pps = nal }
        }
    }

    private fun frame(unit: ByteArray) {
        val s = sps ?: return; val p = pps ?: return
        if (unit[4].toInt() and 31 != 5) return
        val dimensions = h264Dimensions(s)
        frames.acceptH264(s, p, unit, dimensions.width, dimensions.height)
    }
}

/** The connected gateway's identity and its camera records, refreshed every five seconds (iOS MobileSiteCoverageScene). */
class SiteCameraRecords(val gateway: SiteGatewayIdentity = SiteGatewayIdentity(), val records: List<SiteCameraMetadata> = emptyList(), val session: Long = 0, val checked: Boolean = false, val error: String? = null) {
    fun geometry(model: WendyModel, site: StoredSite): Map<String, SiteCameraGeometry> {
        if (session == 0L || session != model.session) return emptyMap()
        val now = System.currentTimeMillis()
        return site.site.cameras.mapNotNull { camera -> SiteCoverage.geometry(camera, site.site, records, gateway, now)?.let { camera.id to it } }.toMap()
    }
}

/**
 * Reads the camera records of the connected gateway while [active]. [assets] limits it to a site's devices;
 * [known] skips the identity lookup when the caller already resolved it.
 */
@Composable
fun rememberSiteCameraRecords(model: WendyModel, active: Boolean, assets: List<SiteAsset>? = null, known: SiteGatewayIdentity? = null, refresh: Int = 0): SiteCameraRecords {
    var state by remember { mutableStateOf(SiteCameraRecords()) }
    val visible = LocalPageVisible.current
    LaunchedEffect(model.session, active && visible, known, refresh) {
        state = SiteCameraRecords()
        val session = model.session
        val device = model.selected
        if (!active || !visible) return@LaunchedEffect
        if (session == 0L || device == null) { state = SiteCameraRecords(checked = true); return@LaunchedEffect }
        if (assets != null && assets.none { !it.robot && it.id == recordingDeviceID(device) }) { state = SiteCameraRecords(checked = true); return@LaunchedEffect }
        val gateway = try {
            known ?: run {
                val info = model.backend.call("DeviceInfo", session).obj(); val enrollment = model.backend.call("DeviceEnrollment", session).obj()
                SiteGatewayIdentity.resolve(SiteGatewayIdentity.cloud(device.selector), enrollment.flag("Enrolled"), enrollment.text("OrganizationID"), enrollment.text("DeviceID"), info.text("serial_number", "serial"))
            }
        } catch (e: CancellationException) { throw e } catch (_: Exception) { state = SiteCameraRecords(checked = true, error = "Could not check the camera’s device identity."); return@LaunchedEffect }
        if (session != model.session) return@LaunchedEffect
        state = SiteCameraRecords(gateway, session = session)
        while (isActive && session == model.session) {
            state = try { SiteCameraRecords(gateway, model.cameraMetadata(session), session, true) }
            catch (e: CancellationException) { throw e } catch (_: Exception) { SiteCameraRecords(gateway, emptyList(), session, true, "Camera alignment unavailable. Reconnect to the camera and try again.") }
            delay(5000)
        }
    }
    return state
}

/** Retries failed cameras with back-off and re-checks aligned ones each minute (iOS SiteAlignmentSchedule). */
private class SiteAlignmentSchedule {
    private val entries = HashMap<String, Pair<Int, Long>>()
    fun due(source: String, now: Long) = now >= (entries[source]?.second ?: 0L)
    fun completed(source: String, success: Boolean, now: Long) {
        val failures = if (success) 0 else min((entries[source]?.first ?: 0) + 1, 5)
        val delay = if (success) 60.0 else min(300.0, 15 * 2.0.pow(failures))
        entries[source] = failures to now + (delay * 1000).toLong()
    }
}

/** Aligns the site's unplaced cameras of the connected gateway in the background while [enabled] (iOS SiteAutomaticAlignment). */
@Composable
fun rememberAutomaticAlignment(model: WendyModel, site: StoredSite?, records: SiteCameraRecords, enabled: Boolean): Map<String, String> {
    var statuses by remember { mutableStateOf(mapOf<String, String>()) }
    val session = records.session
    LaunchedEffect(site?.id, site?.site?.cameras, site?.visualMap, records.gateway, session, enabled) {
        statuses = emptyMap()
        if (!enabled || site == null || session == 0L || records.gateway.id.isEmpty() || session != model.session) return@LaunchedEffect
        val cameras = site.site.cameras.filter { records.gateway.matches(it.gatewayID) && it.position == null }
        if (cameras.isEmpty()) return@LaunchedEffect
        if (site.visualMap == null) { statuses = cameras.associate { it.sourceID to "Automatic alignment needs a room reference scan." }; return@LaunchedEffect }
        val engine = SiteLocalizationEngine()
        try { withContext(Dispatchers.IO) { engine.load(site) } }
        catch (e: CancellationException) { throw e } catch (e: Exception) { statuses = cameras.associate { it.sourceID to (e.message ?: "The room’s visual reference map is invalid.") }; return@LaunchedEffect }
        val schedule = SiteAlignmentSchedule()
        while (isActive && model.session == session) {
            for (camera in cameras) {
                if (!isActive || model.session != session) return@LaunchedEffect
                if (!schedule.due(camera.sourceID, System.currentTimeMillis())) continue
                val feed = model.cameras.filter { it.text("stable_id") == camera.sourceID }.singleOrNull()
                if (feed == null || feed.has("thermal")) {
                    statuses = statuses + (camera.sourceID to "Waiting for an available visual camera.")
                    schedule.completed(camera.sourceID, false, System.currentTimeMillis()); continue
                }
                statuses = statuses + (camera.sourceID to "Checking camera position…")
                try {
                    val stream = CalibrationStream(model, feed)
                    val result = stream.use { model.alignCamera(SiteAlignmentRequest(site, camera, records.gateway, session), stream.frames, engine, automatic = true) }
                    statuses = statuses + (camera.sourceID to if (result.lens.measured) "Position checked automatically" else "Approximate position checked automatically")
                    schedule.completed(camera.sourceID, true, System.currentTimeMillis())
                } catch (e: CancellationException) { throw e } catch (e: Exception) {
                    if (model.session != session) return@LaunchedEffect
                    schedule.completed(camera.sourceID, false, System.currentTimeMillis())
                    statuses = statuses + (camera.sourceID to "${e.message ?: "Alignment failed."} Will retry automatically.")
                }
            }
            delay(2000)
        }
    }
    return statuses
}

/** The coverage note under a site's 3D view (iOS MobileSiteCoverageScene.explanation). */
fun coverageExplanation(site: StoredSite, records: SiteCameraRecords, geometry: Map<String, SiteCameraGeometry>): String = when {
    records.error != null -> records.error
    site.site.cameras.isEmpty() -> "Add cameras to show their viewing areas."
    !records.checked -> "Checking camera alignment…"
    geometry.isEmpty() -> "Open a camera to connect. Wendy will try to align it using the scan references."
    else -> {
        val manual = geometry.values.count { it.pose.provenance == "Manual visualization" }
        val estimated = geometry.values.count { it.lens?.measured != true }
        if (manual > 0) "${geometry.size} viewing areas · $manual manually aligned. Visual estimates only."
        else if (estimated > 0) "${geometry.size} viewing areas · $estimated estimated. Cones show direction, not sensor range."
        else "${geometry.size} calibrated viewing areas. Cones show direction, not sensor range."
    }
}

/** The 6×9 inner-corner checkerboard with a white border, 900 × 1200 pixels (iOS RobotCalibrationPattern). */
fun calibrationPattern(): Bitmap = Bitmap.createBitmap(900, 1200, Bitmap.Config.ARGB_8888).apply {
    eraseColor(android.graphics.Color.WHITE)
    val canvas = android.graphics.Canvas(this); val paint = android.graphics.Paint().apply { color = android.graphics.Color.BLACK }
    for (row in 0 until 10) for (column in 0 until 7) if ((row + column) % 2 == 0)
        canvas.drawRect((column + 1) * 100f, (row + 1) * 100f, (column + 2) * 100f, (row + 2) * 100f, paint)
}

/** Draws the calibration checkerboard filling the space with exact square cells. */
@Composable
fun CalibrationCheckerboard(modifier: Modifier = Modifier) {
    Canvas(modifier.background(Color.White)) {
        // A white border plus 7×10 square cells gives 6×9 inner corners; the screen's size does not matter.
        val side = floor(min(size.width / 9, size.height / 12))
        val origin = Offset((size.width - 9 * side) / 2 + side, (size.height - 12 * side) / 2 + side)
        for (row in 0 until 10) for (column in 0 until 7) if ((row + column) % 2 == 0)
            drawRect(Color.Black, Offset(origin.x + column * side, origin.y + row * side), Size(side, side))
    }
}

/**
 * Align one camera (iOS MobileSiteCalibration): match its live video to the scan and save the lens and position
 * on the gateway, adjust it manually, or improve accuracy by measuring the lens with a checkerboard.
 */
@Composable
fun SiteCameraAlignContent(model: WendyModel, models: SiteModelStore, request: SiteAlignmentRequest, feed: JSONObject, saveManual: (Site) -> Unit,
                           onSaved: suspend (SiteLocalizationResult) -> Unit = {}, padding: androidx.compose.ui.unit.Dp = 16.dp) {
    val nav = LocalNavigator.current
    val scope = rememberCoroutineScope()
    val context = LocalContext.current
    val frames = remember { CameraCalibrationFrames().apply { acquire() } }
    DisposableEffect(frames) { onDispose { frames.release() } }
    val engine = remember { SiteLocalizationEngine() }
    val lensEngine = remember { ScreenLensCalibrator() }
    var progress by remember { mutableStateOf(ScreenLensProgress()) }
    var lens by remember { mutableStateOf<SiteCameraLens?>(null) }
    var result by remember { mutableStateOf<SiteLocalizationResult?>(null) }
    var revision by remember { mutableStateOf<Long?>(null) }
    var stream by remember { mutableLongStateOf(0L) }
    var alignmentStream by remember { mutableLongStateOf(0L) }
    var busy by remember { mutableStateOf(false) }
    var calibrating by remember { mutableStateOf(false) }
    var calibrationMessage by remember { mutableStateOf<String?>(null) }
    var saved by remember { mutableStateOf(false) }
    var attemptedAutomatic by remember { mutableStateOf(false) }
    var previewReady by remember { mutableStateOf(false) }
    var status by remember { mutableStateOf("Checking saved alignment…") }
    var detail by remember { mutableStateOf("") }
    var failed by remember { mutableStateOf(false) }
    var improve by remember { mutableStateOf(false) }
    val hasMap = request.site.visualMap != null
    val current = request.isCurrent(model)
    LaunchedEffect(frames) { while (isActive && !previewReady) { if (frames.latest != null) previewReady = true; delay(250) } }

    fun perform(calibration: Boolean = false, failureTitle: String = "Could not align camera", work: suspend () -> Unit) {
        busy = true; calibrating = calibration
        if (calibration) calibrationMessage = null
        scope.launch {
            try { if (!request.isCurrent(model)) throw SiteException("Camera connection changed."); work() }
            catch (e: CancellationException) { throw e }
            catch (e: Exception) { if (calibration) calibrationMessage = e.message else { status = failureTitle; detail = e.message ?: ""; failed = true } }
            finally { busy = false }
        }
    }
    suspend fun reloadRevision() {
        val records = model.cameraMetadata(request.session).filter { it.sourceID == request.camera.sourceID }
        if (!request.isCurrent(model) || records.size != 1) throw SiteLocalizationException(SiteLocalizationException.Kind.InvalidResult)
        revision = records[0].revision
    }
    suspend fun save() {
        var localWarning: String? = null
        val expected = revision ?: throw SiteLocalizationException(SiteLocalizationException.Kind.InvalidImage)
        if (!request.isCurrent(model)) throw SiteLocalizationException(SiteLocalizationException.Kind.InvalidImage)
        val solved = result
        if (solved != null) {
            val frame = frames.latest
            if (stream == 0L || stream != alignmentStream || frame == null || !frame.isFresh(System.currentTimeMillis()) || frame.width != solved.lens.width || frame.height != solved.lens.height)
                throw SiteException("Camera stream changed. Align to the model again before saving.")
            model.saveCameraAlignment(request.session, request.camera.sourceID, expected, solved)
            localWarning = try { onSaved(solved); null } catch (e: CancellationException) { throw e } catch (e: Exception) {
                "Alignment saved on the device, but the local placement could not be updated: ${e.message}"
            }
        } else {
            val measured = lens
            if (measured == null || hasMap) return
            model.saveCameraLens(request.session, request.camera.sourceID, expected, measured)
        }
        saved = true
        if (solved == null) calibrationMessage = "Lens calibration saved on the device. Model alignment is still needed."
        else {
            failed = false
            status = if (solved.lens.measured) "Aligned and saved" else "Aligned and saved · estimated"
            detail = localWarning ?: "Go back to see the camera’s viewing area in the 3D model."
        }
        reloadRevision()
    }
    suspend fun alignAndSave() {
        val expected = revision ?: return
        result = null; saved = false; attemptedAutomatic = true
        status = "Aligning camera…"; detail = "Matching live video to the scan. Keep the camera still."; failed = false
        withContext(Dispatchers.IO) { engine.load(request.site) }
        // Retry fresh frames without weakening the geometric or two-frame validation.
        for (attempt in 1..3) {
            detail = "Matching live video to the scan · attempt $attempt of 3"
            try { result = model.alignCamera(request, frames, engine, persist = false, lensOverride = lens, expectedRevision = expected); break }
            catch (e: SiteLocalizationException) { if (e.kind != SiteLocalizationException.Kind.NoSolution || attempt == 3) throw e; delay(1000) }
        }
        alignmentStream = stream
        save()
    }
    suspend fun capture() {
        val frame = frames.latest?.takeIf { it.isFresh(System.currentTimeMillis()) } ?: throw SiteException("Waiting for fresh camera video.")
        val previous = progress.revision
        val count = withContext(Dispatchers.Default) { lensEngine.add(frame.gray, frame.width, frame.height) }
        if (count < 0) throw SiteException(if (count == -2) "The stream resolution changed. Restart lens calibration." else "The calibration image could not be processed.")
        progress = withContext(Dispatchers.Default) { lensEngine.progress() }
        if (progress.count >= 12 && progress.coverageReady) {
            lens = ScreenLensMeasurement.lens(withContext(Dispatchers.Default) { lensEngine.solve() }, frame.width, frame.height, frame.codec)
            result = null; saved = false
            calibrationMessage = "Lens measured. Retry alignment to apply the improved lens measurement."
        } else calibrationMessage = if (previous == progress.revision) "No calibration view accepted. Show the whole checkerboard, change its angle or position, and try again."
            else "Calibration view accepted · ${progress.count} views collected."
    }
    LaunchedEffect(request.session) {
        try {
            val now = System.currentTimeMillis()
            val records = model.cameraMetadata(request.session).filter { it.sourceID == request.camera.sourceID }
            if (!request.isCurrent(model) || records.size > 1) throw SiteLocalizationException(SiteLocalizationException.Kind.InvalidResult)
            revision = records.firstOrNull()?.revision ?: 0
            lens = records.firstOrNull()?.lens?.takeIf { it.measured }
            val geometry = SiteCoverage.geometry(request.camera, request.site.site, records, request.gateway, now)
            when {
                geometry != null -> {
                    status = if (request.camera.manualAlignment != null) "Manually aligned" else if (geometry.lens?.measured == true) "Saved alignment · calibrated lens" else "Saved alignment · estimated"
                    detail = "Last aligned " + java.text.DateFormat.getDateTimeInstance(java.text.DateFormat.MEDIUM, java.text.DateFormat.SHORT).format(java.util.Date(geometry.pose.observedAt))
                }
                request.camera.position != null -> { status = "Saved placement · needs alignment"; detail = "Wendy has kept the saved position. Tap Align automatically to verify or update it." }
                else -> {
                    status = if (hasMap) "Waiting for live video" else "Needs scan references"
                    detail = if (hasMap) "Alignment will start when the camera is ready." else "This model cannot be aligned automatically without scan reference photos."
                }
            }
        } catch (e: CancellationException) { throw e } catch (e: Exception) { status = "Could not check alignment"; failed = true; detail = e.message ?: "" }
    }
    LaunchedEffect(previewReady, revision != null, current) {
        if (!previewReady || revision == null || !current || attemptedAutomatic || !hasMap || request.camera.position != null) return@LaunchedEffect
        attemptedAutomatic = true
        perform { alignAndSave() }
    }
    val exporter = rememberLauncherForActivityResult(ActivityResultContracts.CreateDocument("image/png")) { uri ->
        if (uri == null) return@rememberLauncherForActivityResult
        scope.launch {
            try { withContext(Dispatchers.IO) { context.contentResolver.openOutputStream(uri)?.use { calibrationPattern().compress(Bitmap.CompressFormat.PNG, 100, it) } ?: throw SiteException("The file could not be opened.") } }
            catch (e: CancellationException) { throw e } catch (e: Exception) { calibrationMessage = "Could not save checkerboard: ${e.message}" }
        }
    }
    Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(padding), verticalArrangement = Arrangement.spacedBy(14.dp)) {
        if (!current) { Text("Connection changed. Go back and reconnect to the camera."); return@Column }
        CameraTile(model, feed, calibration = frames, onStream = { stream = it })
        Surface(shape = RoundedCornerShape(14.dp), color = MaterialTheme.colorScheme.surface) {
            Column(Modifier.fillMaxWidth().padding(14.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
                Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                    if (busy && !calibrating) CircularProgressIndicator(Modifier.size(18.dp), strokeWidth = 2.dp)
                    else Icon(if (failed) Icons.Outlined.WarningAmber else if (saved) Icons.Outlined.CheckCircle else Icons.Outlined.CenterFocusWeak, null, tint = if (failed) LensStyle.warning else LensStyle.accent)
                    Text(status, style = MaterialTheme.typography.titleSmall, fontWeight = FontWeight.SemiBold)
                }
                if (detail.isNotEmpty()) SecondaryText(detail)
            }
        }
        if (!hasMap) SecondaryText("This model has no scan reference photos. You can save lens calibration, but alignment needs a scan with visual references.")
        else LensButton(if (attemptedAutomatic) "Retry alignment" else "Align automatically", primary = true, enabled = !busy && revision != null, modifier = Modifier.fillMaxWidth()) { perform { alignAndSave() } }
        LensButton("Adjust manually", enabled = !busy, modifier = Modifier.fillMaxWidth()) {
            nav.push("Manual alignment") {
                SiteManualAlignmentPage(model, models, request.camera, request.site, request.session) { value ->
                    saveManual(value)
                    scope.launch { result = null; saved = true; failed = false; status = "Manually aligned"; detail = "Saved with the 3D model." }
                }
            }
        }
        result?.let { r ->
            Text((if (r.lens.measured) "Aligned · %.2f px" else "Estimated alignment · %.2f px").format(r.residual), color = LensStyle.accent)
            if (!saved) LensButton("Save alignment", primary = true, enabled = !busy, modifier = Modifier.fillMaxWidth()) { perform(failureTitle = "Could not save alignment") { save() } }
        }
        Surface(shape = RoundedCornerShape(14.dp), color = MaterialTheme.colorScheme.surface) {
            Column(Modifier.fillMaxWidth().padding(14.dp), verticalArrangement = Arrangement.spacedBy(10.dp)) {
                Row(Modifier.fillMaxWidth().clickable { improve = !improve }, verticalAlignment = Alignment.CenterVertically) {
                    Text("Improve accuracy (optional)", Modifier.weight(1f), style = MaterialTheme.typography.titleSmall)
                    Icon(if (improve) Icons.Outlined.ExpandLess else Icons.Outlined.ExpandMore, null)
                }
                if (improve) {
                    Text(if (lens?.measured == true) "Lens calibrated" else "Checkerboard · ${progress.count} views", fontWeight = FontWeight.SemiBold)
                    SecondaryText("Keep the camera still. Capture at least 12 distinct views with the checkerboard tilted and near each edge.")
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        TextButton(onClick = { exporter.launch("Wendy-checkerboard.png") }) { Text("Save checkerboard") }
                        Spacer(Modifier.weight(1f))
                        TextButton(onClick = {
                            nav.push("Lens calibration") {
                                PhoneLensCalibrationPage(model, feed) { measured -> lens = measured; result = null; saved = false; calibrationMessage = "Lens measured. Retry alignment to apply the improved lens measurement." }
                            }
                        }, enabled = !busy) { Text("Use this phone") }
                    }
                    LensButton("Capture view", primary = true, enabled = !busy, modifier = Modifier.fillMaxWidth()) { perform(calibration = true) { capture() } }
                    if (!hasMap && lens?.measured == true && !saved) LensButton("Save lens calibration", enabled = !busy, modifier = Modifier.fillMaxWidth()) { perform(calibration = true) { save() } }
                    if (busy && calibrating) LoadingRow("Checking calibration view…")
                    calibrationMessage?.let { SecondaryText(it) }
                }
            }
        }
    }
}

/**
 * Measure the camera's lens with this phone's screen (iOS PhoneLensCalibrationView): the phone shows a checkerboard
 * and the person moves and tilts it in front of the fixed camera until enough distinct views pass the lens check.
 */
@Composable
fun PhoneLensCalibrationPage(model: WendyModel, feed: JSONObject, completion: (SiteCameraLens) -> Unit) {
    val nav = LocalNavigator.current
    val activity = androidx.activity.compose.LocalActivity.current
    val haptics = LocalHapticFeedback.current
    val frames = remember { CameraCalibrationFrames().apply { acquire() } }
    val engine = remember { ScreenLensCalibrator() }
    var progress by remember { mutableStateOf(ScreenLensProgress()) }
    var failure by remember { mutableStateOf<String?>(null) }
    var validating by remember { mutableStateOf(false) }
    var needsMoreAngles by remember { mutableStateOf(false) }
    var lastValidationFailure by remember { mutableStateOf<String?>(null) }
    var help by remember { mutableStateOf(false) }
    var finished by remember { mutableStateOf(false) }
    val session = model.session
    DisposableEffect(activity) {
        // A bright, awake screen keeps the checkerboard visible to the camera.
        val window = activity?.window; val prior = window?.attributes?.screenBrightness
        window?.addFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)
        window?.attributes = window?.attributes?.apply { screenBrightness = maxOf(screenBrightness, 0.85f) }
        onDispose {
            window?.clearFlags(WindowManager.LayoutParams.FLAG_KEEP_SCREEN_ON)
            if (prior != null) window.attributes = window.attributes.apply { screenBrightness = prior }
            frames.release()
        }
    }
    LaunchedEffect(Unit) {
        var lastSolved = 0L; var lastSequence: Long? = null
        while (isActive && !finished) {
            if (model.session != session) { failure = "The camera connection ended. Go back and try again."; return@LaunchedEffect }
            if (help) { delay(350); continue }
            val frame = frames.latest
            if (frame != null && frame.isFresh(System.currentTimeMillis()) && frame.sequence != lastSequence) {
                lastSequence = frame.sequence
                val count = withContext(Dispatchers.Default) { engine.add(frame.gray, frame.width, frame.height) }
                if (count < 0) { failure = if (count == -2) "The stream resolution changed. Go back and restart lens calibration." else "The calibration image could not be processed."; return@LaunchedEffect }
                val next = withContext(Dispatchers.Default) { engine.progress() }
                val captured = next.revision != progress.revision
                progress = next
                if (!next.coverageReady) needsMoreAngles = false
                if (captured) {
                    haptics.performHapticFeedback(HapticFeedbackType.TextHandleMove)
                    if (next.shouldSolve(lastSolved)) {
                        lastSolved = next.revision; validating = true
                        try {
                            val lens = ScreenLensMeasurement.lens(withContext(Dispatchers.Default) { engine.solve() }, frame.width, frame.height, frame.codec)
                            finished = true
                            haptics.performHapticFeedback(HapticFeedbackType.LongPress)
                            completion(lens); nav.pop(); return@LaunchedEffect
                        } catch (e: CancellationException) { throw e } catch (e: Exception) { needsMoreAngles = true; lastValidationFailure = e.message }
                        validating = false
                    }
                }
            }
            delay(350)
        }
    }
    Column(Modifier.fillMaxSize().background(Color.White).padding(vertical = 12.dp), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(10.dp)) {
        Row(Modifier.fillMaxWidth().padding(horizontal = 16.dp), verticalAlignment = Alignment.CenterVertically) {
            Text(if (validating) "Checking lens…" else "${progress.count} views in use", Modifier.weight(1f), color = Color.Black, style = MaterialTheme.typography.titleMedium)
            IconButton(onClick = { help = true }) { Icon(Icons.Outlined.Info, "Help", tint = Color.Black) }
        }
        Row(Modifier.padding(horizontal = 16.dp), horizontalArrangement = Arrangement.spacedBy(16.dp), verticalAlignment = Alignment.CenterVertically) {
            Box(Modifier.width(132.dp)) { CameraTile(model, feed, compact = true, calibration = frames) }
            Column(verticalArrangement = Arrangement.spacedBy(4.dp)) {
                Text("Camera view: recorded positions", color = Color.Black, style = MaterialTheme.typography.labelMedium)
                ScreenLensSide.entries.forEach { side ->
                    val recorded = progress.hasSample(side)
                    Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                        Icon(if (recorded) Icons.Outlined.CheckCircle else Icons.Outlined.RadioButtonUnchecked, null, Modifier.size(16.dp), tint = if (recorded) LensStyle.online else Color.Gray)
                        Text(side.title, color = Color.Black, style = MaterialTheme.typography.bodySmall)
                    }
                }
            }
        }
        Text(failure ?: if (validating) "Hold still while the lens measurement is checked." else progress.instruction, Modifier.padding(horizontal = 16.dp),
            color = Color.Black, textAlign = TextAlign.Center, style = MaterialTheme.typography.bodyLarge, fontWeight = FontWeight.Medium)
        CalibrationCheckerboard(Modifier.fillMaxWidth().weight(1f))
        Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
            listOf("12+ views" to (progress.count >= 12), "Coverage" to progress.coverageReady, "Lens checked" to finished).forEach { (title, done) ->
                Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(4.dp)) {
                    Icon(if (done) Icons.Outlined.CheckCircle else Icons.Outlined.RadioButtonUnchecked, null, Modifier.size(14.dp), tint = if (done) LensStyle.online else Color.Gray)
                    Text(title, color = if (done) LensStyle.green else Color.Gray, style = MaterialTheme.typography.labelSmall)
                }
            }
        }
        Text(if (needsMoreAngles) "Positions captured. More views at different angles are still needed." else "Keep the camera still. Pause at each position until the phone vibrates.",
            Modifier.padding(horizontal = 16.dp), color = Color.DarkGray, textAlign = TextAlign.Center, style = MaterialTheme.typography.bodySmall)
    }
    if (help) AlertDialog(onDismissRequest = { help = false }, confirmButton = { TextButton(onClick = { help = false }) { Text("Done") } }, title = { Text("How to move the phone") }, text = {
        Column(verticalArrangement = Arrangement.spacedBy(10.dp)) {
            Text("Keep the camera fixed and face this screen toward it. Move the phone as shown, then pause until it vibrates.")
            Text("Bring the left edge closer to the camera, then the right edge. Then the top edge, then the bottom edge. Also move the phone closer and farther away.")
            Text("The checks track the number of views, image coverage and the lens measurement. Recorded positions alone do not mean calibration is complete; angles still need to pass the lens check.")
            Text("Capture pauses while this help is open. After 30 views, new captures replace older ones; you do not need to start over.")
            lastValidationFailure?.let { Text("Last lens check: $it") }
        }
    })
}
