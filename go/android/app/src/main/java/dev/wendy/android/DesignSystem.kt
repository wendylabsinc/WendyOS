package dev.wendy.android

import androidx.compose.animation.animateColorAsState
import androidx.compose.animation.core.animateFloatAsState
import androidx.compose.animation.core.tween
import androidx.compose.foundation.*
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.geometry.Size
import androidx.compose.ui.graphics.*
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.graphics.drawscope.withTransform
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.semantics.*
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp

val WendyInk = Color(0xFF102D28)
val WendyTeal = Color(0xFF087A66)
val WendyLime = Color(0xFFD3F58B)
val WendyMint = Color(0xFFB6DCCF)

@Composable
fun WendyTheme(content: @Composable () -> Unit) {
    val dark = isSystemInDarkTheme()
    val colors = if (dark) darkColorScheme(
        primary = WendyLime, onPrimary = WendyInk, primaryContainer = Color(0xFF24483D), onPrimaryContainer = WendyLime,
        secondary = WendyMint, onSecondary = WendyInk, secondaryContainer = Color(0xFF263C34), onSecondaryContainer = Color(0xFFDDEBE2),
        background = Color(0xFF101B18), onBackground = Color(0xFFEAF0E7), surface = Color(0xFF182620), onSurface = Color(0xFFEAF0E7),
        surfaceVariant = Color(0xFF293830), onSurfaceVariant = Color(0xFFAABCB0), surfaceContainer = Color(0xFF1C2B24),
        surfaceContainerLow = Color(0xFF182620), surfaceContainerHigh = Color(0xFF26372E), surfaceContainerHighest = Color(0xFF304237),
        outline = Color(0xFF64786A), outlineVariant = Color(0xFF35473B), error = Color(0xFFFFB4AB)
    ) else lightColorScheme(
        primary = WendyTeal, onPrimary = Color.White, primaryContainer = WendyLime, onPrimaryContainer = WendyInk,
        secondary = Color(0xFF4F6C5B), onSecondary = Color.White, secondaryContainer = Color(0xFFE5EDDD), onSecondaryContainer = WendyInk,
        background = Color(0xFFF4F5EF), onBackground = WendyInk, surface = Color(0xFFFEFFFA), onSurface = WendyInk,
        surfaceVariant = Color(0xFFE8EDE3), onSurfaceVariant = Color(0xFF647369), surfaceContainer = Color(0xFFEEF1E8),
        surfaceContainerLow = Color(0xFFF9FAF4), surfaceContainerHigh = Color(0xFFE7EDE1), surfaceContainerHighest = Color(0xFFDCE5D8),
        outline = Color(0xFF89978A), outlineVariant = Color(0xFFDFE5D9), error = Color(0xFFAE352C)
    )
    val type = Typography(
        displaySmall = TextStyle(fontSize = 38.sp, lineHeight = 42.sp, fontWeight = FontWeight.SemiBold, letterSpacing = (-1.7).sp),
        headlineLarge = TextStyle(fontSize = 32.sp, lineHeight = 36.sp, fontWeight = FontWeight.SemiBold, letterSpacing = (-1.1).sp),
        headlineMedium = TextStyle(fontSize = 28.sp, lineHeight = 33.sp, fontWeight = FontWeight.SemiBold, letterSpacing = (-0.7).sp),
        headlineSmall = TextStyle(fontSize = 24.sp, lineHeight = 29.sp, fontWeight = FontWeight.SemiBold, letterSpacing = (-0.5).sp),
        titleLarge = TextStyle(fontSize = 21.sp, lineHeight = 26.sp, fontWeight = FontWeight.SemiBold, letterSpacing = (-0.4).sp),
        titleMedium = TextStyle(fontSize = 16.sp, lineHeight = 22.sp, fontWeight = FontWeight.SemiBold),
        titleSmall = TextStyle(fontSize = 14.sp, lineHeight = 20.sp, fontWeight = FontWeight.SemiBold),
        bodyLarge = TextStyle(fontSize = 16.sp, lineHeight = 24.sp),
        bodyMedium = TextStyle(fontSize = 14.sp, lineHeight = 21.sp),
        bodySmall = TextStyle(fontSize = 12.sp, lineHeight = 18.sp),
        labelLarge = TextStyle(fontSize = 14.sp, lineHeight = 20.sp, fontWeight = FontWeight.SemiBold),
        labelMedium = TextStyle(fontSize = 12.sp, lineHeight = 16.sp, fontWeight = FontWeight.Medium, letterSpacing = 0.3.sp),
        labelSmall = TextStyle(fontSize = 10.sp, lineHeight = 14.sp, fontWeight = FontWeight.SemiBold, letterSpacing = 0.8.sp)
    )
    MaterialTheme(colorScheme = colors, typography = type, shapes = Shapes(
        extraSmall = RoundedCornerShape(8.dp), small = RoundedCornerShape(12.dp), medium = RoundedCornerShape(18.dp),
        large = RoundedCornerShape(24.dp), extraLarge = RoundedCornerShape(30.dp)
    ), content = content)
}

@Composable
fun WendyMark(modifier: Modifier = Modifier, color: Color = MaterialTheme.colorScheme.primary) {
    Canvas(modifier) {
        val scale = minOf(size.width / 247f, size.height / 182f)
        withTransform({ translate((size.width - 247 * scale) / 2, (size.height - 182 * scale) / 2); scale(scale, scale, Offset.Zero) }) {
            val ring = Path().apply {
                fillType = PathFillType.EvenOdd
                moveTo(70f, 21f); lineTo(140f, 91f); lineTo(70f, 161f); lineTo(0f, 91f); close()
                moveTo(70f, 44f); lineTo(117f, 91f); lineTo(70f, 138f); lineTo(23f, 91f); close()
            }
            drawPath(ring, color)
            drawPath(Path().apply { moveTo(156f, 0f); lineTo(247f, 91f); lineTo(156f, 182f); lineTo(65f, 91f); close() }, color)
        }
    }
}

/** Decorative device drawing. It does not imply live topology or connection state. */
@Composable
fun DeviceIllustration(modifier: Modifier = Modifier, accent: Color = WendyLime) {
    Canvas(modifier.clearAndSetSemantics { }) {
        val s = minOf(size.width, size.height) / 200f
        withTransform({ translate((size.width - 200 * s) / 2, (size.height - 200 * s) / 2); scale(s, s, Offset.Zero) }) {
            drawCircle(accent.copy(alpha = 0.07f), 96f, Offset(100f, 100f))
            drawCircle(accent.copy(alpha = 0.14f), 76f, Offset(100f, 100f), style = Stroke(0.8f))
            drawCircle(accent.copy(alpha = 0.12f), 96f, Offset(100f, 100f), style = Stroke(0.8f))
            fun face(vararg xy: Float): Path = Path().apply { moveTo(xy[0], xy[1]); for (i in 2 until xy.size step 2) lineTo(xy[i], xy[i+1]); close() }
            val top = face(100f, 45f, 157f, 76f, 100f, 108f, 43f, 76f)
            val left = face(43f, 76f, 100f, 108f, 100f, 156f, 43f, 124f)
            val right = face(100f, 108f, 157f, 76f, 157f, 124f, 100f, 156f)
            drawPath(left, Color(0xFF1D5546)); drawPath(right, Color(0xFF34705B)); drawPath(top, Color(0xFF548B70))
            listOf(top, left, right).forEach { drawPath(it, accent.copy(alpha = 0.55f), style = Stroke(1f)) }
            for (i in 0..4) {
                val x = 57f + i * 8
                drawLine(accent.copy(alpha = 0.45f), Offset(x, 94f + i*4.5f), Offset(x, 119f + i*4.5f), 2f, StrokeCap.Round)
            }
            drawPath(face(100f, 65f, 123f, 78f, 100f, 91f, 77f, 78f), accent)
            drawCircle(accent, 3f, Offset(143f, 113f))
            drawLine(accent.copy(alpha = .5f), Offset(100f, 156f), Offset(100f, 183f), 1f)
            drawCircle(accent, 3.5f, Offset(100f, 183f))
            drawCircle(accent, 2.5f, Offset(27f, 56f))
            drawCircle(accent.copy(alpha = .6f), 2.5f, Offset(177f, 61f))
        }
    }
}

@Composable
fun StatusPill(text: String, modifier: Modifier = Modifier, dark: Boolean = false, active: Boolean = true) {
    val color = if (dark) WendyLime else if (active) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurfaceVariant
    Row(modifier.clip(CircleShape).background(color.copy(alpha = if (dark) .12f else .09f)).padding(horizontal = 10.dp, vertical = 6.dp),
        horizontalArrangement = Arrangement.spacedBy(6.dp), verticalAlignment = Alignment.CenterVertically) {
        Box(Modifier.size(5.dp).background(color, CircleShape))
        Text(text, color = color, style = MaterialTheme.typography.labelMedium)
    }
}

@Composable
fun Eyebrow(text: String, color: Color = MaterialTheme.colorScheme.onSurfaceVariant) {
    Text(text.uppercase(), style = MaterialTheme.typography.labelSmall, color = color)
}

@Composable
fun IconTile(icon: ImageVector, modifier: Modifier = Modifier, accent: Color = MaterialTheme.colorScheme.primary) {
    Box(modifier.size(46.dp).clip(RoundedCornerShape(15.dp)).background(accent.copy(alpha = .09f)), contentAlignment = Alignment.Center) {
        Icon(icon, null, tint = accent, modifier = Modifier.size(23.dp))
    }
}

@Composable
fun ActionTile(title: String, subtitle: String, icon: ImageVector, modifier: Modifier = Modifier, onClick: () -> Unit) {
    Surface(onClick = onClick, modifier = modifier, color = MaterialTheme.colorScheme.surface, shape = RoundedCornerShape(22.dp), border = BorderStroke(1.dp, MaterialTheme.colorScheme.outlineVariant)) {
        Column(Modifier.padding(16.dp), verticalArrangement = Arrangement.spacedBy(6.dp)) {
            Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween, verticalAlignment = Alignment.CenterVertically) {
                Icon(icon, null, Modifier.size(22.dp), tint = MaterialTheme.colorScheme.primary)
                Icon(Icons.Outlined.NorthEast, null, Modifier.size(16.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
            }
            Spacer(Modifier.height(5.dp))
            Text(title, style = MaterialTheme.typography.titleSmall)
            Text(subtitle, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}

@Composable
fun SettingsRow(title: String, subtitle: String, icon: ImageVector, onClick: () -> Unit) {
    Surface(onClick = onClick, shape = RoundedCornerShape(20.dp), color = MaterialTheme.colorScheme.surface,
        border = BorderStroke(1.dp, MaterialTheme.colorScheme.outlineVariant), modifier = Modifier.fillMaxWidth()) {
        Row(Modifier.padding(14.dp), horizontalArrangement = Arrangement.spacedBy(14.dp), verticalAlignment = Alignment.CenterVertically) {
            IconTile(icon)
            Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(3.dp)) {
                Text(title, style = MaterialTheme.typography.titleMedium)
                if (subtitle.isNotBlank()) Text(subtitle, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
            }
            Icon(Icons.Outlined.ChevronRight, null, Modifier.size(20.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
        }
    }
}

@Composable
fun ChoiceTabs(choices: List<String>, selected: String, onSelect: (String) -> Unit, modifier: Modifier = Modifier) {
    Row(modifier.clip(RoundedCornerShape(16.dp)).background(MaterialTheme.colorScheme.surfaceVariant).padding(4.dp), horizontalArrangement = Arrangement.spacedBy(4.dp)) {
        choices.forEach { choice ->
            val active = choice == selected
            val color by animateColorAsState(if (active) MaterialTheme.colorScheme.surface else Color.Transparent, label = "tab")
            Surface(onClick = { onSelect(choice) }, color = color, shape = RoundedCornerShape(12.dp), modifier = Modifier.weight(1f).semantics { this.selected = active }) {
                Text(choice, Modifier.padding(vertical = 12.dp, horizontal = 12.dp), textAlign = androidx.compose.ui.text.style.TextAlign.Center,
                    style = MaterialTheme.typography.labelLarge, color = if (active) MaterialTheme.colorScheme.onSurface else MaterialTheme.colorScheme.onSurfaceVariant)
            }
        }
    }
}

@Composable
fun DeviceCard(device: Device, connected: Boolean = false, connecting: Boolean = false, onClick: () -> Unit) {
    Surface(onClick = onClick, color = MaterialTheme.colorScheme.surface, shape = RoundedCornerShape(23.dp),
        border = BorderStroke(1.dp, MaterialTheme.colorScheme.outlineVariant), modifier = Modifier.fillMaxWidth()) {
        Column(Modifier.padding(18.dp), verticalArrangement = Arrangement.spacedBy(16.dp)) {
            Row(horizontalArrangement = Arrangement.spacedBy(14.dp), verticalAlignment = Alignment.CenterVertically) {
                IconTile(if (device.lite) Icons.Outlined.Sensors else Icons.Outlined.Memory)
                Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                    Text(device.title, style = MaterialTheme.typography.titleMedium, maxLines = 2, overflow = TextOverflow.Ellipsis)
                    Text(if (connected) "Connected" else if (device.lite) "Wendy Lite" else "Wendy device", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                }
                when {
                    connecting -> CircularProgressIndicator(Modifier.size(21.dp), strokeWidth = 2.dp)
                    connected -> Icon(Icons.Outlined.CheckCircle, "Connected", tint = MaterialTheme.colorScheme.primary, modifier = Modifier.size(21.dp))
                    else -> Icon(Icons.Outlined.NorthEast, null, tint = MaterialTheme.colorScheme.primary, modifier = Modifier.size(21.dp))
                }
            }
            HorizontalDivider(color = MaterialTheme.colorScheme.outlineVariant)
            Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                Text(if (device.selector.startsWith("cloud://")) "Remote connection" else device.selector, modifier = Modifier.weight(1f), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis)
                Surface(color = MaterialTheme.colorScheme.surfaceVariant, shape = CircleShape) {
                    Text(device.transport.ifBlank { "Device" }, Modifier.padding(horizontal = 10.dp, vertical = 5.dp), style = MaterialTheme.typography.labelMedium)
                }
            }
        }
    }
}

@Composable
fun MetricTile(label: String, value: String, detail: String, fraction: Float?, modifier: Modifier = Modifier, icon: ImageVector = Icons.Outlined.Memory) {
    val progress by animateFloatAsState(fraction?.coerceIn(0f, 1f) ?: 0f, tween(600), label = label)
    Surface(modifier, color = MaterialTheme.colorScheme.surface, shape = RoundedCornerShape(23.dp), border = BorderStroke(1.dp, MaterialTheme.colorScheme.outlineVariant)) {
        Column(Modifier.padding(18.dp), verticalArrangement = Arrangement.spacedBy(8.dp)) {
            Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween, verticalAlignment = Alignment.CenterVertically) {
                Eyebrow(label)
                Icon(icon, null, Modifier.size(18.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
            }
            Text(value, style = MaterialTheme.typography.headlineMedium, maxLines = 1, overflow = TextOverflow.Ellipsis)
            if (fraction != null) LinearProgressIndicator(progress = { progress }, modifier = Modifier.fillMaxWidth().height(5.dp).clip(CircleShape), color = MaterialTheme.colorScheme.primary, trackColor = MaterialTheme.colorScheme.surfaceVariant, drawStopIndicator = {})
            Text(detail, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis)
        }
    }
}

fun formatBytes(bytes: Long): String = when {
    bytes >= 1L shl 30 -> "%.1f GB".format(bytes / (1L shl 30).toDouble())
    bytes >= 1L shl 20 -> "%.0f MB".format(bytes / (1L shl 20).toDouble())
    bytes >= 1L shl 10 -> "%.0f KB".format(bytes / 1024.0)
    else -> "$bytes B"
}
