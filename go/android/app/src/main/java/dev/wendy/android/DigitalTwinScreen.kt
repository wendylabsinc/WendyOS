package dev.wendy.android

import android.content.Intent
import android.net.Uri
import android.provider.OpenableColumns
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.outlined.ArrowBack
import androidx.compose.material.icons.automirrored.outlined.ArrowForward
import androidx.compose.material.icons.outlined.*
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.core.content.FileProvider
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import java.io.File

/**
 * The connected gateway's digital twin (iOS NativeSiteView and SiteWorkspace): saved 3D captures, the
 * gateway's fixed cameras placed in them, live previews and manual alignment. Captures stay on this phone.
 * Robot maps and controls are not on Android yet.
 */
@Composable
fun DigitalTwinScreen(model: WendyModel) {
    val context = LocalContext.current
    val nav = LocalNavigator.current
    val page = LocalNavPage.current
    val scope = rememberCoroutineScope()
    val store = remember { SiteModelStore(File(context.filesDir, "WendySites")) }
    var revision by remember { mutableIntStateOf(0) }
    var sites by remember { mutableStateOf<List<StoredSite>?>(null) }
    var index by remember { mutableIntStateOf(0) }
    var error by remember { mutableStateOf<String?>(null) }
    var notice by remember { mutableStateOf<String?>(null) }
    var gateway by remember { mutableStateOf(SiteGatewayIdentity()) }
    var placing by remember { mutableStateOf<String?>(null) }
    var selected by remember { mutableStateOf<String?>(null) }
    var removing by remember { mutableStateOf(false) }
    var busy by remember { mutableStateOf(false) }
    var reveal by remember { mutableStateOf<String?>(null) }
    val session = model.session
    LaunchedEffect(revision) {
        sites = try { withContext(Dispatchers.IO) { store.list() } } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message; emptyList() }
        reveal?.let { id -> sites?.indexOfFirst { it.id == id }?.takeIf { it >= 0 }?.let { index = it }; reveal = null }
        index = index.coerceIn(0, maxOf(0, (sites?.size ?: 1) - 1))
    }
    LaunchedEffect(session) {
        gateway = SiteGatewayIdentity(); notice = null
        val device = model.selected ?: return@LaunchedEffect
        if (session == 0L) return@LaunchedEffect
        try {
            val info = runCatching { model.backend.call("DeviceInfo", session).obj() }.getOrNull()
            val enrollment = runCatching { model.backend.call("DeviceEnrollment", session).obj() }.getOrNull()
            val identity = SiteGatewayIdentity.resolve(SiteGatewayIdentity.cloud(device.selector), enrollment?.flag("Enrolled") == true, enrollment?.text("OrganizationID") ?: "",
                enrollment?.text("DeviceID") ?: "", info?.text("serial_number"))
            if (session == model.session) gateway = identity
        } catch (e: CancellationException) { throw e } catch (e: Exception) { if (session == model.session) notice = e.message }
    }
    val importer = rememberLauncherForActivityResult(ActivityResultContracts.OpenDocument()) { uri: Uri? ->
        if (uri == null) return@rememberLauncherForActivityResult
        busy = true; error = null
        scope.launch {
            try {
                val saved = withContext(Dispatchers.IO) {
                    val name = context.contentResolver.query(uri, arrayOf(OpenableColumns.DISPLAY_NAME), null, null, null)?.use { c -> if (c.moveToFirst()) c.getString(0) else null } ?: "Digital Twin"
                    val bytes = context.contentResolver.openInputStream(uri)?.use { readBounded(it, SiteArchive.MAXIMUM_BYTES) } ?: throw SiteException("This file could not be read.")
                    if (SiteArchive.isZip(bytes)) { UsdReader.read(bytes); store.create(name.substringBeforeLast('.').take(200).ifBlank { "Digital Twin" }, bytes) }
                    else { val archive = SiteArchive.parse(bytes); UsdReader.read(archive.model); store.importArchive(archive) }
                }
                reveal = saved.id; revision++
            } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { busy = false }
        }
    }
    fun share(saved: StoredSite) = scope.launch {
        try {
            val file = withContext(Dispatchers.IO) {
                val folder = File(context.cacheDir, "sites").apply { deleteRecursively(); mkdirs() }
                File(folder, SiteModelStore.shareFilename(saved.site.name)).also { it.writeBytes(store.archive(saved.id).data()) }
            }
            val uri = FileProvider.getUriForFile(context, "${context.packageName}.files", file)
            context.startActivity(Intent.createChooser(Intent(Intent.ACTION_SEND).setType("application/octet-stream").putExtra(Intent.EXTRA_STREAM, uri).addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION), "Share Digital Twin"))
        } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message }
    }
    val list = sites
    val current = list?.getOrNull(index)
    LaunchedEffect(current?.id, list?.size) {
        page?.actions = {
            var open by remember { mutableStateOf(false) }
            Box {
                IconButton(onClick = { open = true }) { Icon(Icons.Outlined.MoreHoriz, "Site actions") }
                DropdownMenu(open, onDismissRequest = { open = false }) {
                    DropdownMenuItem(text = { Text("Import Model or Site") }, leadingIcon = { Icon(Icons.Outlined.Download, null) }, onClick = { open = false; importer.launch(arrayOf("*/*")) })
                    current?.let { saved ->
                        DropdownMenuItem(text = { Text("Share Digital Twin…") }, leadingIcon = { Icon(Icons.Outlined.Share, null) }, onClick = { open = false; share(saved) })
                        DropdownMenuItem(text = { Text("Remove Capture…", color = MaterialTheme.colorScheme.error) }, leadingIcon = { Icon(Icons.Outlined.Delete, null) }, onClick = { open = false; removing = true })
                    }
                    DropdownMenuItem(text = { Text("Recently Removed") }, leadingIcon = { Icon(Icons.Outlined.History, null) }, onClick = { open = false; nav.push("Recently Removed") { RecentlyRemoved(store) { revision++ } } })
                }
            }
        }
    }
    when {
        list == null -> Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) { LensLoading("Loading sites…") }
        current == null -> Column(Modifier.fillMaxSize().padding(28.dp), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(14.dp, Alignment.CenterVertically)) {
            Icon(Icons.Outlined.ViewInAr, null, Modifier.size(44.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
            Text("Create Your Site", style = MaterialTheme.typography.titleLarge, fontWeight = FontWeight.Bold)
            SecondaryText("Import a USDZ model or a .wendysite file, then place ${model.selected?.title ?: "this gateway"}’s cameras in it. Room scanning needs an iPhone or iPad with LiDAR.")
            LensButton("Import Model", primary = true, enabled = !busy) { importer.launch(arrayOf("*/*")) }
            if (busy) LoadingRow("Saving model…")
            error?.let { Text(it, color = MaterialTheme.colorScheme.error) }
        }
        else -> TwinWorkspace(model, store, current, revision, gateway, placing, selected, busy, notice ?: model.camerasError, error,
            previous = if (list.size > 1) ({ index = (index - 1 + list.size) % list.size; placing = null }) else null,
            next = if (list.size > 1) ({ index = (index + 1) % list.size; placing = null }) else null,
            setPlacing = { placing = it }, setSelected = { selected = it }, changed = { revision++ }, setBusy = { busy = it }, setError = { error = it })
    }
    if (removing && current != null) ConfirmAction("Remove Capture", "Move ${current.site.name} and its camera placements to Recently Removed on this device? You can restore it later.", "Remove Capture",
        onDismiss = { removing = false }) {
        removing = false
        try { store.remove(current.id); revision++ } catch (e: Exception) { error = e.message }
    }
}

@Composable
private fun TwinWorkspace(model: WendyModel, store: SiteModelStore, current: StoredSite, revision: Int, gateway: SiteGatewayIdentity, placing: String?, selected: String?, busy: Boolean,
                          notice: String?, error: String?, previous: (() -> Unit)?, next: (() -> Unit)?, setPlacing: (String?) -> Unit, setSelected: (String?) -> Unit,
                          changed: () -> Unit, setBusy: (Boolean) -> Unit, setError: (String?) -> Unit) {
    val nav = LocalNavigator.current
    val scope = rememberCoroutineScope()
    val (saved, geometry, loadError) = rememberSiteModel(store, current.id, revision)
    val site = saved?.site ?: current.site
    fun persist(value: Site) {
        setBusy(true); setError(null)
        scope.launch {
            try { withContext(Dispatchers.IO) { store.save(value) }; setPlacing(null); changed() }
            catch (e: CancellationException) { throw e } catch (e: Exception) { setError(e.message) } finally { setBusy(false) }
        }
    }
    // The gateway's saved alignments, and background alignment of unplaced cameras (iOS NativeSiteView).
    val records = rememberSiteCameraRecords(model, saved != null && gateway.id.isNotEmpty(), known = gateway.takeIf { it.id.isNotEmpty() }, refresh = revision)
    val statuses = rememberAutomaticAlignment(model, saved, records, enabled = saved != null && placing == null)
    val coverage = saved?.let { records.geometry(model, it) }.orEmpty()
    // Fixed cameras of this gateway with a persistent identity, unique on the device.
    val groups = model.cameras.groupBy { it.text("stable_id") }
    val available = model.cameras.filter { val id = it.text("stable_id"); id.isNotEmpty() && groups[id]?.size == 1 }
    Column(Modifier.fillMaxSize()) {
        Row(Modifier.fillMaxWidth().padding(horizontal = 8.dp), verticalAlignment = Alignment.CenterVertically) {
            IconButton(onClick = { previous?.invoke() }, enabled = previous != null) { Icon(Icons.AutoMirrored.Outlined.ArrowBack, "Previous site") }
            Text(site.name, Modifier.weight(1f), style = MaterialTheme.typography.titleMedium, maxLines = 1, overflow = TextOverflow.Ellipsis)
            IconButton(onClick = { next?.invoke() }, enabled = next != null) { Icon(Icons.AutoMirrored.Outlined.ArrowForward, "Next site") }
        }
        Box(Modifier.fillMaxWidth().weight(1f)) {
            when {
                geometry != null -> SiteSceneView(geometry, site.cameras.mapNotNull { siteMarker(it, it.id == selected, it.id == placing, coverage[it.id]) }, placing != null && !busy, onSelect = setSelected,
                    onPlace = { point ->
                        val id = placing ?: return@SiteSceneView
                        val i = site.cameras.indexOfFirst { it.id == id }
                        if (i >= 0 && point.valid) persist(site.copy(cameras = site.cameras.toMutableList().also { it[i] = it[i].copy(position = point) }))
                    }, modifier = Modifier.fillMaxSize())
                loadError != null -> EmptyMessage("Model unavailable", loadError, Icons.Outlined.ViewInAr)
                else -> Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) { LensLoading("Loading model…") }
            }
            if (placing != null) Surface(Modifier.align(Alignment.TopCenter).padding(12.dp), shape = CircleShape, color = MaterialTheme.colorScheme.surface.copy(alpha = .92f)) {
                Row(Modifier.padding(horizontal = 14.dp, vertical = 6.dp), verticalAlignment = Alignment.CenterVertically) {
                    Text("Tap the model where the camera is mounted.", style = MaterialTheme.typography.bodyMedium)
                    TextButton(onClick = { setPlacing(null) }) { Text("Cancel Placement") }
                }
            }
        }
        Surface(Modifier.fillMaxWidth().heightIn(max = 300.dp), color = MaterialTheme.colorScheme.background) {
            LazyColumn(contentPadding = PaddingValues(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                item {
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        Text("Cameras · ${site.cameras.size}", Modifier.weight(1f), style = MaterialTheme.typography.titleSmall, fontWeight = FontWeight.SemiBold)
                        TextButton(onClick = { nav.push("Pair cameras") { PairingPage(model, model.selected?.title ?: "Gateway") } }) { Icon(Icons.Outlined.Add, null, Modifier.size(16.dp)); Text("Pair cameras") }
                    }
                }
                notice?.let { item { SecondaryText(it) } }
                items(site.cameras, key = { it.id }) { camera ->
                    val feed = if (gateway.matches(camera.gatewayID)) available.firstOrNull { it.text("stable_id") == camera.sourceID } else null
                    var menu by remember(camera.id) { mutableStateOf(false) }
                    Surface(shape = RoundedCornerShape(16.dp), color = if (placing == camera.id) LensStyle.green.copy(alpha = .12f) else MaterialTheme.colorScheme.surface) {
                        Row(Modifier.fillMaxWidth().padding(12.dp), horizontalArrangement = Arrangement.spacedBy(10.dp), verticalAlignment = Alignment.CenterVertically) {
                            Icon(if (camera.position == null) Icons.Outlined.PhotoCamera else Icons.Outlined.CheckCircle, null, tint = LensStyle.accent)
                            Column(Modifier.weight(1f)) {
                                Text(camera.name)
                                SecondaryText(statuses[camera.sourceID]?.takeIf { coverage[camera.id] == null } ?: alignmentLabel(camera, coverage[camera.id]))
                            }
                            if (feed != null) IconButton(onClick = { nav.push(camera.name) { Column(Modifier.padding(16.dp)) { CameraTile(model, feed) } } }) { Icon(Icons.Outlined.PlayCircle, "Preview ${camera.name}") }
                            Box {
                                IconButton(onClick = { menu = true }, enabled = !busy) { Icon(Icons.Outlined.MoreHoriz, "Actions for ${camera.name}") }
                                DropdownMenu(menu, onDismissRequest = { menu = false }) {
                                    DropdownMenuItem(text = { Text(if (camera.position == null) "Place in model" else "Move camera") }, onClick = { menu = false; setPlacing(camera.id) })
                                    if (feed != null && saved != null) DropdownMenuItem(text = { Text("Calibrate and Align") }, onClick = {
                                        menu = false
                                        val request = SiteAlignmentRequest(saved, camera, gateway, model.session)
                                        nav.push("Align camera") {
                                            SiteCameraAlignContent(model, store, request, feed, saveManual = { value -> store.save(value) }, onSaved = { result ->
                                                withContext(Dispatchers.IO) {
                                                    val value = store.load(saved.id).site
                                                    val index = value.cameras.indexOfFirst { it.id == camera.id }
                                                    if (index >= 0) store.save(value.copy(cameras = value.cameras.toMutableList().also { it[index] = it[index].copy(position = result.pose.position, manualAlignment = null) }))
                                                }
                                                changed()
                                            })
                                        }
                                    })
                                    DropdownMenuItem(text = { Text("Remove from Model", color = MaterialTheme.colorScheme.error) }, onClick = { menu = false; persist(site.copy(cameras = site.cameras.filter { it.id != camera.id })) })
                                }
                            }
                        }
                    }
                }
                val unplaced = available.filter { c -> site.cameras.none { gateway.matches(it.gatewayID) && it.sourceID == c.text("stable_id") } }
                if (gateway.id.isNotEmpty() && unplaced.isNotEmpty()) item {
                    Text("AVAILABLE CAMERAS", Modifier.padding(top = 8.dp), style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                }
                if (gateway.id.isNotEmpty()) items(unplaced, key = { "add:" + it.text("stable_id") }) { camera ->
                    LensSection(null) {
                        LensRow("Add ${camera.text("name").ifEmpty { "Camera" }}", "Place it in ${site.name}", Icons.Outlined.Add, enabled = !busy) {
                            val added = SiteCamera(gatewayID = gateway.id, sourceID = camera.text("stable_id"), name = camera.text("name").ifEmpty { "Camera" })
                            setBusy(true)
                            scope.launch {
                                try { withContext(Dispatchers.IO) { store.save(site.copy(cameras = site.cameras + added)) }; changed(); setPlacing(added.id) }
                                catch (e: CancellationException) { throw e } catch (e: Exception) { setError(e.message) } finally { setBusy(false) }
                            }
                        }
                    }
                }
                model.cameras.filter { it.text("stable_id").isEmpty() }.map { it.text("name") }.takeIf { it.isNotEmpty() }?.let { names ->
                    item { SecondaryText("${model.selected?.title ?: "This gateway"} has no persistent camera ID for ${names.joinToString()}. Those cameras cannot be saved in the model yet.") }
                }
                saved?.let { value -> if (value.site.cameras.isNotEmpty()) item { SecondaryText(coverageExplanation(value, records, coverage)) } }
                item { SecondaryText("Robot maps, localization and controls are not on Android yet.") }
                if (busy) item { LoadingRow("Saving placement…") }
                error?.let { item { Text(it, color = MaterialTheme.colorScheme.error) } }
            }
        }
    }
}

@Composable
private fun RecentlyRemoved(store: SiteModelStore, restored: () -> Unit) {
    var removed by remember { mutableStateOf(store.removed()) }
    var error by remember { mutableStateOf<String?>(null) }
    PageColumn {
        if (removed.isEmpty()) item { EmptyMessage("No removed captures", "", Icons.Outlined.History) }
        formSection {
            removed.forEach { capture ->
                Row(Modifier.fillMaxWidth().padding(vertical = 6.dp), verticalAlignment = Alignment.CenterVertically) {
                    Column(Modifier.weight(1f)) { Text(capture.site.name); SecondaryText("${capture.site.cameras.size} camera placements") }
                    TextButton(onClick = { try { store.restore(capture.id); removed = store.removed(); restored() } catch (e: Exception) { error = e.message } }) { Text("Restore") }
                }
            }
        }
        error?.let { item { Text(it, color = MaterialTheme.colorScheme.error) } }
    }
}
