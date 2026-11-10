package dev.wendy.android

import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.clickable
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.lazy.LazyListScope
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp

// Grouped-form building blocks matching the iOS Form/Section layout, drawn with
// the Android design system's cards and colors.

/** A titled card of rows with an optional footer, like an iOS form section. */
@Composable
fun FormSection(title: String? = null, footer: String? = null, content: @Composable ColumnScope.() -> Unit) {
    Column(Modifier.fillMaxWidth(), verticalArrangement = Arrangement.spacedBy(6.dp)) {
        if (title != null) Eyebrow(title)
        Surface(Modifier.fillMaxWidth(), shape = RoundedCornerShape(20.dp), color = MaterialTheme.colorScheme.surface,
            border = BorderStroke(1.dp, MaterialTheme.colorScheme.outlineVariant)) {
            Column(Modifier.padding(horizontal = 16.dp, vertical = 8.dp), verticalArrangement = Arrangement.spacedBy(2.dp), content = content)
        }
        if (footer != null) Text(footer, Modifier.padding(horizontal = 4.dp), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

fun LazyListScope.formSection(title: String? = null, footer: String? = null, content: @Composable ColumnScope.() -> Unit) {
    item { FormSection(title, footer, content) }
}

/** Label on the left, value on the right (iOS LabeledContent). Empty values are hidden. */
@Composable
fun LabeledRow(label: String, value: String, selectable: Boolean = false, valueColor: androidx.compose.ui.graphics.Color = MaterialTheme.colorScheme.onSurface) {
    if (value.isEmpty()) return
    Row(Modifier.fillMaxWidth().padding(vertical = 9.dp), horizontalArrangement = Arrangement.spacedBy(16.dp), verticalAlignment = Alignment.CenterVertically) {
        Text(label, Modifier.weight(1f), style = MaterialTheme.typography.bodyMedium)
        val text = @Composable { Text(value, style = MaterialTheme.typography.bodyMedium, color = valueColor, textAlign = TextAlign.End) }
        Box(Modifier.weight(1.2f), contentAlignment = Alignment.CenterEnd) { if (selectable) SelectionContainer { text() } else text() }
    }
}

/** A tappable row that opens another page, with an optional current value. */
@Composable
fun NavRow(title: String, icon: ImageVector? = null, value: String = "", subtitle: String = "", enabled: Boolean = true, onClick: () -> Unit) {
    Row(Modifier.fillMaxWidth().clickable(enabled = enabled, onClick = onClick).padding(vertical = 11.dp), horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
        if (icon != null) Icon(icon, null, Modifier.size(22.dp), tint = if (enabled) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurfaceVariant)
        Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(2.dp)) {
            Text(title, style = MaterialTheme.typography.bodyLarge, color = if (enabled) MaterialTheme.colorScheme.onSurface else MaterialTheme.colorScheme.onSurfaceVariant)
            if (subtitle.isNotEmpty()) Text(subtitle, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        if (value.isNotEmpty()) Text(value, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis, modifier = Modifier.widthIn(max = 160.dp))
        Icon(Icons.Outlined.ChevronRight, null, Modifier.size(20.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

/** A plain action row inside a form section. */
@Composable
fun ActionRow(title: String, icon: ImageVector? = null, destructive: Boolean = false, enabled: Boolean = true, onClick: () -> Unit) {
    val color = if (!enabled) MaterialTheme.colorScheme.onSurfaceVariant else if (destructive) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.primary
    Row(Modifier.fillMaxWidth().clickable(enabled = enabled, onClick = onClick).padding(vertical = 12.dp), horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
        if (icon != null) Icon(icon, null, Modifier.size(20.dp), tint = color)
        Text(title, style = MaterialTheme.typography.bodyLarge, color = color)
    }
}

@Composable
fun ToggleRow(title: String, checked: Boolean, enabled: Boolean = true, subtitle: String = "", onChange: (Boolean) -> Unit) {
    Row(Modifier.fillMaxWidth().padding(vertical = 6.dp), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
        Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(2.dp)) {
            Text(title, style = MaterialTheme.typography.bodyLarge, color = if (enabled) MaterialTheme.colorScheme.onSurface else MaterialTheme.colorScheme.onSurfaceVariant)
            if (subtitle.isNotEmpty()) Text(subtitle, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        Switch(checked = checked, onCheckedChange = onChange, enabled = enabled)
    }
}

@Composable
fun LoadingRow(text: String) {
    Row(Modifier.fillMaxWidth().padding(vertical = 10.dp), horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
        CircularProgressIndicator(Modifier.size(18.dp), strokeWidth = 2.dp)
        Text(text, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}

@Composable
fun SecondaryText(text: String, modifier: Modifier = Modifier) {
    if (text.isNotBlank()) Text(text, modifier.padding(vertical = 4.dp), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
}

/** A short failure message that expands to redacted technical details (iOS FailureNote). */
@Composable
fun FailureNote(message: String) {
    val failure = remember(message) { PresentedFailure(message) }
    var expanded by remember { mutableStateOf(false) }
    Column(Modifier.fillMaxWidth().clickable { expanded = !expanded }.padding(vertical = 8.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
        Row(horizontalArrangement = Arrangement.spacedBy(8.dp), verticalAlignment = Alignment.Top) {
            Icon(Icons.Outlined.ErrorOutline, null, Modifier.size(18.dp), tint = MaterialTheme.colorScheme.error)
            Text(failure.message, Modifier.weight(1f), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            Icon(if (expanded) Icons.Outlined.ExpandLess else Icons.Outlined.ExpandMore, if (expanded) "Hide details" else "Show details", Modifier.size(18.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
        }
        if (expanded) SelectionContainer { Text(failure.details, style = MaterialTheme.typography.labelSmall.copy(fontFamily = FontFamily.Monospace), color = MaterialTheme.colorScheme.onSurfaceVariant) }
    }
}

/** A full-page failure with a retry action (iOS FailureView). */
@Composable
fun FailureView(title: String, error: String, retryLabel: String = "Try again", forgetIdentity: (() -> Unit)? = null, retry: () -> Unit) {
    val failure = remember(error) { PresentedFailure(error) }
    var details by remember { mutableStateOf(false) }
    Column(Modifier.fillMaxWidth().padding(28.dp), horizontalAlignment = Alignment.CenterHorizontally, verticalArrangement = Arrangement.spacedBy(14.dp)) {
        IconTile(if (failure.blocked) Icons.Outlined.GppMaybe else Icons.Outlined.WifiOff, Modifier.size(56.dp))
        Text(failure.title ?: title, style = MaterialTheme.typography.titleLarge, textAlign = TextAlign.Center)
        Text(failure.message, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant, textAlign = TextAlign.Center)
        OutlinedButton(onClick = retry) { Text(retryLabel) }
        if (forgetIdentity != null) {
            TextButton(onClick = forgetIdentity) { Text("Forget saved identity and retry") }
            SecondaryText("Clears this device’s saved identity on this phone.")
        }
        TextButton(onClick = { details = true }) { Text("Technical details", style = MaterialTheme.typography.bodySmall) }
    }
    if (details) DetailsDialog("Connection details", failure.details) { details = false }
}

@Composable
fun DetailsDialog(title: String, text: String, dismiss: () -> Unit) {
    AlertDialog(onDismissRequest = dismiss, title = { Text(title) }, text = {
        SelectionContainer { Text(text, Modifier.heightIn(max = 360.dp).verticalScroll(rememberScrollState()), style = MaterialTheme.typography.bodySmall.copy(fontFamily = FontFamily.Monospace)) }
    }, confirmButton = { TextButton(onClick = dismiss) { Text("Done") } })
}

class MenuItem(val title: String, val icon: ImageVector? = null, val destructive: Boolean = false, val enabled: Boolean = true, val action: () -> Unit)

/** A "more" button that opens a menu (iOS OverflowMenu). */
@Composable
fun OverflowMenu(label: String, items: List<MenuItem>, icon: ImageVector = Icons.Outlined.MoreVert, enabled: Boolean = true) {
    var open by remember { mutableStateOf(false) }
    Box {
        IconButton(onClick = { open = true }, enabled = enabled && items.isNotEmpty(), modifier = Modifier.semantics { contentDescription = label }) { Icon(icon, null) }
        DropdownMenu(open, onDismissRequest = { open = false }) {
            items.forEach { item ->
                val color = if (item.destructive) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.onSurface
                DropdownMenuItem(text = { Text(item.title, color = color) }, enabled = item.enabled, leadingIcon = item.icon?.let { { Icon(it, null, tint = color) } },
                    onClick = { open = false; item.action() })
            }
        }
    }
}

/** A confirmation that names its action, with destructive styling where needed. */
@Composable
fun ConfirmAction(title: String, message: String, confirm: String, destructive: Boolean = true, onDismiss: () -> Unit, onConfirm: () -> Unit) {
    AlertDialog(onDismissRequest = onDismiss, title = { Text(title) }, text = { Text(message) },
        confirmButton = { TextButton(onClick = onConfirm) { Text(confirm, color = if (destructive) MaterialTheme.colorScheme.error else MaterialTheme.colorScheme.primary) } },
        dismissButton = { TextButton(onClick = onDismiss) { Text("Cancel") } })
}

@Composable
fun PageColumn(modifier: Modifier = Modifier, content: LazyListScope.() -> Unit) {
    androidx.compose.foundation.lazy.LazyColumn(modifier.fillMaxSize(), contentPadding = PaddingValues(start = 20.dp, end = 20.dp, top = 12.dp, bottom = 32.dp),
        verticalArrangement = Arrangement.spacedBy(18.dp), content = content)
}
