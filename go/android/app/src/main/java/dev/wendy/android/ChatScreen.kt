package dev.wendy.android

import android.content.Intent
import android.net.Uri
import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.material.icons.automirrored.outlined.*
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.*
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.style.TextDecoration
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.NonCancellable
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import org.json.JSONArray
import org.json.JSONObject
import java.util.concurrent.atomic.AtomicLong

data class ChatModelOption(val id: String, val name: String, val port: Long = 11434, val unavailableReason: String = "") {
    val displayName get() = readableName(name.ifEmpty { id })
    companion object {
        /** Turns repository and Ollama tags into a short model name (iOS ChatModelOption.readableName). */
        fun readableName(identifier: String): String {
            val parts = identifier.split(":")
            var value = parts[0].substringAfterLast('/')
            if ('/' !in identifier && parts.size > 1 && Regex("^\\d+(?:\\.\\d+)?[bBmM]$").matches(parts[1])) value += "-" + parts[1].uppercase()
            value = value.replace(Regex("(?i)\\.gguf$|[-_]GGUF$"), "")
            value = value.replace(Regex("(?i)[-_.](?:Q[2-8](?:_[A-Z0-9]+)*|IQ[1-4](?:_[A-Z0-9]+)*|BF16|F16|F32)$"), "")
            value = value.replace("-", " ").replace("_", " ")
            for (family in listOf("Qwen", "Llama", "Gemma", "Phi", "Nemotron")) value = value.replace(Regex("(?i)\\b$family(?=[0-9]|\\b)"), "$family ")
            value = value.replace(Regex("(?i)[\\s·]+(\\d+(?:\\.\\d+)?B)\\b"), " · $1")
            value = value.replace(Regex("\\s+"), " ").trim()
            return value.ifEmpty { identifier }
        }
    }
}

data class ChatCard(val kind: String, val title: String, val camera: Long)
class ChatMessage(val id: Long, val user: Boolean, text: String = "") {
    var text by mutableStateOf(text)
    var trace by mutableStateOf("")
    var interrupted by mutableStateOf(false)
    var cards by mutableStateOf(emptyList<ChatCard>())
}
data class ChatApproval(val id: Long, val action: String, val app: String) {
    val isNotificationPermission get() = action == "enable_campaign_notifications"
    val isCampaign get() = action == "create_person_detection_campaign"
    val title get() = if (isNotificationPermission) "Allow campaign notifications?" else if (isCampaign) "Create person-detection campaign?" else "Allow device action?"
    val confirmLabel get() = if (isNotificationPermission) "Allow notifications" else if (isCampaign) "Create campaign" else "Allow"
    val arguments: JSONObject get() = runCatching { JSONObject(app) }.getOrDefault(JSONObject())
    val campaignName get() = arguments.optString(if (isCampaign) "name" else "campaign").ifEmpty { "this campaign" }
    val cameraName get() = arguments.optString("camera").ifEmpty { "Unknown camera" }.let { if (it == "*") "All current and future cameras" else it }
    val clipSummary get() = "${arguments.optInt("before_seconds")}s before · ${arguments.optInt("after_seconds")}s after detection"
}

/** One device's conversation. It outlives the Chat pane, as iOS keeps DeviceChatModel per session. */
class DeviceChat(private val model: WendyModel, val session: Long, private val scope: CoroutineScope) {
    private val ids = AtomicLong(1)
    var port by mutableStateOf("11434")
    var models by mutableStateOf(emptyList<ChatModelOption>())
    var selected by mutableStateOf("")
    val messages = mutableStateListOf<ChatMessage>()
    var prompt by mutableStateOf("")
    var loading by mutableStateOf(false); private set
    var streaming by mutableStateOf(false); private set
    var error by mutableStateOf<String?>(null)
    var activity by mutableStateOf(""); private set
    var approval by mutableStateOf<ChatApproval?>(null); private set
    var resolvingApproval by mutableStateOf(false); private set
    private var activeResponse: Long? = null
    private var turn = 0L
    private var loadedPort: Long? = null
    private var job: Job? = null
    val portNumber get() = port.toLongOrNull()?.takeIf { it in 1..65535 }
    val unavailableReason get() = models.firstOrNull { it.id == selected }?.unavailableReason ?: ""
    val modelName get() = models.firstOrNull { it.id == selected }?.displayName ?: ChatModelOption.readableName(selected)
    val canSend get() = unavailableReason.isEmpty() && !streaming && !loading && selected.isNotEmpty() && portNumber == loadedPort && prompt.isNotBlank()
    fun choose(id: String) { selected = id; models.firstOrNull { it.id == id }?.let { port = it.port.toString(); loadedPort = it.port } }
    suspend fun discoverIfNeeded() { if (loadedPort == null && !loading) discover() }
    suspend fun discover() {
        val requested = portNumber ?: return
        if (streaming || loading) return
        val previous = selected
        loading = true; error = null
        try {
            val result = model.backend.call("ChatModels", session, requested).items().map { ChatModelOption(it.text("ID"), it.text("Name"), it.number("Port").takeIf { p -> p > 0 } ?: requested, it.text("UnavailableReason")) }
            if (portNumber != requested) return
            models = result
            choose(if (result.any { it.id == previous }) previous else result.firstOrNull()?.id ?: "")
            loadedPort = if (result.isEmpty()) null else requested
        } catch (e: CancellationException) { throw e } catch (e: Exception) { if (portNumber == requested) error = e.message } finally { loading = false }
    }
    fun send() {
        if (!canSend) return
        val text = prompt.trim()
        prompt = ""; error = null; streaming = true
        messages.add(ChatMessage(ids.getAndIncrement(), true, text))
        val response = ChatMessage(ids.getAndIncrement(), false)
        messages.add(response)
        activeResponse = response.id
        val request = model.nextID()
        turn = request
        // Each request owns its history and cannot switch models mid-generation.
        val history = JSONArray(messages.filter { it.user || it.text.isNotEmpty() }.map { json("Role" to if (it.user) "user" else "assistant", "Content" to it.text) })
        val selectedModel = selected
        val requestedPort = portNumber ?: 11434
        job = scope.launch {
            try {
                model.backend.call("StartChat", session, request, requestedPort, selectedModel, history)
                while (turn == request) {
                    val result = model.backend.call("PollChat", session, request).obj()
                    if (turn != request) break
                    response.text = result.text("Text"); response.trace = result.text("Trace")
                    response.cards = result.list("Cards").map { ChatCard(it.text("Kind"), it.text("Title"), it.number("Camera")) }
                    activity = result.text("Activity")
                    approval = result.number("ApprovalID").takeIf { it != 0L }?.let { ChatApproval(it, result.text("ApprovalAction"), result.text("ApprovalApp")) }
                    result.text("Error").takeIf { it.isNotEmpty() }?.let { error = it }
                    if (result.flag("Done")) break
                    delay(100)
                }
            } catch (e: CancellationException) { throw e } catch (e: Exception) { if (turn == request) error = e.message }
            finally {
                if (turn == request) { streaming = false; activeResponse = null; approval = null; activity = "" }
                withContext(NonCancellable) { try { model.backend.call("StopChat", session, request) } catch (_: Exception) {} }
            }
        }
    }
    fun isWaiting(message: ChatMessage) = streaming && activeResponse == message.id && message.text.isEmpty() && message.cards.isEmpty() && approval == null
    fun resolve(pending: ChatApproval, allow: Boolean) {
        if (approval?.id != pending.id || resolvingApproval) return
        resolvingApproval = true
        val request = turn
        scope.launch {
            try { model.backend.call("ResolveChatApproval", session, request, pending.id, allow); if (turn == request && approval?.id == pending.id) approval = null }
            catch (e: CancellationException) { throw e } catch (e: Exception) { if (turn == request) error = e.message } finally { resolvingApproval = false }
        }
    }
    fun stop() {
        turn = 0
        messages.firstOrNull { it.id == activeResponse }?.interrupted = true
        streaming = false; activeResponse = null; approval = null; activity = ""
        job?.cancel()
    }
    fun clear() { stop(); messages.clear(); error = null }
    fun useModel(name: String, newPort: Long) {
        clear(); port = newPort.toString(); loadedPort = newPort
        models = models.filter { it.id != name } + ChatModelOption(name, name, newPort)
        selected = name
    }
}

@Composable
fun ChatScreen(model: WendyModel) {
    val nav = LocalNavigator.current
    val chat = model.deviceChat ?: return
    val deviceName = model.selected?.title ?: "this device"
    val scope = rememberCoroutineScope()
    var menu by remember { mutableStateOf(false) }
    var details by remember { mutableStateOf(false) }
    val list = rememberLazyListState()
    LaunchedEffect(chat) { chat.discoverIfNeeded() }
    LaunchedEffect(chat.messages.size, chat.messages.lastOrNull()?.text, chat.approval?.id) { if (chat.messages.isNotEmpty()) list.animateScrollToItem(list.layoutInfo.totalItemsCount.coerceAtLeast(1) - 1) }
    fun openConnection() = nav.push("Connection") { ChatConnectionPage(chat, deviceName) }
    fun openLibrary() = nav.push("Models") { ModelLibraryScreen(model, chat.port) { name, port -> chat.useModel(name, port) } }
    Column(Modifier.fillMaxSize().imePadding()) {
        Row(Modifier.padding(horizontal = 16.dp, vertical = 4.dp), verticalAlignment = Alignment.CenterVertically) {
            Box(Modifier.weight(1f)) {
                TextButton(onClick = { menu = true }, enabled = !chat.streaming && !chat.loading) {
                    Text(if (chat.selected.isEmpty()) "Choose a model" else chat.modelName, maxLines = 1, style = MaterialTheme.typography.titleSmall)
                    Icon(Icons.Outlined.ExpandMore, null, Modifier.size(16.dp))
                }
                DropdownMenu(menu, onDismissRequest = { menu = false }) {
                    chat.models.forEach { option -> DropdownMenuItem(text = { Text(option.displayName) }, onClick = { menu = false; chat.choose(option.id) },
                        trailingIcon = if (option.id == chat.selected) ({ Icon(Icons.Outlined.Check, null) }) else null) }
                    if (chat.models.isNotEmpty()) HorizontalDivider()
                    DropdownMenuItem(text = { Text("Browse models") }, leadingIcon = { Icon(Icons.Outlined.Layers, null) }, onClick = { menu = false; openLibrary() })
                    DropdownMenuItem(text = { Text("Connection settings") }, leadingIcon = { Icon(Icons.Outlined.Lan, null) }, onClick = { menu = false; openConnection() })
                    DropdownMenuItem(text = { Text("Refresh models") }, leadingIcon = { Icon(Icons.Outlined.Refresh, null) }, onClick = { menu = false; scope.launch { chat.discover() } })
                }
            }
            if (chat.loading) CircularProgressIndicator(Modifier.size(18.dp), strokeWidth = 2.dp)
            IconButton(onClick = chat::clear) { Icon(Icons.Outlined.EditNote, "New chat") }
        }
        LazyColumn(Modifier.weight(1f), state = list, contentPadding = PaddingValues(horizontal = 12.dp, vertical = 16.dp), verticalArrangement = Arrangement.spacedBy(18.dp)) {
            if (chat.messages.isEmpty()) item { EmptyMessage("Ask about your device", "Inspect apps, diagnose issues, or manage campaigns. Changes that need approval will ask first.", Icons.Outlined.Forum) }
            items(chat.messages, key = { it.id }) { message -> ChatMessageView(model, chat, message) }
            chat.approval?.let { pending -> item(key = "approval") { ChatApprovalCard(pending, deviceName, !chat.resolvingApproval) { chat.resolve(pending, it) } } }
        }
        if (chat.error != null || chat.unavailableReason.isNotEmpty()) ChatConnectionNotice(chat, deviceName, { scope.launch { chat.discover() } }, ::openConnection) { details = true }
        if (chat.approval == null && chat.streaming && chat.activity.isNotEmpty()) Text(chat.activity, Modifier.padding(horizontal = 28.dp), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        Surface(Modifier.padding(horizontal = 16.dp, vertical = 10.dp), shape = RoundedCornerShape(24.dp), color = MaterialTheme.colorScheme.surface, border = BorderStroke(1.dp, MaterialTheme.colorScheme.outlineVariant)) {
            Row(Modifier.padding(6.dp), verticalAlignment = Alignment.Bottom, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                TextField(chat.prompt, { chat.prompt = it }, Modifier.weight(1f), placeholder = { Text("Message") }, maxLines = 6,
                    colors = TextFieldDefaults.colors(focusedContainerColor = Color.Transparent, unfocusedContainerColor = Color.Transparent, focusedIndicatorColor = Color.Transparent, unfocusedIndicatorColor = Color.Transparent))
                val active = chat.streaming || chat.canSend
                IconButton(onClick = { if (chat.streaming) chat.stop() else chat.send() }, enabled = active,
                    modifier = Modifier.size(44.dp).background(if (active) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.surfaceVariant, CircleShape)) {
                    Icon(if (chat.streaming) Icons.Outlined.Stop else Icons.Outlined.ArrowUpward, if (chat.streaming) "Stop response" else "Send message",
                        tint = if (active) MaterialTheme.colorScheme.onPrimary else MaterialTheme.colorScheme.onSurfaceVariant)
                }
            }
        }
    }
    if (details) DetailsDialog("Connection details", PresentedFailure(chat.error ?: chat.unavailableReason).details) { details = false }
}

@Composable
private fun ChatMessageView(model: WendyModel, chat: DeviceChat, message: ChatMessage) {
    var trace by remember { mutableStateOf(false) }
    Column(Modifier.fillMaxWidth().background(if (message.user) MaterialTheme.colorScheme.primary.copy(alpha = .06f) else Color.Transparent, RoundedCornerShape(14.dp)).padding(16.dp),
        verticalArrangement = Arrangement.spacedBy(8.dp)) {
        Row(horizontalArrangement = Arrangement.spacedBy(6.dp), verticalAlignment = Alignment.CenterVertically) {
            Icon(if (message.user) Icons.Outlined.AccountCircle else Icons.Outlined.AutoAwesome, null, Modifier.size(14.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
            Text(if (message.user) "You" else chat.modelName, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        if (message.trace.isNotEmpty()) {
            Text(if (trace) "Hide device activity" else "Device activity", Modifier.clickable { trace = !trace }, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.primary)
            if (trace) SelectionContainer { Text(message.trace, fontFamily = FontFamily.Monospace, style = MaterialTheme.typography.bodySmall) }
        }
        when {
            chat.isWaiting(message) -> CircularProgressIndicator(Modifier.size(18.dp), strokeWidth = 2.dp)
            message.user -> SelectionContainer { Text(message.text, style = MaterialTheme.typography.bodyLarge) }
            else -> ChatMarkdown(message.text)
        }
        if (message.interrupted) SecondaryText("Response stopped")
        ChatDeviceCards(model, message.cards)
    }
}

@Composable
private fun ChatConnectionNotice(chat: DeviceChat, deviceName: String, retry: () -> Unit, settings: () -> Unit, details: () -> Unit) {
    val unavailable = chat.unavailableReason.isNotEmpty()
    Surface(Modifier.fillMaxWidth().padding(horizontal = 16.dp, vertical = 6.dp), shape = RoundedCornerShape(12.dp), color = MaterialTheme.colorScheme.errorContainer.copy(alpha = .5f)) {
        Column(Modifier.padding(14.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
            Text(if (unavailable) "Model server unavailable" else if (chat.messages.isEmpty()) "Could not load models" else "Chat request failed", style = MaterialTheme.typography.titleSmall)
            Text(if (unavailable) "${chat.modelName} is downloaded on $deviceName, but Wendy could not reach its server. Check that the model app is running, then retry."
                else "The request could not finish. Check the connection to $deviceName and try again. Your conversation is still here.", style = MaterialTheme.typography.bodySmall)
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                OutlinedButton(onClick = retry, enabled = !chat.loading && !chat.streaming) { Text(if (chat.loading) "Checking…" else "Retry connection") }
                TextButton(onClick = settings, enabled = !chat.streaming) { Text("Connection settings") }
            }
            TextButton(onClick = details) { Text("Technical details", style = MaterialTheme.typography.bodySmall) }
        }
    }
}

@Composable
private fun ChatConnectionPage(chat: DeviceChat, deviceName: String) {
    val nav = LocalNavigator.current
    val scope = rememberCoroutineScope()
    PageColumn {
        formSection("Inference server") {
            LabeledRow("Device", deviceName)
            OutlinedTextField(chat.port, { chat.port = it.filter(Char::isDigit) }, Modifier.fillMaxWidth(), label = { Text("Port") }, singleLine = true, keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number))
            ActionRow("Find models", enabled = !chat.streaming && !chat.loading && chat.portNumber != null) { scope.launch { chat.discover(); if (chat.error == null) nav.pop() } }
            if (chat.loading) LoadingRow("Finding models…")
        }
        if (chat.selected.isNotEmpty()) formSection("Model ID") { SelectionContainer { Text(chat.selected, Modifier.padding(vertical = 8.dp), fontFamily = FontFamily.Monospace, style = MaterialTheme.typography.bodySmall) } }
        item { SecondaryText("Wendy skills and MCP tools are available in this chat.") }
    }
}

@Composable
private fun ChatApprovalCard(approval: ChatApproval, deviceName: String, enabled: Boolean, resolve: (Boolean) -> Unit) {
    var settings by remember { mutableStateOf(false) }
    Surface(Modifier.fillMaxWidth().padding(horizontal = 4.dp), shape = RoundedCornerShape(14.dp), color = MaterialTheme.colorScheme.surface, border = BorderStroke(1.dp, MaterialTheme.colorScheme.outlineVariant)) {
        Column(Modifier.padding(18.dp), verticalArrangement = Arrangement.spacedBy(10.dp)) {
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically) {
                Icon(if (approval.isNotificationPermission) Icons.Outlined.NotificationsActive else if (approval.isCampaign) Icons.Outlined.PersonSearch else Icons.Outlined.PanTool, null, tint = MaterialTheme.colorScheme.primary)
                Text(approval.title, style = MaterialTheme.typography.titleMedium)
            }
            SecondaryText(deviceName)
            when {
                approval.isNotificationPermission -> {
                    Text("Allow “${approval.campaignName}” to send notifications through Wendy Cloud?")
                    SecondaryText("Applies across devices using this campaign in this device’s organization. This permission does not upload recordings. Android cannot receive these notifications yet.")
                }
                approval.isCampaign -> {
                    val args = approval.arguments
                    LabeledRow("Name", approval.campaignName); LabeledRow("Camera", approval.cameraName); LabeledRow("Recording", approval.clipSummary)
                    LabeledRow("Notifications", if (args.optBoolean("notify")) "Ask for permission next" else "Off")
                    LabeledRow("Upload clips", if (args.optBoolean("upload")) "Automatically" else "Off")
                    SecondaryText("Starts person detection and records clips on the device. The device may download a detection model.")
                    Text(if (settings) "Hide detection settings" else "Detection settings", Modifier.clickable { settings = !settings }, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.primary)
                    if (settings) { LabeledRow("Confidence", "%.0f%%".format(args.optDouble("threshold", 0.0) * 100)); LabeledRow("Alert interval", "${args.optInt("cooldown_seconds")} seconds") }
                }
                else -> {
                    Text(approval.action.replace('_', ' ').split(' ').joinToString(" ") { it.replaceFirstChar(Char::uppercase) })
                    Text(if (settings) "Hide details" else "Details", Modifier.clickable { settings = !settings }, style = MaterialTheme.typography.labelMedium, color = MaterialTheme.colorScheme.primary)
                    if (settings) SelectionContainer { Text(approval.app, Modifier.heightIn(max = 160.dp), fontFamily = FontFamily.Monospace, style = MaterialTheme.typography.bodySmall) }
                }
            }
            HorizontalDivider(color = MaterialTheme.colorScheme.outlineVariant)
            Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(8.dp, Alignment.End)) {
                OutlinedButton(onClick = { resolve(false) }, enabled = enabled) { Text("Cancel") }
                Button(onClick = { resolve(true) }, enabled = enabled) { Text(approval.confirmLabel) }
            }
        }
    }
}

@Composable
private fun ChatDeviceCards(model: WendyModel, cards: List<ChatCard>) {
    val nav = LocalNavigator.current
    val session = model.session
    cards.forEach { card ->
        when (card.kind) {
            "camera" -> {
                val camera = model.cameras.firstOrNull { it.number("id") == card.camera && it.text("name") == card.title }
                LaunchedEffect(card) { if (camera == null) model.refreshDetails() }
                if (camera != null) CameraTile(model, camera)
                else FormSection { Text(card.title, Modifier.padding(top = 8.dp), style = MaterialTheme.typography.titleSmall); SecondaryText("Camera is no longer available.") }
            }
            "notifications" -> FormSection(card.title) { SecondaryText("Push notifications are not available on Android yet.") }
            "resources" -> {
                var value by remember { mutableStateOf<ResourceSnapshot?>(null) }
                var error by remember { mutableStateOf<String?>(null) }
                val visible = LocalPageVisible.current
                LaunchedEffect(session, visible) {
                    while (visible) {
                        try { value = ResourceSnapshot(model.backend.call("Resources", session).obj()); error = null } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message }
                        delay(3000)
                    }
                }
                FormSection(card.title) {
                    error?.let { FailureNote(it) }
                    value?.let { r ->
                        if (r.hasHost) { LabeledRow("CPU", r.cpu?.let { "%.1f%%".format(it) } ?: "Unavailable"); LabeledRow("Memory", "${formatBytes(r.memoryUsed)} / ${formatBytes(r.memoryTotal)}") }
                        if (r.hasDisk) LabeledRow("Storage", "${formatBytes(r.diskUsed)} / ${formatBytes(r.diskTotal)}")
                        r.gpus.forEach { LabeledRow(it.name, "%.1f%%".format(it.utilization)) }
                        SecondaryText("Live device readings")
                    } ?: run { if (error == null) LoadingRow("Reading device activity…") }
                }
            }
            else -> {
                var info by remember { mutableStateOf<JSONObject?>(null) }
                var error by remember { mutableStateOf<String?>(null) }
                LaunchedEffect(session) { try { info = model.backend.call("DeviceInfo", session).obj() } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } }
                FormSection(card.title.ifEmpty { "Device info" }) {
                    error?.let { FailureNote(it) }
                    info?.let { i ->
                        LabeledRow("Model", i.text("board_model")); LabeledRow("Agent", i.text("version"))
                        LabeledRow("OS", "${i.text("os")} ${i.text("os_version")}".trim()); LabeledRow("Architecture", i.text("cpu_architecture"))
                        NavRow("Device info") { nav.push("Settings") { DeviceSettingsScreen(model) } }
                    } ?: run { if (error == null) LoadingRow("Loading device info…") }
                }
            }
        }
    }
}

// MARK: Markdown

/** Block-level Markdown for assistant replies; fenced code stays literal while it streams (iOS ChatMarkdownBlock). */
sealed class MarkdownBlock {
    data class Paragraph(val text: String) : MarkdownBlock()
    data class Heading(val level: Int, val text: String) : MarkdownBlock()
    data class Item(val marker: String, val depth: Int, val text: String) : MarkdownBlock()
    data class Code(val text: String) : MarkdownBlock()
    data class Quote(val text: String) : MarkdownBlock()
    data object Rule : MarkdownBlock()
    data class Table(val rows: List<List<String>>) : MarkdownBlock()
    companion object {
        fun cells(row: String): List<String> = row.trim().removePrefix("|").removeSuffix("|").split("|").map { it.trim() }
        private fun divider(row: String) = cells(row).let { c -> c.isNotEmpty() && c.all { cell -> cell.trim(':').let { it.length >= 3 && it.all { ch -> ch == '-' } } } }
        fun parse(source: String): List<MarkdownBlock> {
            val lines = source.split("\n")
            val blocks = mutableListOf<MarkdownBlock>()
            val paragraph = mutableListOf<String>()
            fun flush() { if (paragraph.isNotEmpty()) { blocks.add(Paragraph(paragraph.joinToString("\n"))); paragraph.clear() } }
            var index = 0
            while (index < lines.size) {
                val line = lines[index]; val trimmed = line.trim(); index++
                if (trimmed.isEmpty()) { flush(); continue }
                if (trimmed.startsWith("```") || trimmed.startsWith("~~~")) {
                    flush()
                    val fence = trimmed.takeWhile { it == trimmed.first() }
                    val code = mutableListOf<String>()
                    while (index < lines.size) { val next = lines[index]; index++; if (next.trim().startsWith(fence)) break; code.add(next) }
                    blocks.add(Code(code.joinToString("\n"))); continue
                }
                val heading = trimmed.takeWhile { it == '#' }.length
                if (heading in 1..6 && trimmed.getOrNull(heading) == ' ') { flush(); blocks.add(Heading(heading, trimmed.drop(heading + 1))); continue }
                if (trimmed in setOf("---", "***", "___")) { flush(); blocks.add(Rule); continue }
                if (trimmed.startsWith(">")) { flush(); blocks.add(Quote(trimmed.drop(1).trim())); continue }
                if (index < lines.size && '|' in trimmed && divider(lines[index])) {
                    flush(); index++
                    val rows = mutableListOf(cells(trimmed))
                    while (index < lines.size && '|' in lines[index] && lines[index].isNotBlank()) { rows.add(cells(lines[index])); index++ }
                    blocks.add(Table(rows)); continue
                }
                var marker: String? = null; var body = ""
                if (listOf("- ", "* ", "+ ").any { trimmed.startsWith(it) }) {
                    marker = "•"; body = trimmed.drop(2)
                    if (body.startsWith("[ ] ")) { marker = "☐"; body = body.drop(4) } else if (body.lowercase().startsWith("[x] ")) { marker = "☑"; body = body.drop(4) }
                } else {
                    val digits = trimmed.takeWhile(Char::isDigit); val rest = trimmed.drop(digits.length)
                    if (digits.isNotEmpty() && (rest.startsWith(". ") || rest.startsWith(") "))) { marker = "$digits."; body = rest.drop(2) }
                }
                if (marker != null) { flush(); blocks.add(Item(marker, minOf(6, (line.length - line.trimStart().length) / 2), body)) } else paragraph.add(line)
            }
            flush()
            return blocks
        }
        /** Inline emphasis, code and links. Unmatched markers stay literal. */
        fun inline(text: String, code: SpanStyle, link: SpanStyle): AnnotatedString = buildAnnotatedString {
            var i = 0
            while (i < text.length) {
                val rest = text.substring(i)
                val match = Regex("^`([^`]+)`").find(rest)?.let { m -> { withStyle(code) { append(m.groupValues[1]) } } to m.value.length }
                    ?: Regex("^\\*\\*(.+?)\\*\\*|^__(.+?)__").find(rest)?.let { m -> { withStyle(SpanStyle(fontWeight = FontWeight.SemiBold)) { append(inline(m.groupValues[1].ifEmpty { m.groupValues[2] }, code, link)) } } to m.value.length }
                    ?: Regex("^\\*([^*\\s][^*]*?)\\*|^_([^_\\s][^_]*?)_").find(rest)?.let { m -> { withStyle(SpanStyle(fontStyle = FontStyle.Italic)) { append(inline(m.groupValues[1].ifEmpty { m.groupValues[2] }, code, link)) } } to m.value.length }
                    ?: Regex("^\\[([^\\]]+)]\\((https?://[^)\\s]+)\\)").find(rest)?.let { m -> { withLink(LinkAnnotation.Url(m.groupValues[2], TextLinkStyles(link))) { append(m.groupValues[1]) } } to m.value.length }
                if (match != null) { match.first(); i += match.second } else { append(text[i]); i++ }
            }
        }
    }
}

@Composable
fun ChatMarkdown(source: String) {
    val codeStyle = SpanStyle(fontFamily = FontFamily.Monospace, background = MaterialTheme.colorScheme.surfaceVariant)
    val linkStyle = SpanStyle(color = MaterialTheme.colorScheme.primary, textDecoration = TextDecoration.Underline)
    val blocks = remember(source) { MarkdownBlock.parse(source) }
    fun inline(text: String) = MarkdownBlock.inline(text, codeStyle, linkStyle)
    SelectionContainer {
        Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
            blocks.forEach { block ->
                when (block) {
                    is MarkdownBlock.Paragraph -> Text(inline(block.text), style = MaterialTheme.typography.bodyLarge)
                    is MarkdownBlock.Heading -> Text(inline(block.text), Modifier.padding(top = 4.dp), style = if (block.level <= 2) MaterialTheme.typography.titleLarge else MaterialTheme.typography.titleMedium)
                    is MarkdownBlock.Item -> Row(Modifier.padding(start = (block.depth * 16).dp), horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                        Text(block.marker, Modifier.widthIn(min = 16.dp), style = MaterialTheme.typography.bodyLarge); Text(inline(block.text), style = MaterialTheme.typography.bodyLarge)
                    }
                    is MarkdownBlock.Code -> Box(Modifier.fillMaxWidth().background(MaterialTheme.colorScheme.surfaceVariant, RoundedCornerShape(6.dp)).horizontalScroll(rememberScrollState()).padding(10.dp)) {
                        Text(block.text, fontFamily = FontFamily.Monospace, style = MaterialTheme.typography.bodyMedium, softWrap = false)
                    }
                    is MarkdownBlock.Quote -> Row(Modifier.height(IntrinsicSize.Min), horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                        Box(Modifier.width(3.dp).fillMaxHeight().background(MaterialTheme.colorScheme.outline)); Text(inline(block.text), color = MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                    MarkdownBlock.Rule -> HorizontalDivider()
                    is MarkdownBlock.Table -> Column(Modifier.horizontalScroll(rememberScrollState())) {
                        block.rows.forEachIndexed { index, row ->
                            Row { row.forEach { cell -> Text(inline(cell), Modifier.widthIn(min = 80.dp).padding(6.dp), fontWeight = if (index == 0) FontWeight.SemiBold else null, style = MaterialTheme.typography.bodyMedium) } }
                            if (index == 0) HorizontalDivider()
                        }
                    }
                }
            }
        }
    }
}

// MARK: Model library

data class HubFile(val name: String, val quantization: String, val bytes: Long, val unavailableReason: String) {
    /** Weights plus 25% and 4 GiB for the runtime and a 32,768-token context (modelhub.MemoryEstimate). */
    val estimatedMemory get() = bytes + bytes / 4 + 4L * (1L shl 30)
}
enum class ModelFit(val title: String) { Unknown("Capacity unknown"), Comfortable("Fits estimated budget"), Tight("Close to memory limit"), TooLarge("Exceeds available capacity") }

class ModelHardware(value: JSONObject) {
    val os = value.text("OS"); val architecture = value.text("Architecture"); val gpu = value.text("GPU"); val jetpack = value.text("JetPack"); val warning = value.text("Warning")
    val memoryTotal = value.number("MemoryTotal"); val memoryAvailable = value.number("MemoryAvailable"); val diskFree = value.number("DiskFree")
    val gpuTotal = value.number("GPUTotal"); val gpuAvailable = value.number("GPUAvailable")
    val diskKnown = value.flag("DiskKnown"); val cuda = value.flag("CUDA"); val sharedMemory = value.flag("SharedMemory")
    val canInstall get() = os == "linux" && architecture in setOf("arm64", "aarch64", "amd64", "x86_64")
    /** The managed runtime follows capabilities, not memory size (modelhub.Hardware.Accelerator). */
    val accelerator: Int get() = if (!cuda) 0 else if (jetpack.startsWith("5")) 2 else if (jetpack.startsWith("6")) 3 else 1
    val computeName get() = when (accelerator) { 1 -> "NVIDIA GPU"; 2 -> "NVIDIA GPU · JetPack 5"; 3 -> "NVIDIA GPU · JetPack 6"; else -> "CPU" }
    fun fit(file: HubFile, install: Boolean): ModelFit {
        val disk = file.bytes + if (install) 12L * (1L shl 30) else 0
        if (diskKnown && disk > diskFree) return ModelFit.TooLarge
        val (total, available) = if (accelerator != 0 && !sharedMemory) gpuTotal to gpuAvailable else memoryTotal to memoryAvailable
        if (total <= 0) return ModelFit.Unknown
        if (file.estimatedMemory > total || file.estimatedMemory > available) return ModelFit.TooLarge
        if (file.estimatedMemory > available - available / 5) return ModelFit.Tight
        return if (diskKnown) ModelFit.Comfortable else ModelFit.Unknown
    }
}

private fun parseFiles(value: JSONObject) = value.list("Files").map { HubFile(it.text("Name"), it.text("Quantization"), it.number("Bytes"), it.text("UnavailableReason")) }

@Composable
private fun fitColor(fit: ModelFit) = when (fit) { ModelFit.Comfortable -> MaterialTheme.colorScheme.primary; ModelFit.Tight -> Color(0xFFE0912B); ModelFit.TooLarge -> MaterialTheme.colorScheme.error; else -> MaterialTheme.colorScheme.onSurfaceVariant }

/** Search Hugging Face, check capacity and download a GGUF model to the device (iOS ModelLibraryView on iPhone). */
@Composable
fun ModelLibraryScreen(model: WendyModel, port: String, ready: (String, Long) -> Unit) {
    val nav = LocalNavigator.current
    val session = model.session
    var query by remember { mutableStateOf("") }
    var repositories by remember { mutableStateOf(emptyList<JSONObject>()) }
    val details = remember { mutableStateMapOf<String, JSONObject>() }
    var hardware by remember { mutableStateOf<ModelHardware?>(null) }
    var searching by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    LaunchedEffect(session) {
        try { hardware = ModelHardware(model.backend.call("ModelHardware", session).obj()) } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message }
    }
    LaunchedEffect(query) {
        val text = query
        searching = true; error = null
        try {
            if (text.isNotEmpty()) delay(350)
            val rows = model.backend.call("SearchModels", text).items()
            if (query != text) return@LaunchedEffect
            repositories = rows
            // Read actual file sizes before marking a starter model as suitable. Never infer capacity from parameter count.
            if (text.isEmpty()) rows.filter { details[it.text("ID")] == null }.forEach { repo ->
                runCatching { model.backend.call("ModelFiles", repo.text("ID")).obj() }.getOrNull()?.let { details[repo.text("ID")] = it }
            }
        } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { searching = false }
    }
    val sorted = if (query.isNotBlank()) repositories else repositories.sortedWith(compareByDescending<JSONObject> { repo ->
        details[repo.text("ID")]?.let(::parseFiles)?.firstOrNull()?.let { hardware?.fit(it, hardware?.canInstall == true) } == ModelFit.Comfortable
    }.thenBy { details[it.text("ID")]?.let(::parseFiles)?.firstOrNull()?.bytes ?: Long.MAX_VALUE }.thenBy { it.text("ID") })
    Column(Modifier.fillMaxSize()) {
        OutlinedTextField(query, { query = it }, Modifier.fillMaxWidth().padding(16.dp), singleLine = true, placeholder = { Text("Search Hugging Face") }, leadingIcon = { Icon(Icons.Outlined.Search, null) },
            trailingIcon = if (query.isNotEmpty()) ({ IconButton(onClick = { query = "" }) { Icon(Icons.Outlined.Close, "Clear search") } }) else null)
        PageColumn(Modifier.weight(1f)) {
            if (searching && repositories.isEmpty()) item { LoadingRow("Finding models…") }
            error?.let { item { FormSection { FailureNote(it) } } }
            formSection(if (query.isEmpty()) "Suggested models" else "Search results") {
                sorted.forEach { repo ->
                    val id = repo.text("ID")
                    Row(Modifier.fillMaxWidth().clickable { nav.push("Model details") { ModelDetailPage(model, id, port, hardware, details) { name, p -> ready(name, p); nav.pop(); nav.pop() } } }.padding(vertical = 8.dp),
                        verticalAlignment = Alignment.CenterVertically) {
                        Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(3.dp)) {
                            Text(ChatModelOption.readableName(repo.text("Name").ifEmpty { id }), style = MaterialTheme.typography.bodyLarge)
                            if (repo.text("Description").isNotEmpty()) Text(repo.text("Description"), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 2)
                            details[id]?.let(::parseFiles)?.firstOrNull()?.let { file ->
                                val fit = hardware?.fit(file, hardware?.canInstall == true) ?: ModelFit.Unknown
                                Text("${formatBytes(file.bytes)} · ${fit.title}", style = MaterialTheme.typography.bodySmall, color = fitColor(fit))
                            }
                        }
                        Icon(Icons.Outlined.ChevronRight, null, tint = MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                }
                if (!searching && sorted.isEmpty()) SecondaryText("No models found")
            }
        }
    }
}

@Composable
private fun ModelDetailPage(model: WendyModel, repo: String, initialPort: String, hardware: ModelHardware?, cache: MutableMap<String, JSONObject>, ready: (String, Long) -> Unit) {
    val context = LocalContext.current
    val session = model.session
    val scope = rememberCoroutineScope()
    var detail by remember { mutableStateOf(cache[repo]) }
    var file by remember { mutableStateOf("") }
    var install by remember { mutableStateOf(hardware?.canInstall == true) }
    var port by remember { mutableStateOf(initialPort) }
    var acceptWarning by remember { mutableStateOf(false) }
    var running by remember { mutableStateOf(false) }
    var progress by remember { mutableStateOf("") }
    var completed by remember { mutableLongStateOf(0) }
    var total by remember { mutableLongStateOf(0) }
    var error by remember { mutableStateOf<String?>(null) }
    var job by remember { mutableLongStateOf(0) }
    var runJob by remember { mutableStateOf<Job?>(null) }
    LaunchedEffect(repo) {
        try { val value = cache[repo] ?: model.backend.call("ModelFiles", repo).obj(); cache[repo] = value; detail = value; file = parseFiles(value).firstOrNull()?.name ?: "" }
        catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message }
    }
    LaunchedEffect(file, install) { acceptWarning = false }
    DisposableEffect(Unit) { onDispose { if (job != 0L) { val captured = job; model.task { model.backend.call("CancelModelRun", session, captured) } } } }
    val files = detail?.let(::parseFiles).orEmpty()
    val selected = files.firstOrNull { it.name == file }
    val fit = selected?.let { hardware?.fit(it, install) } ?: ModelFit.Unknown
    val portNumber = port.toLongOrNull()?.takeIf { it in 1..65535 }
    val canRun = !running && selected?.unavailableReason?.isEmpty() == true && (install || portNumber != null) && (!install || hardware?.canInstall == true) && (fit == ModelFit.Comfortable || acceptWarning)
    fun start() {
        val value = detail ?: return
        running = true; error = null; progress = "Checking device and model"; completed = 0; total = 0
        val request = model.nextID(); job = request
        val targetPort = if (install) 11434 else portNumber ?: 11434
        runJob = scope.launch {
            try {
                model.backend.call("StartModelRun", session, request, targetPort, hardware?.accelerator ?: 0, repo, file, value.text("Revision"), install, acceptWarning)
                while (job == request) {
                    val status = model.backend.call("ModelRunStatus", session, request).obj()
                    progress = status.text("Progress"); completed = status.number("Completed"); total = status.number("Total")
                    if (status.flag("Done")) {
                        if (status.text("Error").isEmpty()) ready(status.text("Model"), status.number("Port")) else { error = status.text("Error"); acceptWarning = false }
                        break
                    }
                    delay(300)
                }
            } catch (e: CancellationException) { throw e } catch (e: Exception) { if (job == request) error = e.message }
            finally { if (job == request) { running = false; job = 0 }; withContext(NonCancellable) { try { model.backend.call("CancelModelRun", session, request) } catch (_: Exception) {} } }
        }
    }
    PageColumn {
        val value = detail
        if (value == null) { item { if (error == null) LoadingRow("Reading model files…") else FormSection { FailureNote(error!!) } }; return@PageColumn }
        formSection {
            Text(ChatModelOption.readableName(repo), Modifier.padding(top = 8.dp), style = MaterialTheme.typography.titleMedium)
            SelectionContainer { SecondaryText(repo) }
            ActionRow("Model card", Icons.AutoMirrored.Outlined.OpenInNew) { context.startActivity(Intent(Intent.ACTION_VIEW, Uri.parse("https://huggingface.co/$repo"))) }
        }
        formSection("Weights") { ChoiceField("Quality & size", files.map { it.name to "${it.quantization} · ${formatBytes(it.bytes)}" }, file, !running) { file = it } }
        selected?.let { f ->
            if (f.unavailableReason.isNotEmpty()) formSection("Runtime support") { SecondaryText(f.unavailableReason) }
            formSection("Capacity", "Includes weights, 25% overhead and 4 GiB for the runtime and a 32,768-token context. Actual memory varies by model." + if (install) " Allow an additional 12 GiB of storage for the runtime and extraction." else "") {
                Text(fit.title, Modifier.padding(vertical = 8.dp), color = fitColor(fit), style = MaterialTheme.typography.titleSmall)
                LabeledRow("Download", formatBytes(f.bytes)); LabeledRow("Estimated memory", formatBytes(f.estimatedMemory))
            }
            if (f.unavailableReason.isEmpty()) {
                formSection("Run on device", if (install) "Wendy selects the GPU and an available port automatically when supported. Models stay on the device." else "Uses an existing Ollama server and its compute settings.") {
                    ToggleRow("Manage runtime automatically", install, enabled = !running && hardware?.canInstall == true) { install = it }
                    if (!install) OutlinedTextField(port, { port = it.filter(Char::isDigit) }, Modifier.fillMaxWidth(), label = { Text("Server port") }, singleLine = true, enabled = !running, keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Number))
                    hardware?.let { LabeledRow("Compute", it.computeName) }
                    if (fit != ModelFit.Comfortable) ToggleRow("Run despite capacity warning", acceptWarning, enabled = !running) { acceptWarning = it }
                }
                formSection {
                    if (running) {
                        Text(progress, Modifier.padding(top = 8.dp), style = MaterialTheme.typography.bodyMedium)
                        if (total > 0) LinearProgressIndicator(progress = { (completed.toFloat() / total).coerceIn(0f, 1f) }, Modifier.fillMaxWidth().padding(vertical = 6.dp)) else LinearProgressIndicator(Modifier.fillMaxWidth().padding(vertical = 6.dp))
                        ActionRow("Cancel download") { job = 0; running = false; runJob?.cancel() }
                    } else ActionRow("Download & Run", Icons.Outlined.Download, enabled = canRun) { start() }
                }
            }
        }
        formSection(model.selected?.title ?: "Device") {
            hardware?.let { h ->
                LabeledRow("Available memory", formatBytes(h.memoryAvailable))
                if (h.diskKnown) LabeledRow("Free storage", formatBytes(h.diskFree))
                if (h.warning.isNotEmpty()) Text(h.warning, style = MaterialTheme.typography.bodySmall, color = Color(0xFFE0912B))
                if (!h.canInstall) SecondaryText("Automatic runtime installation requires Linux on arm64 or x86-64. An existing Ollama server can still be used.")
            } ?: LoadingRow("Checking device capacity")
            LabeledRow("License", value.text("License").ifEmpty { "See model card" })
        }
        error?.let { formSection { FailureNote(it) } }
    }
}
