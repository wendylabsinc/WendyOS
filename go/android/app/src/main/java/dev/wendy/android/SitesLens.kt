package dev.wendy.android

import android.net.Uri
import android.provider.OpenableColumns
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import java.io.File
import java.text.DateFormat
import java.util.Date
import java.util.UUID

private enum class SitesTab(val title: String) { Home("Home"), Missions("Missions"), Activity("Activity") }

private val sitesBackground: Color @Composable get() = if (LensStyle.isDark) MaterialTheme.colorScheme.background else Color(0xFFF1EEE7)

/** Sites (iOS MobileSitesLens): local sites that bring devices, robots and 3D models together. Nothing here changes Cloud. */
@Composable
fun SitesLens(model: WendyModel, askWendy: () -> Unit) {
    val context = LocalContext.current
    val nav = LocalNavigator.current
    val page = LocalNavPage.current
    val store = remember(model.account, model.organization) { SitesStore(File(context.filesDir, "MobileSites"), model.account, model.organization) }
    var tab by remember { mutableStateOf(SitesTab.Home) }
    var message by remember { mutableStateOf<String?>(null) }
    fun perform(action: () -> Unit) = try { action() } catch (e: Exception) { message = e.message }
    LaunchedEffect(model.account, model.organization) { model.refresh() }
    LaunchedEffect(tab) { page?.title = tab.title }
    // The site switcher and Create Site sit in the toolbar, as on iOS.
    LaunchedEffect(store, store.revision) {
        page?.actions = {
            if (store.sites.isNotEmpty()) {
                var open by remember { mutableStateOf(false) }
                Box {
                    TextButton(onClick = { open = true }) { Text(store.current?.name ?: "Sites", maxLines = 1, overflow = TextOverflow.Ellipsis, fontWeight = FontWeight.SemiBold); Icon(Icons.Outlined.ExpandMore, null, Modifier.size(16.dp)) }
                    DropdownMenu(open, onDismissRequest = { open = false }) {
                        store.sites.forEach { site -> DropdownMenuItem(text = { Text(site.name) }, onClick = { open = false; perform { store.select(site.id) } },
                            trailingIcon = if (store.current?.id == site.id) ({ Icon(Icons.Outlined.Check, "Selected") }) else null) }
                    }
                }
                IconButton(onClick = { nav.push("New Site") { SiteEditor(model, store, null) { tab = SitesTab.Home } } }, enabled = store.loadError == null && model.initialized) { Icon(Icons.Outlined.Add, "Create Site") }
            }
        }
    }
    Column(Modifier.fillMaxSize().background(sitesBackground)) {
        Box(Modifier.weight(1f)) {
            when (tab) {
                SitesTab.Home -> SitesHome(model, store, { message = it }) { tab = SitesTab.Home }
                SitesTab.Missions -> SitesEmpty(Icons.Outlined.Flag, "Missions",
                    if (store.current?.models?.isEmpty() != false) "Missions use places from a 3D model. Add a model to prepare your site." else "Mission planning and execution are not available in this build yet.") {
                    val site = store.current
                    if (site != null && site.models.isEmpty()) LensButton("Add a 3D Model", primary = true) { nav.push("Add a 3D model") { AddSiteModelPage(store, site.id) } }
                    else if (site == null) LensButton("Create Site", primary = true, enabled = store.loadError == null) { nav.push("New Site") { SiteEditor(model, store, null) {} } }
                }
                SitesTab.Activity -> {
                    val events = store.events.filter { it.siteID == store.current?.id }.reversed()
                    if (events.isEmpty()) SitesEmpty(Icons.Outlined.Schedule, "No activity yet", "Site creation and model additions will appear here.") {}
                    else LazyColumn(contentPadding = PaddingValues(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                        items(events, key = { it.id }) { event ->
                            Surface(shape = RoundedCornerShape(14.dp), color = MaterialTheme.colorScheme.surface) {
                                Column(Modifier.fillMaxWidth().padding(14.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                                    Text(event.text); SecondaryText(DateFormat.getDateTimeInstance(DateFormat.MEDIUM, DateFormat.SHORT).format(Date(event.date)))
                                }
                            }
                        }
                    }
                }
            }
        }
        AskWendyBar(model, askWendy)
        NavigationBar(containerColor = MaterialTheme.colorScheme.surface) {
            SitesTab.entries.forEach { item ->
                NavigationBarItem(selected = tab == item, onClick = { tab = item }, label = { Text(item.title) },
                    icon = { Icon(when (item) { SitesTab.Home -> Icons.Outlined.Home; SitesTab.Missions -> Icons.Outlined.Flag; SitesTab.Activity -> Icons.Outlined.Schedule }, null) },
                    colors = NavigationBarItemDefaults.colors(selectedIconColor = LensStyle.accent, selectedTextColor = LensStyle.accent, indicatorColor = LensStyle.green.copy(alpha = .12f)))
            }
        }
    }
    message?.let { text -> AlertDialog(onDismissRequest = { message = null }, title = { Text("Sites") }, text = { Text(text) }, confirmButton = { TextButton(onClick = { message = null }) { Text("OK") } }) }
}

@Composable
private fun SitesEmpty(icon: androidx.compose.ui.graphics.vector.ImageVector, title: String, description: String, actions: @Composable ColumnScope.() -> Unit) {
    Column(Modifier.fillMaxSize().padding(28.dp), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(14.dp, Alignment.CenterVertically)) {
        Icon(icon, null, Modifier.size(44.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
        Text(title, style = MaterialTheme.typography.titleLarge, fontWeight = FontWeight.Bold, textAlign = TextAlign.Center)
        Text(description, color = MaterialTheme.colorScheme.onSurfaceVariant, textAlign = TextAlign.Center)
        actions()
    }
}

@Composable
private fun SitesHome(model: WendyModel, store: SitesStore, alert: (String) -> Unit, created: () -> Unit) {
    val nav = LocalNavigator.current
    fun perform(action: () -> Unit) = try { action() } catch (e: Exception) { alert(e.message ?: "Could not update the site.") }
    val site = store.current
    if (site == null) {
        SitesEmpty(Icons.Outlined.Business, "Set up your first site", store.loadError ?: "Bring your devices, robots and 3D models together in one place.") {
            LensButton("Create Site", primary = true, enabled = store.loadError == null && model.initialized) { nav.push("New Site") { SiteEditor(model, store, null, created) } }
            SecondaryText("Saved on this phone. Cloud sync is not available yet.")
        }
        return
    }
    fun openModel(link: SiteModelLink) = nav.push(link.name) { SiteModelViewer(model, store, site.id, link) }
    fun openCameras(link: SiteModelLink) = nav.push("Place Cameras") { SiteCamerasPage(model, store, site.id, link.id) }
    LazyColumn(Modifier.fillMaxSize(), contentPadding = PaddingValues(16.dp), verticalArrangement = Arrangement.spacedBy(16.dp)) {
        item(key = "setup") {
            if (!site.hideSetup) LensSection(null) {
                Row(Modifier.fillMaxWidth().padding(start = 16.dp, end = 4.dp, top = 6.dp), verticalAlignment = Alignment.CenterVertically) {
                    Text("Finish setting up · ${if (site.models.isEmpty()) 1 else 2} of 4", Modifier.weight(1f), style = MaterialTheme.typography.labelLarge, fontWeight = FontWeight.SemiBold, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    TextButton(onClick = { perform { store.setSetupHidden(true, site.id) } }) { Text("Not Now") }
                }
                LensRow("Site, devices and robots", site.assetSummary, Icons.Outlined.CheckCircle, navigates = false)
                LensDivider()
                if (site.models.isEmpty()) LensRow("Add a 3D model", "", Icons.Outlined.ViewInAr) { nav.push("Add a 3D model") { AddSiteModelPage(store, site.id) } }
                else LensRow("3D model added", "", Icons.Outlined.CheckCircle, LensStyle.offline, navigates = false)
                LensDivider()
                LensRow("Place and calibrate cameras", "", Icons.Outlined.PhotoCamera, if (site.models.isEmpty()) LensStyle.offline else LensStyle.green) {
                    site.primaryModel?.let(::openCameras) ?: alert("Add a 3D model first.")
                }
                LensDivider()
                LensRow("Create a mission", "", Icons.Outlined.Flag, if (site.models.isEmpty()) LensStyle.offline else LensStyle.green) {
                    alert(if (site.models.isEmpty()) "Add a 3D model first." else "Create a mission is not available in this build yet.")
                }
            } else TextButton(onClick = { perform { store.setSetupHidden(false, site.id) } }) { Text("Continue setup") }
        }
        item(key = "models") {
            LensSection("3D models") {
                site.models.forEach { link ->
                    var menu by remember(link.id) { mutableStateOf(false) }
                    var renaming by remember(link.id) { mutableStateOf(false) }
                    Row(Modifier.fillMaxWidth().clickable { openModel(link) }.padding(horizontal = 12.dp, vertical = 8.dp), horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
                        SiteModelThumbnailView(store, link.id)
                        Column(Modifier.weight(1f)) {
                            Text(link.name, maxLines = 2, overflow = TextOverflow.Ellipsis)
                            if (site.defaultModel == link.id) SecondaryText("Default")
                        }
                        Box {
                            IconButton(onClick = { menu = true }) { Icon(Icons.Outlined.MoreHoriz, "Options for ${link.name}") }
                            DropdownMenu(menu, onDismissRequest = { menu = false }) {
                                DropdownMenuItem(text = { Text("Rename") }, leadingIcon = { Icon(Icons.Outlined.Edit, null) }, onClick = { menu = false; renaming = true })
                                DropdownMenuItem(text = { Text("Save to device") }, leadingIcon = { Icon(Icons.Outlined.Upload, null) }, onClick = { menu = false; nav.push("Save to device") { SiteDeviceModelsPage(model, store, site.id, link) } })
                                DropdownMenuItem(text = { Text("Make default") }, leadingIcon = { Icon(Icons.Outlined.Check, null) }, onClick = { menu = false; perform { store.setDefault(link.id, site.id) } })
                            }
                        }
                    }
                    if (renaming) InputDialog("Rename model", listOf(Field("name", "Model name", link.name)), onDismiss = { renaming = false }, submitLabel = "Save") { values ->
                        renaming = false; perform { store.renameModel(link.id, site.id, values.getValue("name")) }
                    }
                    LensDivider()
                }
                LensRow("Add a 3D model", "", Icons.Outlined.Add) { nav.push("Add a 3D model") { AddSiteModelPage(store, site.id) } }
                LensDivider()
                LensRow("Load from device", "", Icons.Outlined.Download) { nav.push("Load from device") { SiteDeviceModelsPage(model, store, site.id, null) } }
            }
        }
        site.primaryModel?.let { link -> item(key = "live") { SiteLiveCameras(model, store, site, link) } }
        item(key = "assets") {
            LensSection("Devices and Robots") {
                LensRow("Edit devices and robots", "", Icons.Outlined.Edit) { nav.push("Edit devices and robots") { SiteEditor(model, store, site) {} } }
                if (site.assets.isEmpty()) Text("No devices or robots selected", Modifier.padding(16.dp), color = MaterialTheme.colorScheme.onSurfaceVariant)
                site.assets.forEach { asset -> LensDivider(); LensRow(asset.name, "", if (asset.robot) Icons.Outlined.SmartToy else Icons.Outlined.Memory, navigates = false) }
            }
        }
        item(key = "footer") {
            Column(Modifier.padding(horizontal = 16.dp)) {
                SecondaryText("On this phone")
                SecondaryText("Sites, model links and activity are saved locally for this account and organization. Cloud sync is not available yet.")
            }
        }
    }
}

/** Thumbnails render on the CPU from the saved model and are cached by file and revision. */
private object SiteThumbnailCache { val images = android.util.LruCache<String, android.graphics.Bitmap>(24) }

@Composable
private fun SiteModelThumbnailView(store: SitesStore, modelID: String) {
    var image by remember(modelID) { mutableStateOf<android.graphics.Bitmap?>(null) }
    var loading by remember(modelID) { mutableStateOf(true) }
    LaunchedEffect(modelID, store.revision) {
        loading = true
        image = try {
            withContext(Dispatchers.Default) {
                val saved = store.models.load(modelID)
                val key = saved.model.path + saved.site.modelRevision
                SiteThumbnailCache.images.get(key) ?: SiteThumbnail.render(scaled(UsdReader.read(saved.model.readBytes()), saved.site.metresPerModelUnit), 176, 144).also { SiteThumbnailCache.images.put(key, it) }
            }
        } catch (e: CancellationException) { throw e } catch (_: Exception) { null }
        loading = false
    }
    Box(Modifier.size(88.dp, 72.dp).clip(RoundedCornerShape(12.dp)).background(MaterialTheme.colorScheme.onSurface.copy(alpha = .08f)), contentAlignment = Alignment.Center) {
        when {
            image != null -> Image(image!!.asImageBitmap(), "3D model preview", Modifier.fillMaxSize().padding(4.dp))
            loading -> CircularProgressIndicator(Modifier.size(18.dp), strokeWidth = 2.dp)
            else -> Icon(Icons.Outlined.ViewInAr, "3D model preview unavailable", tint = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}

/** A site model's geometry in metres, after the site's own scale. */
internal fun scaled(geometry: SiteGeometry, metresPerUnit: Double): SiteGeometry {
    if (metresPerUnit == 1.0) return geometry
    val s = metresPerUnit.toFloat()
    return SiteGeometry(geometry.meshes.map { m -> SiteMesh(m.name, FloatArray(m.positions.size) { m.positions[it] * s }, m.normals, m.color) })
}

private class AssetChoice(val asset: SiteAsset, val status: String, val online: Boolean)

/** Create a site, or edit its devices and robots (iOS MobileCreateSite). */
@Composable
private fun SiteEditor(model: WendyModel, store: SitesStore, editing: SiteRecord?, created: () -> Unit) {
    val nav = LocalNavigator.current
    val page = LocalNavPage.current
    var name by remember { mutableStateOf(editing?.name ?: "") }
    val selected = remember { mutableStateListOf<String>().also { it.addAll(editing?.assets?.map { a -> a.id }.orEmpty()) } }
    var choices by remember { mutableStateOf(editing?.assets?.map { AssetChoice(it, "Offline", false) }.orEmpty()) }
    var loadingRobots by remember { mutableStateOf(true) }
    var robotError by remember { mutableStateOf<String?>(null) }
    var error by remember { mutableStateOf<String?>(null) }
    val inventory = (model.nearbyDevices + model.cloudDevices).distinctBy { recordingDeviceID(it) }
    LaunchedEffect(inventory) {
        val known = choices.map { it.asset.id }.toSet()
        val refreshed = inventory.map { device ->
            val status = siteStatus(model, device)
            AssetChoice(SiteAsset(recordingDeviceID(device), device.title, false), status, status in setOf("Online", "Connected", "Nearby"))
        }
        val ids = refreshed.map { it.asset.id }.toSet()
        // Discovery may finish later: add new rows without overwriting choices already made.
        choices = refreshed + choices.filter { it.asset.robot || it.asset.id !in ids }.map { if (it.asset.robot) it else AssetChoice(it.asset, "Offline", false) }
        if (editing == null) refreshed.filter { it.online && it.asset.id !in known && it.asset.id !in selected }.forEach { selected += it.asset.id }
    }
    LaunchedEffect(Unit) {
        try {
            val snapshot = model.backend.call("UnitreePairings", 0).obj()
            val seen = HashSet<String>()
            val robots = snapshot.list("Robots").filter { it.text("Serial").isNotEmpty() && seen.add(it.text("Serial")) }.map {
                AssetChoice(SiteAsset("unitree:" + it.text("Serial"), it.text("Name").ifEmpty { it.text("Serial") }, true), if (it.flag("Connected")) "Connected" else "Not connected", it.flag("Connected"))
            }
            val ids = robots.map { it.asset.id }.toSet()
            choices = choices.filter { it.asset.id !in ids } + robots
            if (editing == null) robots.filter { it.online && it.asset.id !in selected }.forEach { selected += it.asset.id }
            robotError = snapshot.text("Warning").ifEmpty { null }
        } catch (e: CancellationException) { throw e } catch (e: Exception) { robotError = e.message } finally { loadingRobots = false }
    }
    fun save() {
        try {
            val assets = choices.filter { it.asset.id in selected }.map { it.asset }
            if (editing != null) store.setAssets(assets, editing.id) else store.create(name, assets)
            created(); nav.pop()
        } catch (e: Exception) { error = e.message }
    }
    LaunchedEffect(name, selected.toList(), choices) {
        page?.actions = { TextButton(onClick = ::save, enabled = (editing != null || (name.isNotBlank() && name.length <= 200)) && model.initialized) { Text(if (editing == null) "Create" else "Save", fontWeight = FontWeight.SemiBold) } }
    }
    PageColumn(Modifier.background(sitesBackground)) {
        if (editing == null) formSection("Name") { OutlinedTextField(name, { name = it }, Modifier.fillMaxWidth(), placeholder = { Text("e.g. Oakland warehouse") }, singleLine = true) }
        formSection("Devices and robots") {
            NavRow("Devices", value = "${choices.count { !it.asset.robot && it.asset.id in selected }} selected") { nav.push("Devices") { AssetPicker("Devices", { choices.filter { c -> !c.asset.robot } }, selected) } }
            NavRow("Robots", value = "${choices.count { it.asset.robot && it.asset.id in selected }} selected") { nav.push("Robots") { AssetPicker("Robots", { choices.filter { c -> c.asset.robot } }, selected, loadingRobots, robotError) } }
        }
        formSection(footer = if (editing == null) "This creates a local site. It does not change Cloud device assignments or pair robots." else "Changes apply to this site on your phone. Saved models and camera placements are kept.") {
            SecondaryText("On this phone")
        }
        error?.let { item { Text(it, color = MaterialTheme.colorScheme.error) } }
    }
}

private fun siteStatus(model: WendyModel, device: Device): String = when {
    model.session != 0L && model.selected?.selector == device.selector -> "Connected"
    !device.isCloud -> "Nearby"
    model.cloudError != null || model.presenceError.isNotEmpty() || System.currentTimeMillis() - model.presenceAt > 60_000 -> "Status unknown"
    else -> model.presence?.firstOrNull { it.selector == device.selector }?.let { if (it.online) "Online" else "Offline" } ?: "Status unknown"
}

@Composable
private fun AssetPicker(title: String, choices: () -> List<AssetChoice>, selected: MutableList<String>, loading: Boolean = false, warning: String? = null) {
    var search by remember { mutableStateOf("") }
    val visible = choices().filter { search.isBlank() || it.asset.name.contains(search.trim(), ignoreCase = true) }
        .sortedWith(compareByDescending<AssetChoice> { it.online }.thenBy { it.asset.name.lowercase() }.thenBy { it.asset.id })
    PageColumn(Modifier.background(sitesBackground)) {
        item { OutlinedTextField(search, { search = it }, Modifier.fillMaxWidth(), placeholder = { Text("Search by name") }, leadingIcon = { Icon(Icons.Outlined.Search, null) }, singleLine = true) }
        item {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(title.uppercase(), Modifier.weight(1f), style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                val ids = visible.map { it.asset.id }
                TextButton(onClick = { ids.filter { it !in selected }.forEach { selected += it } }, enabled = ids.isNotEmpty() && !selected.containsAll(ids)) { Text("Select All") }
                val all = choices().map { it.asset.id }.toSet()
                TextButton(onClick = { selected.removeAll { it in all } }, enabled = selected.any { it in all }) { Text("Unselect All") }
            }
        }
        formSection {
            visible.forEach { choice ->
                val picked = choice.asset.id in selected
                Row(Modifier.fillMaxWidth().clickable { if (picked) selected.remove(choice.asset.id) else selected += choice.asset.id }.padding(vertical = 8.dp),
                    horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
                    Icon(if (picked) Icons.Outlined.CheckCircle else Icons.Outlined.RadioButtonUnchecked, if (picked) "Selected" else null, tint = if (picked) LensStyle.accent else MaterialTheme.colorScheme.onSurfaceVariant)
                    Icon(if (choice.asset.robot) Icons.Outlined.SmartToy else Icons.Outlined.Memory, null, tint = if (choice.online) LensStyle.green else MaterialTheme.colorScheme.onSurfaceVariant)
                    Column(Modifier.weight(1f)) {
                        Text(choice.asset.name, color = if (choice.online) MaterialTheme.colorScheme.onSurface else MaterialTheme.colorScheme.onSurfaceVariant)
                        Row(horizontalArrangement = Arrangement.spacedBy(5.dp), verticalAlignment = Alignment.CenterVertically) {
                            Box(Modifier.size(7.dp).background(if (choice.online) LensStyle.green else LensStyle.offline, CircleShape))
                            Text(choice.status, style = MaterialTheme.typography.labelSmall, color = if (choice.online) LensStyle.accent else MaterialTheme.colorScheme.onSurfaceVariant)
                        }
                    }
                }
            }
            if (visible.isEmpty() && !loading) SecondaryText(if (search.isEmpty()) "No ${title.lowercase()} available" else "No matching ${title.lowercase()}")
        }
        if (loading) item { LoadingRow("Looking for ${title.lowercase()}…") }
        warning?.let { item { SecondaryText(it) } }
    }
}

/** Import a USDZ or .wendysite file; room scanning needs an iPhone or iPad with LiDAR (iOS MobileAddSiteModel). */
@Composable
private fun AddSiteModelPage(store: SitesStore, siteID: String) {
    val nav = LocalNavigator.current
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    var busy by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    val picker = rememberLauncherForActivityResult(ActivityResultContracts.OpenDocument()) { uri: Uri? ->
        if (uri == null) return@rememberLauncherForActivityResult
        busy = true; error = null
        scope.launch {
            try {
                val (name, bytes) = withContext(Dispatchers.IO) {
                    val name = context.contentResolver.query(uri, arrayOf(OpenableColumns.DISPLAY_NAME, OpenableColumns.SIZE), null, null, null)?.use { c ->
                        if (c.moveToFirst()) { val size = c.getLong(1); if (size > SiteArchive.MAXIMUM_BYTES) throw SiteException("This model is too large. Export a USDZ model smaller than 128 MB."); c.getString(0) } else null
                    } ?: "3D model"
                    name to (context.contentResolver.openInputStream(uri)?.use { input -> readBounded(input, SiteArchive.MAXIMUM_BYTES) } ?: throw SiteException("This file could not be read."))
                }
                withContext(Dispatchers.Default) { store.addModel(name.substringBeforeLast('.'), bytes, siteID) }
                nav.pop()
            } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message ?: "Choose a self-contained USDZ model of the site." } finally { busy = false }
        }
    }
    PageColumn(Modifier.background(sitesBackground)) {
        formSection("A site can have several models for floors, areas or rescans.", "Room scanning needs an iPhone or iPad with LiDAR. You can import USDZ or .wendysite files here. Your first model becomes the default.") {
            ActionRow("Scan with phone", Icons.Outlined.ViewInAr, enabled = false) {}
            ActionRow("Import a file", Icons.Outlined.Download, enabled = !busy) { picker.launch(arrayOf("*/*")) }
        }
        if (busy) item { LoadingRow("Saving model…") }
        error?.let { item { Text(it, color = MaterialTheme.colorScheme.error) } }
    }
}

internal fun readBounded(input: java.io.InputStream, limit: Long): ByteArray {
    val out = java.io.ByteArrayOutputStream()
    val buffer = ByteArray(256 * 1024)
    while (true) { val n = input.read(buffer); if (n < 0) break; out.write(buffer, 0, n); if (out.size() > limit) throw SiteException("This model is too large. Export a USDZ model smaller than 128 MB.") }
    return out.toByteArray()
}

/** Saved site models on a device: save this model there, or load one into the site (iOS MobileSiteDeviceModels). */
@Composable
private fun SiteDeviceModelsPage(model: WendyModel, store: SitesStore, siteID: String, saving: SiteModelLink?) {
    val nav = LocalNavigator.current
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    var device by remember { mutableStateOf<Device?>(null) }
    var session by remember { mutableLongStateOf(0L) }
    var remote by remember { mutableStateOf(emptyList<org.json.JSONObject>()) }
    var busy by remember { mutableStateOf(false) }
    var message by remember { mutableStateOf<String?>(null) }
    var error by remember { mutableStateOf<String?>(null) }
    var search by remember { mutableStateOf("") }
    val account = model.account; val organization = model.organization
    fun online(d: Device) = siteStatus(model, d) in setOf("Online", "Connected", "Nearby")
    fun connect(target: Device) {
        device = target; busy = true; error = null; message = null; session = 0; remote = emptyList()
        scope.launch {
            try {
                if (!model.connectAndWait(target)) { error = model.detailError ?: "Could not connect to this device."; return@launch }
                val captured = model.session
                val values = model.backend.call("SiteModels", captured, "list", "", "", "").items()
                if (model.session == captured && account == model.account && organization == model.organization) { remote = values; session = captured }
            } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { busy = false }
        }
    }
    fun transfer(item: org.json.JSONObject?) {
        val target = device
        if (session == 0L || model.session != session || target == null) { error = "Reconnect to the device and try again."; return }
        busy = true; error = null
        scope.launch {
            val file = File(context.cacheDir, UUID.randomUUID().toString() + ".wendysite")
            try {
                if (saving != null) {
                    withContext(Dispatchers.IO) { file.writeBytes(store.models.archive(saving.id).data()) }
                    model.backend.call("SiteModels", session, "save", file.absolutePath, saving.name, "")
                    if (account == model.account && organization == model.organization) message = "Saved to ${target.title}"
                } else if (item != null) {
                    model.backend.call("SiteModels", session, "load", file.absolutePath, "", item.text("id"))
                    val bytes = withContext(Dispatchers.IO) { file.readBytes() }
                    if (account != model.account || organization != model.organization) return@launch
                    withContext(Dispatchers.Default) { store.addModel(item.text("name").ifEmpty { "3D model" }, bytes, siteID) }
                    nav.pop()
                }
            } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { file.delete(); busy = false }
        }
    }
    val devices = (model.nearbyDevices + model.cloudDevices).distinctBy { recordingDeviceID(it) }.filter { search.isBlank() || it.title.contains(search.trim(), true) }
        .sortedWith(compareByDescending<Device> { online(it) }.thenBy { it.title.lowercase() })
    PageColumn(Modifier.background(sitesBackground)) {
        val chosen = device
        if (chosen != null) {
            formSection("Device") {
                LabeledRow(chosen.title, if (session == 0L) "Connecting" else "Connected")
                ActionRow("Change device", enabled = !busy) { device = null; session = 0; remote = emptyList(); error = null }
            }
            if (saving != null) formSection(footer = "Saves the 3D model, placements and scan references. The copy on your phone is kept.") {
                ActionRow("Save model to device", Icons.Outlined.Upload, enabled = !busy && session != 0L && message == null) { transfer(null) }
            } else if (session != 0L) formSection("Saved models") {
                remote.forEach { item ->
                    Column(Modifier.fillMaxWidth().clickable(enabled = !busy) { transfer(item) }.padding(vertical = 8.dp)) {
                        Text(item.text("name").ifEmpty { "3D model" }); SecondaryText(formatBytes(item.number("size")))
                    }
                }
                if (remote.isEmpty() && !busy && error == null) SecondaryText("No models saved on this device")
                ActionRow("Refresh", enabled = !busy) { connect(chosen) }
            }
        } else {
            item { OutlinedTextField(search, { search = it }, Modifier.fillMaxWidth(), placeholder = { Text("Search devices") }, leadingIcon = { Icon(Icons.Outlined.Search, null) }, singleLine = true) }
            formSection("Choose a device") {
                devices.forEach { item ->
                    Row(Modifier.fillMaxWidth().clickable(enabled = online(item) && !busy) { connect(item) }.padding(vertical = 8.dp), horizontalArrangement = Arrangement.spacedBy(10.dp), verticalAlignment = Alignment.CenterVertically) {
                        Box(Modifier.size(8.dp).background(if (online(item)) LensStyle.green else LensStyle.offline, CircleShape))
                        Column { Text(item.title); SecondaryText(siteStatus(model, item)) }
                    }
                }
            }
        }
        if (busy) item { LoadingRow(if (saving == null) "Loading models…" else "Preparing or saving model…") }
        message?.let { item { Row(horizontalArrangement = Arrangement.spacedBy(6.dp)) { Icon(Icons.Outlined.CheckCircle, null, tint = LensStyle.accent); Text(it, color = LensStyle.accent) } } }
        error?.let { item { Text(it, color = MaterialTheme.colorScheme.error) } }
    }
}

/** Loads one saved model's geometry for the viewer and placement pages. */
@Composable
internal fun rememberSiteModel(store: SiteModelStore, modelID: String, refresh: Int = 0): Triple<StoredSite?, SiteGeometry?, String?> {
    var saved by remember(modelID) { mutableStateOf<StoredSite?>(null) }
    var geometry by remember(modelID) { mutableStateOf<SiteGeometry?>(null) }
    var error by remember(modelID) { mutableStateOf<String?>(null) }
    LaunchedEffect(modelID, refresh) {
        try {
            val value = withContext(Dispatchers.IO) { store.load(modelID) }
            if (geometry == null || saved?.site?.modelRevision != value.site.modelRevision) geometry = withContext(Dispatchers.Default) { scaled(UsdReader.read(value.model.readBytes()), value.site.metresPerModelUnit) }
            saved = value; error = null
        } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message ?: "Model unavailable" }
    }
    return Triple(saved, geometry, error)
}

/** The model in 3D with its cameras (iOS MobileSiteModelViewer). */
@Composable
private fun SiteModelViewer(model: WendyModel, store: SitesStore, siteID: String, link: SiteModelLink) {
    val nav = LocalNavigator.current
    val page = LocalNavPage.current
    val (saved, geometry, error) = rememberSiteModel(store.models, link.id, store.revision)
    LaunchedEffect(saved != null) { page?.actions = { IconButton(onClick = { nav.push("Place Cameras") { SiteCamerasPage(model, store, siteID, link.id) } }, enabled = saved != null) { Icon(Icons.Outlined.PhotoCamera, "Cameras") } } }
    Column(Modifier.fillMaxSize()) {
        Box(Modifier.weight(1f)) {
            when {
                geometry != null && saved != null -> SiteSceneView(geometry, saved.site.cameras.mapNotNull { siteMarker(it, false, false) }, false,
                    onSelect = { id -> saved.site.cameras.firstOrNull { it.id == id }?.let { camera -> nav.push(camera.name) { SiteCameraLivePage(model, store, store.sites.firstOrNull { s -> s.id == siteID }, camera, saved) } } }, onPlace = {}, modifier = Modifier.fillMaxSize())
                error != null -> EmptyMessage("Model unavailable", error, Icons.Outlined.ViewInAr)
                else -> Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) { LensLoading("Loading model…") }
            }
        }
        Surface(color = MaterialTheme.colorScheme.surface) {
            LensButton("Save to device", primary = true, enabled = saved != null, modifier = Modifier.fillMaxWidth().padding(16.dp)) { nav.push("Save to device") { SiteDeviceModelsPage(model, store, siteID, link) } }
        }
    }
}

/** Place a site's fixed cameras on a model, preview them live and align them (iOS MobileSiteCameras). */
@Composable
private fun SiteCamerasPage(model: WendyModel, store: SitesStore, siteID: String, initialModel: String) {
    val nav = LocalNavigator.current
    val page = LocalNavPage.current
    val scope = rememberCoroutineScope()
    val site = store.sites.firstOrNull { it.id == siteID }
    var modelID by remember { mutableStateOf(initialModel) }
    var refresh by remember { mutableIntStateOf(0) }
    val (saved, geometry, loadError) = rememberSiteModel(store.models, modelID, refresh + store.revision)
    var placing by remember { mutableStateOf<String?>(null) }
    var selected by remember { mutableStateOf<String?>(null) }
    var busy by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    fun persist(value: Site, text: String, next: String?) {
        busy = true; error = null
        scope.launch {
            try { withContext(Dispatchers.IO) { store.saveCameraModel(value, siteID, text) }; placing = next; refresh++ }
            catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { busy = false }
        }
    }
    LaunchedEffect(site?.models, modelID) {
        page?.title = site?.models?.firstOrNull { it.id == modelID }?.name ?: "Place Cameras"
        page?.actions = {
            var open by remember { mutableStateOf(false) }
            if ((site?.models?.size ?: 0) > 1) Box {
                IconButton(onClick = { open = true }, enabled = !busy) { Icon(Icons.Outlined.Layers, "Choose model") }
                DropdownMenu(open, onDismissRequest = { open = false }) { site?.models?.forEach { link -> DropdownMenuItem(text = { Text(link.name) }, onClick = { open = false; modelID = link.id; placing = null }) } }
            }
        }
    }
    val value = saved?.site
    Column(Modifier.fillMaxSize()) {
        Box(Modifier.fillMaxWidth().weight(1f)) {
            when {
                geometry != null && value != null -> {
                    SiteSceneView(geometry, value.cameras.mapNotNull { siteMarker(it, it.id == selected, it.id == placing) }, placing != null && !busy,
                        onSelect = { selected = it }, onPlace = { point ->
                            val id = placing ?: return@SiteSceneView
                            val index = value.cameras.indexOfFirst { it.id == id }
                            if (index < 0 || !point.valid || busy) return@SiteSceneView
                            val camera = value.cameras[index]
                            persist(value.copy(cameras = value.cameras.toMutableList().also { it[index] = camera.copy(position = point) }), "${camera.name} placed in ${value.name}", null)
                        }, modifier = Modifier.fillMaxSize())
                    value.cameras.firstOrNull { it.id == placing }?.let { camera ->
                        Surface(Modifier.align(Alignment.TopCenter).padding(12.dp), shape = CircleShape, color = MaterialTheme.colorScheme.surface.copy(alpha = .92f)) {
                            Row(Modifier.padding(horizontal = 14.dp, vertical = 8.dp), horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
                                Icon(Icons.Outlined.AddLocation, null, Modifier.size(18.dp)); Text("Tap the model to place ${camera.name}", style = MaterialTheme.typography.bodyMedium)
                            }
                        }
                    }
                }
                loadError != null -> EmptyMessage("Model unavailable", loadError, Icons.Outlined.ViewInAr)
                else -> Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) { LensLoading("Loading model…") }
            }
        }
        // The iOS camera panel, as a bottom section.
        Surface(Modifier.fillMaxWidth().heightIn(max = 320.dp), color = sitesBackground) {
            LazyColumn(contentPadding = PaddingValues(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                item {
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        Text("Place Cameras · ${value?.cameras?.count { it.position != null } ?: 0} of ${value?.cameras?.size ?: 0} placed", Modifier.weight(1f), style = MaterialTheme.typography.titleSmall, fontWeight = FontWeight.SemiBold)
                        if (placing != null) TextButton(onClick = { placing = null }, enabled = !busy) { Text("Cancel placement") }
                    }
                }
                items(value?.cameras.orEmpty(), key = { it.id }) { camera ->
                    var menu by remember(camera.id) { mutableStateOf(false) }
                    Surface(shape = RoundedCornerShape(16.dp), color = if (placing == camera.id) LensStyle.green.copy(alpha = .12f) else MaterialTheme.colorScheme.surface) {
                        Column(Modifier.fillMaxWidth().padding(12.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
                            Row(Modifier.fillMaxWidth().clickable(enabled = !busy) { placing = camera.id }, horizontalArrangement = Arrangement.spacedBy(10.dp), verticalAlignment = Alignment.CenterVertically) {
                                Icon(if (camera.position == null) Icons.Outlined.PhotoCamera else Icons.Outlined.CheckCircle, null, tint = LensStyle.accent)
                                Column(Modifier.weight(1f)) {
                                    Text(camera.name)
                                    SecondaryText(if (placing == camera.id) "Tap the model to place" else if (camera.position == null) "Not placed" else if (camera.manualAlignment != null) "Manually aligned" else "Placed manually")
                                }
                            }
                            Row(verticalAlignment = Alignment.CenterVertically) {
                                TextButton(onClick = { placing = null; saved?.let { s -> nav.push(camera.name) { SiteCameraLivePage(model, store, site, camera, s) } } }, enabled = !busy) { Icon(Icons.Outlined.PlayCircle, null, Modifier.size(16.dp)); Spacer(Modifier.width(4.dp)); Text("Live preview") }
                                TextButton(onClick = { placing = null; saved?.let { s -> nav.push("Align camera") { SiteCameraLivePage(model, store, site, camera, s, align = true) } } }, enabled = !busy) { Text("Align camera") }
                                Spacer(Modifier.weight(1f))
                                Box {
                                    IconButton(onClick = { menu = true }, enabled = !busy) { Icon(Icons.Outlined.MoreHoriz, "Options for ${camera.name}") }
                                    DropdownMenu(menu, onDismissRequest = { menu = false }) {
                                        DropdownMenuItem(text = { Text("Move camera") }, onClick = { menu = false; placing = camera.id })
                                        DropdownMenuItem(text = { Text("Remove from model", color = MaterialTheme.colorScheme.error) }, onClick = {
                                            menu = false; value?.let { v -> persist(v.copy(cameras = v.cameras.filter { it.id != camera.id }), "${camera.name} removed from ${v.name}", null) }
                                        })
                                    }
                                }
                            }
                        }
                    }
                }
                item {
                    LensButton("Add cameras", enabled = !busy && value != null, modifier = Modifier.fillMaxWidth()) {
                        nav.push("Add Camera") { SiteCameraPicker(model, site, value?.cameras.orEmpty()) { camera ->
                            val current = saved?.site ?: return@SiteCameraPicker
                            persist(current.copy(cameras = current.cameras + camera), "${camera.name} added to ${current.name}", camera.id)
                        } }
                    }
                }
                item { SecondaryText("Tap a camera, then tap the model to place it. Robot cameras move with the robot, so they aren’t listed. Manual placement does not calibrate the camera.") }
                if (busy) item { LoadingRow("Saving placement…") }
                error?.let { item { Text(it, color = MaterialTheme.colorScheme.error) } }
            }
        }
    }
}

/** A site device's fixed cameras with a stable identity, not already on this model (iOS MobileSiteCameraPicker). */
@Composable
private fun SiteCameraPicker(model: WendyModel, site: SiteRecord?, existing: List<SiteCamera>, add: (SiteCamera) -> Unit) {
    val nav = LocalNavigator.current
    val scope = rememberCoroutineScope()
    var gateway by remember { mutableStateOf(SiteGatewayIdentity()) }
    var chosen by remember { mutableStateOf<Device?>(null) }
    var session by remember { mutableLongStateOf(0L) }
    var busy by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    var search by remember { mutableStateOf("") }
    val ids = site?.assets?.filter { !it.robot }?.map { it.id }?.toSet().orEmpty()
    val devices = (model.nearbyDevices + model.cloudDevices).distinctBy { recordingDeviceID(it) }.filter { recordingDeviceID(it) in ids }
        .sortedWith(compareByDescending<Device> { siteStatus(model, it) in setOf("Online", "Connected", "Nearby") }.thenBy { it.title.lowercase() })
    val feeds = if (session != 0L && session == model.session) {
        val groups = model.cameras.groupBy { it.text("stable_id") }
        model.cameras.filter { camera ->
            val id = camera.text("stable_id")
            id.isNotEmpty() && groups[id]?.size == 1 && existing.none { gateway.matches(it.gatewayID) && it.sourceID == id } &&
                (search.isBlank() || camera.text("name").contains(search.trim(), true))
        }
    } else emptyList()
    PageColumn(Modifier.background(sitesBackground)) {
        item { OutlinedTextField(search, { search = it }, Modifier.fillMaxWidth(), placeholder = { Text("Search cameras") }, leadingIcon = { Icon(Icons.Outlined.Search, null) }, singleLine = true) }
        formSection("Site devices") {
            devices.forEach { device ->
                val online = siteStatus(model, device) in setOf("Online", "Connected", "Nearby")
                Row(Modifier.fillMaxWidth().clickable(enabled = !busy) {
                    busy = true; error = null; gateway = SiteGatewayIdentity(); session = 0; chosen = device
                    val account = model.account; val organization = model.organization
                    scope.launch {
                        try { val identity = model.siteGateway(device); if (account == model.account && organization == model.organization) { gateway = identity; session = model.session } }
                        catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { busy = false }
                    }
                }.padding(vertical = 8.dp), horizontalArrangement = Arrangement.spacedBy(10.dp), verticalAlignment = Alignment.CenterVertically) {
                    Box(Modifier.size(8.dp).background(if (online) LensStyle.green else LensStyle.offline, CircleShape))
                    Text(device.title, Modifier.weight(1f), color = if (online) MaterialTheme.colorScheme.onSurface else MaterialTheme.colorScheme.onSurfaceVariant)
                    if (chosen?.selector == device.selector) Icon(Icons.Outlined.Check, "Selected", tint = LensStyle.accent)
                }
            }
            if (devices.isEmpty()) SecondaryText("This site has no available fixed-camera devices.")
        }
        if (session == model.session && session != 0L && gateway.id.isNotEmpty()) formSection("Fixed cameras") {
            feeds.forEach { camera ->
                ActionRow(camera.text("name").ifEmpty { "Camera" }, Icons.Outlined.PhotoCamera) {
                    add(SiteCamera(gatewayID = gateway.id, sourceID = camera.text("stable_id"), name = camera.text("name").ifEmpty { "Camera" })); nav.pop()
                }
            }
            if (feeds.isEmpty()) SecondaryText("No additional cameras found")
        }
        if (busy) item { LoadingRow("Finding cameras…") }
        error?.let { item { Text(it, color = MaterialTheme.colorScheme.error) } }
    }
}

/**
 * Finds a saved camera on the site's devices by its gateway identity and stable source, then plays it.
 * With [align], it offers manual alignment; automatic alignment needs OpenCV localization that Android lacks.
 */
@Composable
private fun SiteCameraLivePage(model: WendyModel, store: SitesStore, site: SiteRecord?, camera: SiteCamera, saved: StoredSite, align: Boolean = false) {
    val nav = LocalNavigator.current
    var busy by remember { mutableStateOf(false) }
    var status by remember { mutableStateOf("Finding camera…") }
    var error by remember { mutableStateOf<String?>(null) }
    var session by remember { mutableLongStateOf(0L) }
    var attempt by remember { mutableIntStateOf(0) }
    val assigned = site?.assets?.filter { !it.robot }.orEmpty()
    LaunchedEffect(attempt) {
        busy = true; error = null; session = 0
        try {
            if (assigned.isEmpty()) { error = "No devices are assigned to this site. Add a device in Edit devices and robots."; return@LaunchedEffect }
            val ids = assigned.map { it.id }.toSet()
            var candidates = (model.nearbyDevices + model.cloudDevices + listOfNotNull(model.selected)).distinctBy { recordingDeviceID(it) }.filter { recordingDeviceID(it) in ids }
            if (candidates.isEmpty()) { status = "Loading the site’s devices…"; model.refresh(); candidates = (model.nearbyDevices + model.cloudDevices).distinctBy { recordingDeviceID(it) }.filter { recordingDeviceID(it) in ids } }
            val connected = model.selected?.id
            val ordered = candidates.sortedBy { d -> if (SiteGatewayIdentity.cloud(d.selector) == camera.gatewayID || "cloud-v2/" + recordingDeviceID(d) == camera.gatewayID) 0 else if (d.id == connected) 1 else 2 }
            val failures = mutableListOf<String>()
            for (device in ordered) {
                status = "Finding camera on ${device.title}…"
                try {
                    val identity = model.siteGateway(device)
                    if (!identity.matches(camera.gatewayID)) continue
                    val matches = model.cameras.filter { it.text("stable_id") == camera.sourceID }
                    if (matches.size != 1) { error = "The camera’s device is ${device.title}, but it is not reporting this camera uniquely. Check its camera connection and try again."; return@LaunchedEffect }
                    session = model.session; return@LaunchedEffect
                } catch (e: CancellationException) { throw e } catch (e: Exception) { failures += "${device.title}: ${e.message}" }
            }
            error = when {
                failures.isNotEmpty() -> failures.joinToString("\n\n")
                ordered.isEmpty() -> "This site has ${assigned.size} assigned device(s): ${assigned.joinToString { it.name }}. They are not available in the current device list. Pull to refresh Devices, or tap Try again."
                else -> "The saved camera could not be found on this site’s available devices. Its placement may refer to an old pairing."
            }
        } finally { busy = false }
    }
    val feed = if (session != 0L && session == model.session) model.cameras.filter { it.text("stable_id") == camera.sourceID }.singleOrNull() else null
    Column(Modifier.fillMaxSize().background(sitesBackground).padding(16.dp), verticalArrangement = Arrangement.spacedBy(14.dp)) {
        when {
            feed != null -> {
                CameraTile(model, feed)
                if (align) {
                    Text("Align camera", style = MaterialTheme.typography.titleLarge, fontWeight = FontWeight.Bold)
                    Surface(shape = RoundedCornerShape(14.dp), color = MaterialTheme.colorScheme.surface) {
                        Column(Modifier.fillMaxWidth().padding(14.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
                            Text(if (camera.manualAlignment != null) "Manually aligned" else if (camera.position != null) "Saved placement · needs alignment" else "Not placed", style = MaterialTheme.typography.titleSmall)
                            SecondaryText(if (saved.visualMap == null) "This model has no scan reference photos, so it can’t be aligned automatically." else "Automatic alignment from scan references isn’t available on Android yet. Adjust the camera manually.")
                        }
                    }
                    LensButton("Adjust manually", primary = true, modifier = Modifier.fillMaxWidth()) {
                        nav.push("Manual alignment") {
                            SiteManualAlignmentPage(model, store.models, camera, saved, session) { value ->
                                store.saveCameraModel(value, site?.id ?: throw SiteException("Reopen the camera from its site to save alignment."), "${camera.name} manually aligned")
                            }
                        }
                    }
                }
            }
            else -> Column(Modifier.fillMaxWidth().padding(24.dp), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(14.dp)) {
                Icon(Icons.Outlined.Videocam, null, Modifier.size(40.dp), tint = LensStyle.accent)
                Text(if (busy) status else "Camera unavailable", style = MaterialTheme.typography.titleMedium)
                if (busy) CircularProgressIndicator()
                else {
                    SecondaryText("Wendy checks the devices assigned to this site for the saved camera. Check that its device is online and still belongs to this site.")
                    LensButton("Try again", primary = true) { attempt++ }
                }
            }
        }
        error?.let { SecondaryText(it) }
    }
}

/**
 * Match the camera to walls and objects by hand: position, turn, tilt, roll and field of view. This saves a
 * manual visualization with the model, not a measured calibration (iOS MobileManualCameraAlignment).
 */
@Composable
internal fun SiteManualAlignmentPage(model: WendyModel, models: SiteModelStore, camera: SiteCamera, saved: StoredSite, session: Long, save: (Site) -> Unit) {
    val nav = LocalNavigator.current
    val page = LocalNavPage.current
    val scope = rememberCoroutineScope()
    val (_, geometry, loadError) = rememberSiteModel(models, saved.id)
    var x by remember { mutableDoubleStateOf(camera.position?.x ?: 0.0) }
    var y by remember { mutableDoubleStateOf(camera.position?.y ?: 1.5) }
    var z by remember { mutableDoubleStateOf(camera.position?.z ?: 0.0) }
    var yaw by remember { mutableDoubleStateOf(camera.manualAlignment?.yaw ?: 180.0) }
    var pitch by remember { mutableDoubleStateOf(camera.manualAlignment?.pitch ?: 0.0) }
    var roll by remember { mutableDoubleStateOf(camera.manualAlignment?.roll ?: 180.0) }
    var fov by remember { mutableDoubleStateOf(camera.manualAlignment?.horizontalFOV ?: 70.0) }
    var placing by remember { mutableStateOf(false) }
    var busy by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    val alignment = SiteManualAlignment(saved.site.modelRevision, yaw, pitch, roll, fov, camera.manualAlignment?.width ?: 1600, camera.manualAlignment?.height ?: 1200)
    val preview = camera.copy(position = SitePoint(x, y, z), manualAlignment = alignment)
    fun save() {
        placing = false; busy = true; error = null
        scope.launch {
            try {
                if (model.session != session) throw SiteException("The device connection changed. Reopen alignment before saving.")
                withContext(Dispatchers.IO) {
                    val value = models.load(saved.id).site
                    if (value.modelRevision != alignment.modelRevision) throw SiteException("The model changed. Reopen alignment.")
                    val index = value.cameras.indexOfFirst { it.id == camera.id }
                    if (index < 0) throw SiteException("The model changed. Reopen alignment.")
                    save(value.copy(cameras = value.cameras.toMutableList().also { it[index] = it[index].copy(position = SitePoint(x, y, z), manualAlignment = alignment) }))
                }
                nav.pop()
            } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { busy = false }
        }
    }
    LaunchedEffect(x, y, z, yaw, pitch, roll, fov, busy) { page?.actions = { TextButton(onClick = ::save, enabled = !busy) { Text("Save", fontWeight = FontWeight.SemiBold) } } }
    Column(Modifier.fillMaxSize().background(sitesBackground)) {
        Box(Modifier.fillMaxWidth().height(280.dp)) {
            if (geometry != null) SiteSceneView(geometry, listOfNotNull(siteMarker(preview, true, placing)), placing, onSelect = {}, onPlace = { p -> x = p.x; y = p.y; z = p.z; placing = false }, modifier = Modifier.fillMaxSize())
            else if (loadError != null) EmptyMessage("Model unavailable", loadError, Icons.Outlined.ViewInAr)
            Button(onClick = { placing = !placing }, Modifier.align(Alignment.TopEnd).padding(8.dp), colors = ButtonDefaults.buttonColors(containerColor = LensStyle.green)) { Text(if (placing) "Cancel move" else "Move in model") }
        }
        PageColumn {
            item { SecondaryText("Match the camera’s view cone to walls and objects. This saves a manual visualization, not a measured calibration.") }
            formSection("Position · metres") {
                Stepper("X", x) { x = it }; Stepper("Height", y) { y = it }; Stepper("Z", z) { z = it }
            }
            formSection("Rotation") {
                Adjustment("Turn", yaw, -180.0..180.0) { yaw = it }; Adjustment("Tilt", pitch, -90.0..90.0) { pitch = it }; Adjustment("Roll", roll, -180.0..180.0) { roll = it }
            }
            formSection("Field of view") { Adjustment("Width", fov, 20.0..140.0) { fov = it } }
            error?.let { item { Text(it, color = MaterialTheme.colorScheme.error) } }
        }
    }
}

@Composable
private fun Stepper(title: String, value: Double, set: (Double) -> Unit) {
    Row(Modifier.fillMaxWidth().padding(vertical = 2.dp), verticalAlignment = Alignment.CenterVertically) {
        Text("$title  ${"%.2f".format(value)}", Modifier.weight(1f))
        IconButton(onClick = { set(Math.round((value - 0.1) * 100) / 100.0) }) { Icon(Icons.Outlined.Remove, "Decrease $title") }
        IconButton(onClick = { set(Math.round((value + 0.1) * 100) / 100.0) }) { Icon(Icons.Outlined.Add, "Increase $title") }
    }
}

@Composable
private fun Adjustment(title: String, value: Double, range: ClosedFloatingPointRange<Double>, set: (Double) -> Unit) {
    Column(Modifier.fillMaxWidth()) {
        Text("$title  ${"%.0f".format(value)}°")
        Slider(value.toFloat(), { set(Math.round(it).toDouble()) }, valueRange = range.start.toFloat()..range.endInclusive.toFloat(), steps = 0,
            colors = SliderDefaults.colors(thumbColor = LensStyle.green, activeTrackColor = LensStyle.green))
    }
}

/** The default model's cameras, playing when their device is the connected one (iOS MobileSiteLiveCameras). */
@Composable
private fun SiteLiveCameras(model: WendyModel, store: SitesStore, site: SiteRecord, link: SiteModelLink) {
    val nav = LocalNavigator.current
    var saved by remember(link.id) { mutableStateOf<StoredSite?>(null) }
    var error by remember(link.id) { mutableStateOf<String?>(null) }
    var gateway by remember { mutableStateOf(SiteGatewayIdentity()) }
    LaunchedEffect(link.id, store.revision) {
        try { saved = withContext(Dispatchers.IO) { store.models.load(link.id) }; error = null } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message }
    }
    LaunchedEffect(model.session) {
        gateway = SiteGatewayIdentity()
        val device = model.selected ?: return@LaunchedEffect
        if (model.session == 0L) return@LaunchedEffect
        val session = model.session
        try {
            val info = model.backend.call("DeviceInfo", session).obj(); val enrollment = model.backend.call("DeviceEnrollment", session).obj()
            if (session == model.session) gateway = SiteGatewayIdentity.resolve(SiteGatewayIdentity.cloud(device.selector), enrollment.flag("Enrolled"), enrollment.text("OrganizationID"), enrollment.text("DeviceID"), info.text("serial_number"))
        } catch (e: CancellationException) { throw e } catch (_: Exception) { /* Other gateways stay tap-to-connect. */ }
    }
    val cameras = saved?.site?.cameras.orEmpty()
    LensSection("Live cameras") {
        if (cameras.isEmpty()) Text(error ?: "Add cameras to ${link.name} to see them here.", Modifier.padding(16.dp), color = MaterialTheme.colorScheme.onSurfaceVariant)
        else Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            cameras.chunked(2).forEach { row ->
                Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                    row.forEach { camera ->
                        val device = model.selected
                        val feed = if (device != null && site.assets.any { !it.robot && it.id == recordingDeviceID(device) } && gateway.matches(camera.gatewayID))
                            model.cameras.filter { it.text("stable_id") == camera.sourceID }.singleOrNull() else null
                        Column(Modifier.weight(1f).clickable { saved?.let { s -> nav.push(camera.name) { SiteCameraLivePage(model, store, site, camera, s) } } }, verticalArrangement = Arrangement.spacedBy(6.dp)) {
                            Box(Modifier.fillMaxWidth().height(100.dp).clip(RoundedCornerShape(16.dp)).background(Color.Black.copy(alpha = .85f)), contentAlignment = Alignment.Center) {
                                if (feed != null) CameraTile(model, feed, compact = true) else Icon(Icons.Outlined.PlayCircle, null, Modifier.size(36.dp), tint = Color.White)
                            }
                            Text(camera.name, style = MaterialTheme.typography.titleSmall, maxLines = 2)
                            Text("Watch live", style = MaterialTheme.typography.labelSmall, color = LensStyle.accent)
                        }
                    }
                    if (row.size == 1) Spacer(Modifier.weight(1f))
                }
            }
        }
    }
}
