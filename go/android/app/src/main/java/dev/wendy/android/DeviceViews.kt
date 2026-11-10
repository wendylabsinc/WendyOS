package dev.wendy.android

import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.text.format.DateUtils
import androidx.compose.foundation.ExperimentalFoundationApi
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.combinedClickable
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.material.icons.automirrored.outlined.*
import androidx.compose.material3.*
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import org.json.JSONObject

private val AppState.color: Color @Composable get() = when (this) {
    AppState.Running -> MaterialTheme.colorScheme.primary
    AppState.CrashLooping -> MaterialTheme.colorScheme.error
    else -> MaterialTheme.colorScheme.onSurfaceVariant
}

// MARK: Overview

/** The device landing page: identity, the first app with a webpage, live cameras, apps and shortcuts. */
@Composable
fun DeviceOverviewScreen(model: WendyModel, open: (String) -> Unit) {
    val session = model.session
    var info by remember(session) { mutableStateOf<JSONObject?>(null) }
    LaunchedEffect(session) { info = try { model.backend.call("DeviceInfo", session).obj() } catch (e: CancellationException) { throw e } catch (_: Exception) { null } }
    val running = model.visibleApps.filter { it.state == AppState.Running }
    PageColumn {
        item {
            Box(Modifier.fillMaxWidth().clip(RoundedCornerShape(28.dp)).background(Brush.linearGradient(listOf(Color(0xFF1C4838), Color(0xFF0B2923))))) {
                DeviceIllustration(Modifier.align(Alignment.CenterEnd).size(150.dp).offset(x = 28.dp, y = 8.dp))
                Column(Modifier.padding(24.dp).fillMaxWidth(.78f), verticalArrangement = Arrangement.spacedBy(12.dp)) {
                    StatusPill("Connected", dark = true)
                    Text(model.selected?.title ?: "Your device", style = MaterialTheme.typography.headlineLarge, color = Color(0xFFF3F8EA))
                    val board = info?.text("board_model") ?: ""
                    Text(board.ifEmpty { if (model.lite) "Wendy Lite" else "Wendy device" }, style = MaterialTheme.typography.bodySmall, color = WendyMint)
                }
            }
        }
        running.firstOrNull { it.hasWebpage }?.let { app ->
            item { Button(onClick = { model.openApp(app.name) }, Modifier.fillMaxWidth().heightIn(min = 52.dp)) { Icon(Icons.Outlined.NorthEast, null, Modifier.size(18.dp)); Spacer(Modifier.width(8.dp)); Text("Open ${app.name}") } }
        }
        if (model.cameras.isNotEmpty()) {
            item { SectionHeading("Live view", "All cameras") { open("Cameras") } }
            items(model.cameras.take(4), key = { "overview/${it.number("id")}" }) { camera -> CameraTile(model, camera, compact = true) { open("Cameras") } }
        }
        item {
            SectionHeading("On this device", "All apps") { open("Apps") }
            Spacer(Modifier.height(8.dp))
            when {
                model.appsError != null -> Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) { Icon(Icons.Outlined.Schedule, null, tint = MaterialTheme.colorScheme.onSurfaceVariant); Text("Updating app activity…", color = MaterialTheme.colorScheme.onSurfaceVariant) }
                model.visibleApps.isEmpty() -> Text("Your device is ready for its first app.", color = MaterialTheme.colorScheme.onSurfaceVariant)
                else -> model.visibleApps.take(5).forEach { app ->
                    Row(Modifier.fillMaxWidth().padding(vertical = 6.dp), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                        Icon(Icons.Outlined.Layers, null, tint = MaterialTheme.colorScheme.onSurfaceVariant)
                        Text(app.name, Modifier.weight(1f), style = MaterialTheme.typography.titleMedium, maxLines = 1, overflow = TextOverflow.Ellipsis)
                        Text(app.state.title, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                        if (app.state == AppState.Running && app.hasWebpage) OutlinedButton(onClick = { model.openApp(app.name) }) { Text("Open") }
                    }
                }
            }
        }
        item {
            Row(horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                ActionTile("Cameras", "Live video", Icons.Outlined.Videocam, Modifier.weight(1f)) { open("Cameras") }
                ActionTile("Explore site", "Digital twin", Icons.Outlined.ViewInAr, Modifier.weight(1f)) { open("Digital Twin") }
                ActionTile("Ask your device", "Chat", Icons.Outlined.Forum, Modifier.weight(1f)) { open("Chat") }
            }
        }
    }
}

@Composable
private fun SectionHeading(title: String, action: String, onAction: () -> Unit) {
    Row(Modifier.fillMaxWidth(), verticalAlignment = Alignment.CenterVertically) {
        Text(title, Modifier.weight(1f), style = MaterialTheme.typography.titleLarge)
        TextButton(onClick = onAction) { Text(action) }
    }
}

// MARK: Apps

/** Device resources and the app list (iOS ActivityView on iPhone). Samples every second while visible. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun AppsScreen(model: WendyModel) {
    val nav = LocalNavigator.current
    val session = model.session
    val scope = rememberCoroutineScope()
    var snapshot by remember(session) { mutableStateOf<ResourceSnapshot?>(null) }
    var error by remember(session) { mutableStateOf<String?>(null) }
    var refreshing by remember { mutableStateOf(false) }
    LaunchedEffect(session, model.foregroundGeneration) {
        while (true) {
            try { snapshot = ResourceSnapshot(model.backend.call("Resources", session).obj()); error = null } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message }
            delay(1000)
        }
    }
    LaunchedEffect(session) { model.refreshDetails() }
    PullToRefreshBox(refreshing, onRefresh = { scope.launch { refreshing = true; model.refreshDetails(); refreshing = false } }) {
        PageColumn {
            formSection("Device") {
                val value = snapshot
                if (value != null) {
                    ResourceMetrics(value)
                    if (value.hasTemperatureDetail) NavRow("Temperature & power", Icons.Outlined.Thermostat) { nav.push("Temperature & power") { TemperaturePage(model) } }
                } else if (error == null) LoadingRow("Sampling activity…")
                error?.let { FailureNote(it) }
                if (value != null && value.diskError.isNotEmpty()) SecondaryText("Storage is unavailable.")
            }
            formSection("Apps", "CPU percentages use the whole machine, across all cores.") {
                model.appsError?.let { FailureNote(it) }
                HiddenAppsFilter(model)
                model.visibleApps.forEach { app -> AppRow(model, app, snapshot) }
                if (model.apps.isEmpty() && model.appsError == null) SecondaryText("No apps installed")
            }
        }
    }
}

@Composable
private fun HiddenAppsFilter(model: WendyModel) {
    if (model.hiddenAppCount > 0) ToggleRow("Show hidden apps", model.showHiddenApps, subtitle = "${model.hiddenAppCount} hidden apps") { model.showHiddenApps = it }
}

@OptIn(ExperimentalFoundationApi::class)
@Composable
private fun AppRow(model: WendyModel, app: DeviceApp, snapshot: ResourceSnapshot?) {
    val nav = LocalNavigator.current
    val row = snapshot?.rows?.firstOrNull { !it.isService && it.app == app.name }
    val hidden = model.isAppHidden(app.name)
    Row(Modifier.fillMaxWidth().clickable { nav.push("App details", { AppDetailActions(model, app.name) }) { AppDetailPage(model, app.name) } }.padding(vertical = 8.dp),
        verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
        Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(5.dp)) {
            Text(app.name, style = MaterialTheme.typography.titleSmall, maxLines = 2, color = if (hidden) MaterialTheme.colorScheme.onSurfaceVariant else MaterialTheme.colorScheme.onSurface)
            Row(horizontalArrangement = Arrangement.spacedBy(6.dp), verticalAlignment = Alignment.CenterVertically) {
                Box(Modifier.size(6.dp).background(app.state.color, CircleShape))
                val detail = if (row != null && app.state == AppState.Running) " · CPU ${percent(row.cpu)} · ${formatBytes(row.memory)}" else if (app.version.isNotEmpty()) " · ${app.version}" else ""
                Text(app.state.title + detail, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
        }
        if (app.name in model.appActions) CircularProgressIndicator(Modifier.size(20.dp), strokeWidth = 2.dp)
        OverflowMenu("Actions for ${app.name}", listOf(
            MenuItem(if (hidden) "Show app" else "Hide app", if (hidden) Icons.Outlined.Visibility else Icons.Outlined.VisibilityOff) { model.setAppHidden(app.name, !hidden) },
            MenuItem("Remove app…", Icons.Outlined.Delete, destructive = true, enabled = app.name !in model.appActions) { model.requestAppDeletion(app.name) },
        ))
        Icon(Icons.Outlined.ChevronRight, null, Modifier.size(20.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

@Composable
private fun ResourceMetrics(value: ResourceSnapshot) {
    data class Metric(val title: String, val value: String, val detail: String, val fraction: Float, val icon: ImageVector)
    fun ratio(used: Long, total: Long) = if (total > 0) (used.toFloat() / total).coerceIn(0f, 1f) else 0f
    val metrics = buildList {
        add(Metric("CPU", percent(value.cpu), "${value.cpuCount} logical cores", ((value.cpu ?: 0.0) / 100).toFloat(), Icons.Outlined.Memory))
        add(Metric("Memory", if (value.hasHost) formatBytes(value.memoryUsed) else "—", if (value.hasHost) "of ${formatBytes(value.memoryTotal)}" else "Unavailable", ratio(value.memoryUsed, value.memoryTotal), Icons.Outlined.Storage))
        add(Metric("Storage", if (value.hasDisk) formatBytes(value.diskUsed) else "—", if (value.hasDisk) "of ${formatBytes(value.diskTotal)} · ${value.diskMount}" else "Unavailable", ratio(value.diskUsed, value.diskTotal), Icons.Outlined.SdStorage))
        value.gpus.forEach { add(Metric("GPU · ${it.name}", percent(it.utilization), if (it.memoryTotal > 0) "${formatBytes(it.memoryUsed)} / ${formatBytes(it.memoryTotal)}" else "Shared system memory", (it.utilization / 100).toFloat(), Icons.Outlined.DeveloperBoard)) }
        value.battery?.let { add(Metric("Battery", percent(it.percent), it.state, (it.percent / 100).toFloat(), Icons.Outlined.BatteryStd)) }
    }
    Column(Modifier.padding(vertical = 8.dp), verticalArrangement = Arrangement.spacedBy(10.dp)) {
        metrics.chunked(2).forEach { pair ->
            Row(horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                pair.forEach { MetricTile(it.title, it.value, it.detail, it.fraction, Modifier.weight(1f), it.icon) }
                if (pair.size == 1) Spacer(Modifier.weight(1f))
            }
        }
    }
}

@Composable
private fun TemperaturePage(model: WendyModel) {
    val session = model.session
    var snapshot by remember { mutableStateOf<ResourceSnapshot?>(null) }
    var error by remember { mutableStateOf<String?>(null) }
    LaunchedEffect(session) {
        while (true) {
            try { snapshot = ResourceSnapshot(model.backend.call("Resources", session).obj()); error = null } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message }
            delay(2000)
        }
    }
    PageColumn {
        error?.let { item { FormSection { FailureNote(it) } } }
        val value = snapshot ?: run { item { LoadingRow("Sampling activity…") }; return@PageColumn }
        formSection("Temperature") { value.thermals.forEach { (name, celsius) -> LabeledRow(name.ifEmpty { "Sensor" }, "%.0f°C".format(celsius)) } }
        value.gpus.forEach { gpu ->
            formSection(gpu.name) {
                gpu.temperature?.let { LabeledRow("Temperature", "%.0f°C".format(it)) }
                gpu.power?.let { LabeledRow("Power", "%.1f W".format(it)) }
            }
        }
    }
}

@Composable
private fun AppDetailActions(model: WendyModel, appName: String) {
    val nav = LocalNavigator.current
    OverflowMenu("App actions", listOf(
        MenuItem("Logs", Icons.AutoMirrored.Outlined.Article) { model.logApp = appName; nav.push("Logs") { DeviceLogsPage(model) } },
        MenuItem("Remove app…", Icons.Outlined.Delete, destructive = true, enabled = appName !in model.appActions) { model.requestAppDeletion(appName) },
    ))
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun AppDetailPage(model: WendyModel, appName: String) {
    val session = model.session
    val scope = rememberCoroutineScope()
    var snapshot by remember { mutableStateOf<ResourceSnapshot?>(null) }
    var ports by remember { mutableStateOf(emptyList<JSONObject>()) }
    var error by remember { mutableStateOf<String?>(null) }
    var portError by remember { mutableStateOf<String?>(null) }
    var loadingPorts by remember { mutableStateOf(true) }
    var refreshing by remember { mutableStateOf(false) }
    suspend fun refresh() {
        try { snapshot = ResourceSnapshot(model.backend.call("Resources", session).obj()); error = null } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message }
        try { ports = model.backend.call("Ports", appName, session).items(); portError = null } catch (e: CancellationException) { throw e } catch (e: Exception) { portError = e.message }
        loadingPorts = false
    }
    LaunchedEffect(session, appName) { while (true) { refresh(); delay(2000) } }
    val app = model.apps.firstOrNull { it.name == appName }
    val resource = snapshot?.rows?.firstOrNull { !it.isService && it.app == appName }
    PullToRefreshBox(refreshing, onRefresh = { scope.launch { refreshing = true; model.refreshDetails(); refresh(); refreshing = false } }) {
        PageColumn {
            formSection {
                SelectionContainer { Text(appName, Modifier.padding(vertical = 8.dp), style = MaterialTheme.typography.titleMedium) }
                if (app != null) {
                    LabeledRow("Status", app.state.title, valueColor = app.state.color)
                    LabeledRow("Version", app.version)
                    SecondaryText(app.terminationReason)
                }
            }
            formSection("Usage") {
                error?.let { FailureNote(it) }
                LabeledRow("CPU", percent(resource?.cpu))
                LabeledRow("Memory", resource?.let { formatBytes(it.memory) } ?: "Unavailable")
                LabeledRow("App GPU", "Not reported")
            }
            snapshot?.gpus?.takeIf { it.isNotEmpty() }?.let { gpus ->
                formSection("Device GPU") {
                    gpus.forEach { gpu ->
                        LabeledRow(gpu.name, percent(gpu.utilization))
                        if (gpu.memoryTotal > 0) LabeledRow("GPU memory", "${formatBytes(gpu.memoryUsed)} / ${formatBytes(gpu.memoryTotal)}") else SecondaryText("Shared system memory")
                    }
                }
            }
            formSection("Ports") {
                when {
                    portError != null -> FailureNote(portError!!)
                    loadingPorts && ports.isEmpty() -> LoadingRow("Loading ports…")
                    ports.isEmpty() -> SecondaryText("No listening ports")
                }
                ports.forEach { port ->
                    val address = port.text("address").let { if (':' in it) "[$it]" else it }
                    SelectionContainer { Text("${port.text("protocol").uppercase()}  $address:${port.number("port")}", Modifier.padding(vertical = 6.dp), fontFamily = FontFamily.Monospace, style = MaterialTheme.typography.bodyMedium) }
                }
            }
            if (app != null) formSection {
                if (app.hasWebpage) ActionRow("Open app", Icons.Outlined.NorthEast, enabled = app.state == AppState.Running) { model.openApp(appName) }
                if (appName in model.appActions) LoadingRow("Updating app…")
                else {
                    ActionRow("Start", Icons.Outlined.PlayArrow, enabled = app.state != AppState.Running) { model.setAppRunning(appName, true) }
                    ActionRow("Stop", Icons.Outlined.Stop, enabled = app.state != AppState.Stopped) { model.setAppRunning(appName, false) }
                }
            }
            val services = snapshot?.rows?.filter { it.isService && it.app == appName }.orEmpty()
            if (services.isNotEmpty()) formSection("Services") {
                services.forEach { service ->
                    Column(Modifier.padding(vertical = 6.dp)) {
                        Text(service.name, style = MaterialTheme.typography.bodyLarge)
                        SecondaryText("CPU ${percent(service.cpu)} · ${formatBytes(service.memory)}")
                    }
                }
            }
        }
    }
}

// MARK: Logs

/** Device or app logs with an app picker, filters and follow mode (iOS LogsView on iPhone). */
@Composable
fun DeviceLogsPage(model: WendyModel) {
    val context = LocalContext.current
    var search by remember { mutableStateOf("") }
    var problemsOnly by remember { mutableStateOf(false) }
    var follow by remember { mutableStateOf(true) }
    var inspected by remember { mutableStateOf<LogEntry?>(null) }
    var menu by remember { mutableStateOf(false) }
    LaunchedEffect(model.session, model.logApp, model.foregroundGeneration) { model.observeLogs() }
    val visible = model.logs.filter { (!problemsOnly || it.level.ordinal >= LogLevel.Warning.ordinal) && (search.isEmpty() || it.body.contains(search, true) || it.source.contains(search, true)) }
    val list = rememberLazyListState()
    LaunchedEffect(visible.lastOrNull()?.id, follow) { if (follow && visible.isNotEmpty()) list.scrollToItem(visible.lastIndex) }
    fun copy(text: String) { (context.getSystemService(Context.CLIPBOARD_SERVICE) as ClipboardManager).setPrimaryClip(ClipData.newPlainText("Wendy logs", text)) }
    Column(Modifier.fillMaxSize()) {
        Column(Modifier.padding(horizontal = 16.dp, vertical = 8.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                Box(Modifier.weight(1f)) {
                    TextButton(onClick = { menu = true }) { Text(model.logApp.ifEmpty { "All apps" }, maxLines = 1, overflow = TextOverflow.Ellipsis); Icon(Icons.Outlined.ExpandMore, null, Modifier.size(16.dp)) }
                    DropdownMenu(menu, onDismissRequest = { menu = false }) {
                        DropdownMenuItem(text = { Text("All apps") }, onClick = { menu = false; model.logApp = "" }, trailingIcon = if (model.logApp.isEmpty()) ({ Icon(Icons.Outlined.Check, null) }) else null)
                        model.apps.forEach { app -> DropdownMenuItem(text = { Text(app.name) }, onClick = { menu = false; model.logApp = app.name }, trailingIcon = if (model.logApp == app.name) ({ Icon(Icons.Outlined.Check, null) }) else null) }
                        HorizontalDivider()
                        DropdownMenuItem(text = { Text("Copy visible logs") }, leadingIcon = { Icon(Icons.Outlined.ContentCopy, null) }, onClick = { menu = false; copy(visible.joinToString("\n") { it.copyText }) })
                        DropdownMenuItem(text = { Text("Clear logs") }, leadingIcon = { Icon(Icons.Outlined.Delete, null) }, onClick = { menu = false; model.logs = emptyList() })
                        DropdownMenuItem(text = { Text("${visible.size} messages · Latest 2,000 records", style = MaterialTheme.typography.bodySmall) }, enabled = false, onClick = {})
                    }
                }
                FilterChip(follow, { follow = !follow }, label = { Text("Follow") }, leadingIcon = { Icon(Icons.Outlined.VerticalAlignBottom, null, Modifier.size(16.dp)) })
            }
            Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                OutlinedTextField(search, { search = it }, Modifier.weight(1f), singleLine = true, placeholder = { Text("Filter messages") }, leadingIcon = { Icon(Icons.Outlined.Search, null) },
                    trailingIcon = if (search.isNotEmpty()) ({ IconButton(onClick = { search = "" }) { Icon(Icons.Outlined.Close, "Clear filter") } }) else null,
                    keyboardOptions = KeyboardOptions(autoCorrectEnabled = false))
                FilterChip(problemsOnly, { problemsOnly = !problemsOnly }, label = { Text("Warnings") }, leadingIcon = { Icon(Icons.Outlined.FilterList, null, Modifier.size(16.dp)) })
            }
            model.logError?.let { if (ConnectionRecovery.canRetry(it)) LoadingRow("Reconnecting logs…") else FailureNote(it) }
        }
        HorizontalDivider(color = MaterialTheme.colorScheme.outlineVariant)
        if (visible.isEmpty() && model.logError == null) EmptyMessage(if (search.isEmpty() && !problemsOnly) "Waiting for logs" else "No matching messages", "Messages from the selected app appear here.", Icons.AutoMirrored.Outlined.Article)
        LazyColumn(Modifier.weight(1f), state = list) {
            items(visible, key = { it.id }) { entry -> LogRow(entry, model.logApp.isEmpty(), { inspected = entry }) { copy(entry.copyText) } }
        }
    }
    inspected?.let { entry ->
        AlertDialog(onDismissRequest = { inspected = null }, title = { Text("Log message") }, text = {
            Column(verticalArrangement = Arrangement.spacedBy(6.dp)) {
                LabeledRow("Time", entry.time); LabeledRow("Level", entry.level.title); LabeledRow("Source", entry.source)
                SelectionContainer { Text(entry.message, Modifier.heightIn(max = 320.dp).verticalScroll(rememberScrollState()), fontFamily = FontFamily.Monospace, style = MaterialTheme.typography.bodySmall) }
            }
        }, confirmButton = { TextButton(onClick = { inspected = null }) { Text("Close") } }, dismissButton = { TextButton(onClick = { copy(entry.copyText) }) { Text("Copy") } })
    }
}

@OptIn(ExperimentalFoundationApi::class)
@Composable
private fun LogRow(entry: LogEntry, showSource: Boolean, inspect: () -> Unit, copy: () -> Unit) {
    val color = when (entry.level) { LogLevel.Warning -> Color(0xFFE0912B); LogLevel.Error, LogLevel.Fatal -> MaterialTheme.colorScheme.error; LogLevel.Info -> MaterialTheme.colorScheme.primary; else -> MaterialTheme.colorScheme.onSurfaceVariant }
    val icon = when (entry.level) { LogLevel.Warning -> Icons.Outlined.WarningAmber; LogLevel.Error, LogLevel.Fatal -> Icons.Outlined.Cancel; LogLevel.Info -> Icons.Outlined.Info; else -> Icons.Outlined.Circle }
    Column(Modifier.fillMaxWidth().combinedClickable(onClick = inspect, onLongClick = copy)
        .background(if (entry.level.ordinal >= LogLevel.Warning.ordinal) color.copy(alpha = .06f) else Color.Transparent).padding(horizontal = 16.dp, vertical = 8.dp),
        verticalArrangement = Arrangement.spacedBy(4.dp)) {
        Row(horizontalArrangement = Arrangement.spacedBy(6.dp), verticalAlignment = Alignment.CenterVertically) {
            Icon(icon, entry.level.title, Modifier.size(12.dp), tint = color)
            Text(entry.time, style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            if (showSource && entry.source.isNotEmpty()) Text("· ${entry.source}", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis)
        }
        Text(entry.message, maxLines = 6, overflow = TextOverflow.Ellipsis, fontFamily = FontFamily.Monospace, style = MaterialTheme.typography.bodySmall,
            color = if (entry.level.ordinal >= LogLevel.Error.ordinal) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurface)
    }
    HorizontalDivider(Modifier.padding(start = 16.dp), color = MaterialTheme.colorScheme.outlineVariant)
}

// MARK: Connection

/** Shown while connecting or after a failure; retryable failures reconnect on their own (iOS DeviceConnectionView). */
@Composable
fun DeviceConnectionScreen(model: WendyModel) {
    val device = model.selected ?: return
    val error = model.detailError
    var details by remember { mutableStateOf(false) }
    Column(Modifier.fillMaxSize().padding(28.dp), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(18.dp, Alignment.CenterVertically)) {
        DeviceIllustration(Modifier.size(180.dp), MaterialTheme.colorScheme.primary)
        Text(device.title, style = MaterialTheme.typography.headlineSmall, textAlign = TextAlign.Center)
        if (error != null && !ConnectionRecovery.canRetry(error)) {
            val failure = PresentedFailure(error)
            failure.title?.let { Text(it, style = MaterialTheme.typography.titleMedium) }
            Text(failure.message, color = MaterialTheme.colorScheme.onSurfaceVariant, textAlign = TextAlign.Center)
            if (model.canForgetIdentity) ConfirmingButton("Forget saved identity and reconnect", "Only forget the saved identity if you have verified that ${device.title} was reinstalled or replaced. A different device may be using its address.") { model.connect(device, true) }
            else OutlinedButton(onClick = { model.retryConnection() }) { Text("Check connection") }
        } else {
            CircularProgressIndicator()
            Text("Connecting…", style = MaterialTheme.typography.titleMedium)
            Text("We’ll reconnect as soon as your device is available.", color = MaterialTheme.colorScheme.onSurfaceVariant, textAlign = TextAlign.Center)
        }
        if (error != null) TextButton(onClick = { details = true }) { Text("Connection details", style = MaterialTheme.typography.bodySmall) }
    }
    if (details && error != null) DetailsDialog("Connection details", PresentedFailure(error).details) { details = false }
}

@Composable
private fun ConfirmingButton(title: String, warning: String, action: () -> Unit) {
    var confirming by remember { mutableStateOf(false) }
    Button(onClick = { confirming = true }) { Text(title) }
    if (confirming) ConfirmAction("Device identity changed", warning, "Forget identity and reconnect", onDismiss = { confirming = false }) { confirming = false; action() }
}

// MARK: Activity tab

@Composable
fun ActivityScreen(model: WendyModel, select: (Device) -> Unit) {
    PageColumn {
        item { PageTitle("Activity", "Connections and app changes observed by Wendy.") }
        if (model.experienceEvents.isEmpty()) item { EmptyMessage("You’re up to date", "Activity will appear as you use your devices.", Icons.Outlined.CheckCircle) }
        items(model.experienceEvents, key = { it.id }) { event ->
            Row(Modifier.fillMaxWidth().clickable {
                (model.devices + listOfNotNull(model.selected)).firstOrNull { it.id == event.deviceID }?.let(select)
            }.padding(vertical = 10.dp), horizontalArrangement = Arrangement.spacedBy(14.dp), verticalAlignment = Alignment.Top) {
                IconTile(when (event.kind) { "connected" -> Icons.Outlined.CheckCircle; "app" -> Icons.Outlined.Layers; else -> Icons.Outlined.Bolt })
                Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                    Text(event.title, style = MaterialTheme.typography.titleMedium)
                    Text(event.deviceName, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
                }
                Text(DateUtils.getRelativeTimeSpanString(event.time, System.currentTimeMillis(), DateUtils.MINUTE_IN_MILLIS).toString(), style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
        }
    }
}
