package dev.wendy.android

import androidx.compose.foundation.layout.*
import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.ui.Alignment
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.input.VisualTransformation
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.CancellationException
import org.json.JSONArray
import org.json.JSONObject

class RemoteState {
    var value by mutableStateOf<Any?>(null)
    var error by mutableStateOf<String?>(null)
    var loading by mutableStateOf(true)
    var revision by mutableIntStateOf(0)
    fun refresh() { revision++ }
}
@Composable
fun rememberRemote(model: WendyModel, method: String, vararg args: Any?): RemoteState {
    val state = remember(method, args.toList()) { RemoteState() }
    LaunchedEffect(state, state.revision, model.session, model.account, model.organization) {
        state.loading = true; state.error = null
        try { state.value = model.backend.call(method, *args) }
        catch (e: CancellationException) { throw e }
        catch (e: Exception) { state.error = e.message }
        finally { state.loading = false }
    }
    return state
}
@Composable fun RemoteStatus(state: RemoteState) {
    if (state.loading) LinearProgressIndicator(Modifier.fillMaxWidth().padding(vertical = 8.dp).height(3.dp))
    state.error?.let { message ->
        Surface(color = MaterialTheme.colorScheme.errorContainer, shape = RoundedCornerShape(18.dp), modifier = Modifier.fillMaxWidth().padding(vertical = 8.dp)) {
            Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Row(horizontalArrangement = Arrangement.spacedBy(10.dp), verticalAlignment = Alignment.Top) {
                    Icon(Icons.Outlined.ErrorOutline, null, Modifier.size(20.dp), tint = MaterialTheme.colorScheme.onErrorContainer)
                    Text(message, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onErrorContainer)
                }
                TextButton(onClick = state::refresh) { Text("Try again") }
            }
        }
    }
}
data class Field(val id: String, val label: String, val initial: String = "", val hint: String = "", val secret: Boolean = false, val numeric: Boolean = false, val choices: List<Pair<String, String>>? = null)
@Composable
fun InputDialog(title: String, fields: List<Field>, onDismiss: () -> Unit, submitLabel: String = "Continue", onSubmit: (Map<String, String>) -> Unit) {
    val values = remember(fields) { mutableStateMapOf<String,String>().apply { fields.forEach { put(it.id, it.initial) } } }
    AlertDialog(onDismissRequest = onDismiss, title = { Text(title) }, text = {
        Column(Modifier.verticalScroll(rememberScrollState()), verticalArrangement = Arrangement.spacedBy(10.dp)) {
            fields.forEach { field ->
                if (field.choices != null) {
                    var expanded by remember(field.id) { mutableStateOf(false) }
                    Column {
                        Text(field.label, style = MaterialTheme.typography.labelMedium)
                        Box {
                            OutlinedButton(onClick = { expanded = true }, enabled = field.choices.isNotEmpty(), modifier = Modifier.fillMaxWidth()) {
                                Text(field.choices.firstOrNull { it.first == values[field.id] }?.second ?: if (field.choices.isEmpty()) "No choices available" else "Choose ${field.label.lowercase()}", Modifier.weight(1f))
                                Icon(Icons.Outlined.ExpandMore, null, Modifier.size(18.dp))
                            }
                            DropdownMenu(expanded, onDismissRequest = { expanded = false }) {
                                field.choices.forEach { (id, name) -> DropdownMenuItem(text = { Text(name) }, onClick = { values[field.id] = id; expanded = false }) }
                            }
                        }
                    }
                } else OutlinedTextField(value = values[field.id] ?: "", onValueChange = { values[field.id] = it }, label = { Text(field.label) }, placeholder = { Text(field.hint) }, singleLine = true, modifier = Modifier.fillMaxWidth(),
                    visualTransformation = if (field.secret) PasswordVisualTransformation() else VisualTransformation.None,
                    keyboardOptions = KeyboardOptions(keyboardType = if (field.secret) KeyboardType.Password else if (field.numeric) KeyboardType.Decimal else KeyboardType.Text))
            }
        }
    }, confirmButton = { TextButton(enabled = fields.all { it.choices == null || it.choices.any { choice -> choice.first == values[it.id] } }, onClick = { onSubmit(values.toMap()) }) { Text(submitLabel) } }, dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } })
}
@Composable fun ConfirmDialog(title: String, description: String, onDismiss: () -> Unit, onConfirm: () -> Unit) {
    AlertDialog(onDismissRequest = onDismiss, title = { Text(title) }, text = { Text(description) }, confirmButton = { TextButton(onClick = onConfirm) { Text("Confirm") } }, dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } })
}
@Composable fun SectionCard(title: String, content: @Composable ColumnScope.() -> Unit) {
    Surface(Modifier.fillMaxWidth(), shape = RoundedCornerShape(24.dp), color = MaterialTheme.colorScheme.surface,
        border = BorderStroke(1.dp, MaterialTheme.colorScheme.outlineVariant)) {
        Column(Modifier.padding(20.dp), verticalArrangement = Arrangement.spacedBy(12.dp)) {
            Text(title, style = MaterialTheme.typography.titleMedium)
            content()
        }
    }
}

/** Raw metadata is only composed after an explicit request. */
@Composable fun TechnicalDetails(value: Any?, title: String = "Technical details") {
    if (value == null || value == JSONObject.NULL) return
    var expanded by remember { mutableStateOf(false) }
    Column {
        TextButton(onClick = { expanded = !expanded }) {
            Text(if (expanded) "Hide $title" else title.replaceFirstChar { it.uppercase() })
            Spacer(Modifier.width(6.dp))
            Icon(if (expanded) Icons.Outlined.ExpandLess else Icons.Outlined.ExpandMore, null, Modifier.size(18.dp))
        }
        if (expanded) SelectionContainer { Column(Modifier.padding(12.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) { Details(value) } }
    }
}
@Composable fun SummaryLine(label: String, value: String) {
    if (value.isBlank()) return
    Row(Modifier.fillMaxWidth().padding(vertical = 3.dp), horizontalArrangement = Arrangement.spacedBy(16.dp)) {
        Text(label, Modifier.weight(1f), style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
        Text(value, Modifier.weight(1.3f), style = MaterialTheme.typography.titleSmall, textAlign = androidx.compose.ui.text.style.TextAlign.End)
    }
}
@Composable fun OptionalMessage(message: String) {
    if (message.isNotBlank()) Text(message, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
}

private fun label(key: String) = key.replace('_', ' ').replace(Regex("([a-z])([A-Z])"), "$1 $2").replaceFirstChar { it.uppercase() }
/** Read-only metadata renderer. Binary payloads and credentials never become UI text. */
@Composable fun Details(value: Any?, depth: Int = 0) {
    if (depth > 5 || value == null || value == JSONObject.NULL) return
    when (value) {
        is JSONObject -> value.keys().asSequence().filterNot { it.lowercase() in setOf("bytes", "image", "rgba", "password", "token", "private_key", "plan_json", "data") }.toList().forEach { key ->
            val child = value.opt(key)
            if (child is JSONObject || child is JSONArray) {
                if (child.toString() !in listOf("[]", "{}")) { Text(label(key), style = MaterialTheme.typography.labelLarge); Column(Modifier.padding(start = 10.dp), verticalArrangement = Arrangement.spacedBy(4.dp)) { Details(child, depth + 1) } }
            } else if (child != null && child != JSONObject.NULL && child.toString().isNotBlank()) {
                Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                    Text(label(key), Modifier.weight(0.4f), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                    SelectionContainer(Modifier.weight(0.6f)) { Text(child.toString().take(4000), style = MaterialTheme.typography.bodySmall) }
                }
            }
        }
        is JSONArray -> (0 until minOf(value.length(), 100)).forEach { index -> Details(value.opt(index), depth + 1); if (index < value.length()-1) HorizontalDivider() }
        else -> Text(value.toString().take(4000), style = MaterialTheme.typography.bodySmall)
    }
}
@Composable fun DataPage(model: WendyModel, title: String, method: String, vararg args: Any?) {
    val state = rememberRemote(model, method, *args)
    Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(20.dp), verticalArrangement = Arrangement.spacedBy(14.dp)) {
        PageTitle(title); RemoteStatus(state)
        OutlinedButton(onClick = state::refresh) { Text("Refresh") }
        if (!state.loading && state.error == null) when (method) {
            "Ports" -> {
                if (state.value.items().isEmpty()) EmptyMessage("No exposed ports", "This app has no ports to display.")
                state.value.items().forEach { port -> SectionCard("Port ${port.number("port")}") {
                    SummaryLine("Protocol", port.text("protocol").uppercase())
                    SummaryLine("Address", port.text("address"))
                } }
            }
            "NetworkTraffic" -> {
                val data = state.value.obj()
                OptionalMessage(data.text("interfacesUnavailableReason"))
                data.list("interfaces").filterNot { it.flag("loopback") || it.flag("virtual") }.forEach { network -> SectionCard(network.text("name")) {
                    SummaryLine("Received", formatBytes(network.number("receivedBytes")))
                    SummaryLine("Sent", formatBytes(network.number("sentBytes")))
                } }
                TechnicalDetails(data, "All interfaces and counters")
            }
            else -> TechnicalDetails(state.value)
        }
    }
}
