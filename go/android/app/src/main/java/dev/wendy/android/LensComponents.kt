package dev.wendy.android

import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.isSystemInDarkTheme
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.luminance
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp

// Building blocks of the iOS lens design (PR #2715: MobileLensStyle, LensIcon,
// LensSection, LensRow, LensHero), drawn with Material 3.

object LensStyle {
    val green = Color(0xFF2A7050)
    val robots = Color(0xFFFF9500)
    val sites = Color(0xFF4A5C7A)
    val online = Color(0xFF34C759)
    val offline = Color(0xFF8E8E93)
    val warning = Color(0xFFFF9500)
    val danger = Color(0xFFFF3B30)
    /** The lens accent: brand green, lightened in dark mode for contrast. It follows the app's appearance, not only the system's. */
    val accent: Color @Composable get() = if (isDark) Color(0xFF9FE2BF) else green
    /** True when the app draws dark, whether from the system or the person's Appearance choice. */
    val isDark: Boolean @Composable get() = MaterialTheme.colorScheme.background.luminance() < 0.5f
    /** A symbol color that stays visible on the card: near-black tints switch to the content color in dark mode. */
    @Composable fun visible(color: Color): Color = if (isDark && color.luminance() < 0.05f) MaterialTheme.colorScheme.onSurface else color
}

enum class Lens(val title: String, val icon: ImageVector, val color: Color, val choice: String, val summary: String) {
    Devices("Devices", Icons.Outlined.Memory, LensStyle.green, "I build with devices", "Pis, Jetsons and sensors: apps, logs, updates"),
    Robots("Robots", Icons.Outlined.SmartToy, LensStyle.robots, "I work on robots", "Go2, G1, arms, drones: health, missions, sim"),
    Sites("Sites", Icons.Outlined.Business, LensStyle.sites, "I run a site", "Rooms and warehouses: what’s happening, alerts");
    /** The lens name's color on its tile, lightened in dark mode where the brand color is too dim. */
    val textColor: Color @Composable get() = when (this) { Devices -> LensStyle.accent; Robots -> color; Sites -> if (LensStyle.isDark) Color(0xFFA9B9D9) else color }
    companion object { fun of(name: String?) = entries.firstOrNull { it.name == name } }
}

/** A white symbol on a rounded colored square. */
@Composable
fun LensIcon(icon: ImageVector, color: Color = LensStyle.green, size: Dp = 36.dp) {
    Box(Modifier.size(size).background(color, RoundedCornerShape(if (size > 40.dp) 16.dp else 10.dp)), contentAlignment = Alignment.Center) {
        Icon(icon, null, Modifier.size(size * 0.5f), tint = Color.White)
    }
}

/** An uppercase caption above a rounded card of rows. */
@Composable
fun LensSection(title: String?, modifier: Modifier = Modifier, content: @Composable ColumnScope.() -> Unit) {
    Column(modifier.fillMaxWidth(), verticalArrangement = Arrangement.spacedBy(6.dp)) {
        if (title != null) Text(title.uppercase(), Modifier.padding(start = 16.dp), style = MaterialTheme.typography.labelMedium, fontWeight = FontWeight.SemiBold,
            color = MaterialTheme.colorScheme.onSurfaceVariant)
        Surface(Modifier.fillMaxWidth(), shape = RoundedCornerShape(22.dp), color = MaterialTheme.colorScheme.surface) {
            Column(content = content)
        }
    }
}

@Composable
fun LensRow(title: String, subtitle: String = "", icon: ImageVector, color: Color = LensStyle.green, trailing: String = "", navigates: Boolean = true,
            enabled: Boolean = true, onClick: (() -> Unit)? = null) {
    Row(Modifier.fillMaxWidth().let { if (onClick != null) it.clickable(enabled = enabled, onClick = onClick) else it }.padding(horizontal = 16.dp, vertical = 10.dp),
        verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
        LensIcon(icon, if (enabled) color else LensStyle.offline)
        Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(2.dp)) {
            Text(title, style = MaterialTheme.typography.bodyLarge, fontWeight = FontWeight.SemiBold, maxLines = 2, overflow = TextOverflow.Ellipsis)
            if (subtitle.isNotEmpty()) Text(subtitle, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 2, overflow = TextOverflow.Ellipsis)
        }
        if (trailing.isNotEmpty()) Text(trailing, Modifier.widthIn(max = 96.dp), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis)
        if (navigates) Icon(Icons.Outlined.ChevronRight, null, Modifier.size(18.dp), tint = MaterialTheme.colorScheme.outline)
    }
}

@Composable fun LensDivider() = HorizontalDivider(Modifier.padding(start = 64.dp), color = MaterialTheme.colorScheme.outlineVariant)

/** The large identity card at the top of device and app pages. */
@Composable
fun LensHero(icon: ImageVector, color: Color, kicker: String, title: String, subtitle: String, chips: List<String> = emptyList(),
             avatar: (@Composable () -> Unit)? = null, actions: @Composable RowScope.() -> Unit = {}) {
    Surface(Modifier.fillMaxWidth(), shape = RoundedCornerShape(26.dp), color = MaterialTheme.colorScheme.surface) {
        Column(Modifier.padding(18.dp), verticalArrangement = Arrangement.spacedBy(14.dp)) {
            Row(horizontalArrangement = Arrangement.spacedBy(14.dp), verticalAlignment = Alignment.CenterVertically) {
                if (avatar != null) avatar() else LensIcon(icon, color, 54.dp)
                Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(3.dp)) {
                    Text(kicker, style = MaterialTheme.typography.labelSmall, fontWeight = FontWeight.Bold, color = color, maxLines = 1, letterSpacing = 0.4.sp)
                    Text(title, style = MaterialTheme.typography.headlineSmall, fontWeight = FontWeight.Bold, maxLines = 2, overflow = TextOverflow.Ellipsis)
                    Text(subtitle, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 2, overflow = TextOverflow.Ellipsis)
                }
            }
            if (chips.isNotEmpty()) Row(Modifier.horizontalScroll(rememberScrollState()), horizontalArrangement = Arrangement.spacedBy(6.dp)) {
                chips.forEach { Text(it, Modifier.background(MaterialTheme.colorScheme.surfaceVariant, CircleShape).padding(horizontal = 9.dp, vertical = 5.dp),
                    style = MaterialTheme.typography.labelMedium, fontWeight = FontWeight.SemiBold, maxLines = 1) }
            }
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp), content = actions)
        }
    }
}

/** The bordered and filled action buttons of the lens design. */
@Composable
fun LensButton(title: String, primary: Boolean = false, enabled: Boolean = true, modifier: Modifier = Modifier, onClick: () -> Unit) {
    if (primary) Button(onClick, modifier.heightIn(min = 46.dp), enabled = enabled, colors = ButtonDefaults.buttonColors(containerColor = LensStyle.green, contentColor = Color.White)) { Text(title) }
    else OutlinedButton(onClick, modifier.heightIn(min = 46.dp), enabled = enabled, border = BorderStroke(1.dp, LensStyle.green.copy(alpha = .5f)),
        colors = ButtonDefaults.outlinedButtonColors(contentColor = LensStyle.accent)) { Text(title) }
}

/** An iOS-style segmented picker. */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun <T> LensSegments(options: List<T>, selected: T, label: (T) -> String, modifier: Modifier = Modifier, onSelect: (T) -> Unit) {
    SingleChoiceSegmentedButtonRow(modifier.fillMaxWidth()) {
        options.forEachIndexed { index, option ->
            SegmentedButton(selected = option == selected, onClick = { onSelect(option) }, shape = SegmentedButtonDefaults.itemShape(index, options.size),
                icon = {}, colors = SegmentedButtonDefaults.colors(activeContainerColor = MaterialTheme.colorScheme.surface, activeContentColor = MaterialTheme.colorScheme.onSurface,
                    inactiveContainerColor = MaterialTheme.colorScheme.surfaceVariant, inactiveContentColor = MaterialTheme.colorScheme.onSurfaceVariant,
                    activeBorderColor = MaterialTheme.colorScheme.outline, inactiveBorderColor = MaterialTheme.colorScheme.outlineVariant)) {
                Text(label(option), maxLines = 1, style = MaterialTheme.typography.labelMedium, fontWeight = if (option == selected) FontWeight.SemiBold else FontWeight.Normal)
            }
        }
    }
}

/** A placeholder card shown while a section loads (iOS MobileLoadingPlaceholder). */
@Composable
fun LensLoading(label: String) {
    Row(Modifier.fillMaxWidth().padding(16.dp), horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
        CircularProgressIndicator(Modifier.size(18.dp), strokeWidth = 2.dp, color = LensStyle.green)
        Text(label, style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant)
    }
}
