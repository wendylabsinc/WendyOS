package dev.wendy.android

import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.*
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.ArrowUpward
import androidx.compose.ui.Alignment
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.delay
import org.json.JSONArray
import org.json.JSONObject

@Composable
fun ChatScreen(model: WendyModel) {
    val models = rememberRemote(model, "ChatModels", model.session, 11434)
    var selected by remember { mutableStateOf<JSONObject?>(null) }
    var prompt by remember { mutableStateOf("") }
    var messages by remember { mutableStateOf(emptyList<JSONObject>()) }
    var turn by remember { mutableLongStateOf(0) }
    var snapshot by remember { mutableStateOf(JSONObject()) }
    var running by remember { mutableStateOf(false) }
    val session = model.session
    LaunchedEffect(models.value) { if (selected == null) selected = models.value.items().firstOrNull { it.text("UnavailableReason").isEmpty() } }
    LaunchedEffect(turn, running) {
        if (!running || turn == 0L) return@LaunchedEffect
        try {
            while (running) {
                snapshot = model.backend.call("PollChat", session, turn).obj()
                if (snapshot.flag("Done")) {
                    val text = snapshot.text("Text")
                    if (text.isNotEmpty()) messages = messages + json("Role" to "assistant", "Content" to text)
                    snapshot.text("Error").takeIf { it.isNotBlank() }?.let { model.error = it }
                    running = false
                } else delay(150)
            }
        } catch (e: CancellationException) { throw e }
        catch (e: Exception) { running = false; model.error = e.message }
    }
    DisposableEffect(session) { onDispose { if (turn != 0L) model.task { model.backend.call("StopChat", session, turn) } } }
    Column(Modifier.fillMaxSize().padding(16.dp), verticalArrangement = Arrangement.spacedBy(10.dp)) {
        PageTitle("Device chat", "Models and tools run on ${model.selected?.name}")
        RemoteStatus(models)
        var menu by remember { mutableStateOf(false) }
        Box {
            OutlinedButton(onClick = { menu = true }, enabled = !running) { Text(selected?.text("Name", "ID") ?: "Choose a model") }
            DropdownMenu(expanded = menu, onDismissRequest = { menu = false }) {
                models.value.items().forEach { item -> DropdownMenuItem(text = { Text(item.text("Name", "ID")) }, enabled = item.text("UnavailableReason").isEmpty(), onClick = { selected = item; menu = false }) }
            }
        }
        LazyColumn(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            if (messages.isEmpty() && !running) item { EmptyMessage("Ask about your device", "Inspect apps, diagnose issues, or manage campaigns. Changes that need approval will ask first.") }
            items(messages) { message ->
                val user = message.text("Role") == "user"
                Column(Modifier.fillMaxWidth(), horizontalAlignment = if (user) Alignment.End else Alignment.Start, verticalArrangement = Arrangement.spacedBy(6.dp)) {
                    Eyebrow(if (user) "You" else "Wendy")
                    Surface(color = if (user) MaterialTheme.colorScheme.secondaryContainer else MaterialTheme.colorScheme.surface,
                        shape = RoundedCornerShape(topStart = 20.dp, topEnd = 20.dp, bottomStart = if (user) 20.dp else 4.dp, bottomEnd = if (user) 4.dp else 20.dp), modifier = Modifier.fillMaxWidth(.94f)) {
                        SelectionContainer { Text(message.text("Content"), Modifier.padding(16.dp), style = MaterialTheme.typography.bodyMedium) }
                    }
                }
            }
            if (running) item { SectionCard("Wendy") { SelectionContainer { Text(snapshot.text("Text").ifBlank { "Thinking…" }) }; Text(snapshot.text("Activity"), style = MaterialTheme.typography.bodySmall) } }
            snapshot.list("Cards").forEach { card -> item { ChatResultCard(model, card) } }
        }
        if (running) TextButton(onClick = { model.task { model.backend.call("StopChat", session, turn); running = false } }) { Text("Stop response") }
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.CenterVertically, modifier = Modifier.imePadding()) {
            OutlinedTextField(prompt, { prompt = it }, Modifier.weight(1f), placeholder = { Text("Ask Wendy…") }, maxLines = 5, enabled = !running, shape = RoundedCornerShape(26.dp))
            Button(enabled = !running && prompt.isNotBlank() && selected != null, onClick = {
                val chosen = selected ?: return@Button
                val nextMessages = messages + json("Role" to "user", "Content" to prompt)
                val nextTurn = model.nextID()
                model.task {
                    model.backend.call("StartChat", session, nextTurn, chosen.number("Port").takeIf { it > 0 } ?: 11434, chosen.text("ID", "Name"), JSONArray(nextMessages))
                    messages = nextMessages; prompt = ""; snapshot = JSONObject(); turn = nextTurn; running = true
                }
            }, contentPadding = PaddingValues(14.dp), modifier = Modifier.size(52.dp)) { Icon(Icons.Outlined.ArrowUpward, "Send message") }
        }
    }
    if (running && snapshot.number("ApprovalID") != 0L) {
        val approval = snapshot.number("ApprovalID")
        fun resolve(allow: Boolean) = model.task { model.backend.call("ResolveChatApproval", session, turn, approval, allow) }
        AlertDialog(onDismissRequest = { resolve(false) }, title = { Text("Allow device action?") }, text = { Column { Text(snapshot.text("ApprovalAction")); Text(snapshot.text("ApprovalApp")) } }, confirmButton = { TextButton(onClick = { resolve(true) }) { Text("Allow") } }, dismissButton = { TextButton(onClick = { resolve(false) }) { Text("Deny") } })
    }
}

@Composable
fun ModelsScreen(model: WendyModel) {
    val hardware = rememberRemote(model, "ModelHardware", model.session)
    val models = rememberRemote(model, "ChatModels", model.session, 11434)
    var install by remember { mutableStateOf(false) }
    var job by remember { mutableLongStateOf(0) }
    var state by remember { mutableStateOf(JSONObject()) }
    LaunchedEffect(job) {
        if (job != 0L) try { while (true) { state = model.backend.call("ModelRunStatus", model.session, job).obj(); if (state.flag("Done")) { models.refresh(); break }; delay(500) } }
        catch (e: CancellationException) { throw e } catch (e: Exception) { model.error = e.message }
    }
    Column(Modifier.verticalScroll(rememberScrollState()).padding(20.dp), verticalArrangement = Arrangement.spacedBy(14.dp)) {
        PageTitle("Models"); RemoteStatus(hardware)
        if (!hardware.loading && hardware.error == null) SectionCard("Capacity for models") {
            val capacity = hardware.value.obj()
            if (capacity.number("MemoryTotal") > 0) SummaryLine("Available memory", formatBytes(capacity.number("MemoryAvailable")))
            if (capacity.flag("DiskKnown")) SummaryLine("Free storage", formatBytes(capacity.number("DiskFree")))
            if (capacity.text("GPU").isNotBlank()) SummaryLine("Graphics", capacity.text("GPU"))
            OptionalMessage(capacity.text("Warning"))
            TechnicalDetails(capacity)
        }
        SectionCard("Available models") {
            RemoteStatus(models)
            if (!models.loading && models.error == null && models.value.items().isEmpty()) Text("No models installed yet. Download one to start chatting with your device.")
            models.value.items().forEach { item -> Column(verticalArrangement = Arrangement.spacedBy(6.dp)) {
                Text(item.text("Name", "ID"), style = MaterialTheme.typography.titleMedium)
                OptionalMessage(item.text("Description"))
                if (item.text("UnavailableReason").isBlank()) StatusPill("Available") else OptionalMessage(item.text("UnavailableReason"))
            } }
        }
        Button(onClick = { install = true }, enabled = job == 0L || state.flag("Done")) { Text("Download and run a model") }
        if (job != 0L) SectionCard("Model preparation") {
            Text(state.text("Model"), style = MaterialTheme.typography.titleMedium)
            OptionalMessage(state.text("Progress"))
            OptionalMessage(state.text("Error"))
            if (state.number("Total") > 0) {
                LinearProgressIndicator(progress = { (state.number("Completed").toFloat() / state.number("Total")).coerceIn(0f, 1f) }, modifier = Modifier.fillMaxWidth())
                Text("${formatBytes(state.number("Completed"))} of ${formatBytes(state.number("Total"))}", style = MaterialTheme.typography.bodySmall)
            }
            if (!state.flag("Done")) TextButton(onClick = { model.task { model.backend.call("CancelModelRun", model.session, job) } }) { Text("Cancel") }
        }
    }
    if (install) InputDialog("Run a model", listOf(Field("repo", "Hugging Face repository", hint = "owner/model-GGUF"), Field("file", "GGUF filename"), Field("revision", "Revision", "main")), { install = false }, "Download") { f ->
        install = false; model.task { val id = model.nextID(); model.backend.call("StartModelRun", model.session, id, 11434, 0, f["repo"], f["file"], f["revision"], true, false); job = id }
    }
}

@Composable
private fun ChatResultCard(model: WendyModel, card: JSONObject) {
    when (card.text("Kind")) {
        "camera" -> CameraTile(model, json("id" to card.number("Camera"), "name" to card.text("Title")))
        "resources" -> {
            val state = rememberRemote(model, "Resources", model.session)
            SectionCard("Device performance") {
                RemoteStatus(state)
                val data = state.value.obj(); val host = data.child("Host")
                if (data.flag("HasCPU")) SummaryLine("CPU usage", "%.0f%%".format(data.optDouble("CPUPercent")))
                if (host.number("mem_total_bytes") > 0) SummaryLine("Memory in use", formatBytes(host.number("mem_total_bytes") - host.number("mem_available_bytes")))
                TextButton(onClick = state::refresh) { Text("Refresh") }
            }
        }
        "device_info" -> {
            val state = rememberRemote(model, "DeviceInfo", model.session)
            SectionCard(model.selected?.name ?: "Device") {
                RemoteStatus(state)
                SummaryLine("Model", state.value.obj().text("board_model"))
                SummaryLine("Software", state.value.obj().text("os_version", "os"))
                TechnicalDetails(state.value)
            }
        }
        "notifications" -> SectionCard("Notifications") { Text("Push notifications are not available on Android yet.") }
        else -> SectionCard(card.text("Title").ifBlank { "Device information" }) { TechnicalDetails(card) }
    }
}
