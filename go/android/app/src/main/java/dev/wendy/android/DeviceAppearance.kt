package dev.wendy.android

import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.net.Uri
import android.util.Base64
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.result.PickVisualMediaRequest
import androidx.activity.result.contract.ActivityResultContracts
import androidx.compose.foundation.Image
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
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
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.asImageBitmap
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.layout.ContentScale
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import kotlinx.coroutines.withContext
import org.json.JSONObject
import java.io.ByteArrayOutputStream
import java.security.MessageDigest

/** A person's own icon for a device: a symbol, emoji or photo on a color (iOS DeviceAppearance). */
data class DeviceAppearanceValue(val kind: String = "symbol", val symbol: String = "cpu", val emoji: String = "🤖", val color: String = "green", val photo: String? = null) {
    val tint: Color get() = colors[color] ?: LensStyle.green
    fun json(): JSONObject = JSONObject().put("kind", kind).put("symbol", symbol).put("emoji", emoji).put("color", color).also { if (photo != null) it.put("photo", photo) }
    companion object {
        // Symbol names follow the iOS SF Symbols list so appearances read the same on both.
        val symbols = linkedMapOf("cpu" to Icons.Outlined.Memory, "desktopcomputer" to Icons.Outlined.DesktopWindows, "server.rack" to Icons.Outlined.Dns, "sensor" to Icons.Outlined.Sensors,
            "camera.fill" to Icons.Outlined.PhotoCamera, "video.fill" to Icons.Outlined.Videocam, "car.fill" to Icons.Outlined.DirectionsCar, "airplane" to Icons.Outlined.Flight,
            "bolt.fill" to Icons.Outlined.Bolt, "leaf.fill" to Icons.Outlined.Eco, "house.fill" to Icons.Outlined.Home, "star.fill" to Icons.Outlined.Star)
        val emojis = listOf("🤖", "🦾", "🚀", "🛰️", "🛸", "🐶", "🐱", "🦊", "🐙", "🐝", "🌱", "⚡️", "🔬", "🎥", "🏠", "⭐️")
        val colors = linkedMapOf("green" to LensStyle.green, "blue" to Color(0xFF007AFF), "purple" to Color(0xFFAF52DE), "pink" to Color(0xFFFF2D55), "orange" to Color(0xFFFF9500), "slate" to Color(0xFF52616E))
        fun parse(value: JSONObject) = DeviceAppearanceValue(value.optString("kind", "symbol"), value.optString("symbol", "cpu"), value.optString("emoji", "🤖"), value.optString("color", "green"),
            value.optString("photo").ifEmpty { null })
        /** Length-prefixed account, organization and device identity, hashed (iOS DeviceAppearanceStore.key). */
        fun key(account: String, organization: String, device: Device): String {
            val value = listOf(account, organization, widgetDeviceID(device)).joinToString("") { "${it.toByteArray().size}:$it" }
            return MessageDigest.getInstance("SHA-256").digest(value.toByteArray()).joinToString("") { "%02x".format(it) }
        }
    }
}

/** Device appearances kept on this phone only. */
object DeviceAppearances {
    private var revision by mutableIntStateOf(0)
    private val cache = mutableMapOf<String, DeviceAppearanceValue?>()
    fun get(model: WendyModel, device: Device): DeviceAppearanceValue? {
        revision.let { }
        val key = DeviceAppearanceValue.key(model.account, model.organization, device)
        return cache.getOrPut(key) {
            model.getApplication<WendyApplication>().getSharedPreferences("device-appearance", 0).getString(key, null)?.let { runCatching { DeviceAppearanceValue.parse(JSONObject(it)) }.getOrNull() }
        }
    }
    fun save(model: WendyModel, device: Device, value: DeviceAppearanceValue?) {
        val key = DeviceAppearanceValue.key(model.account, model.organization, device)
        val preferences = model.getApplication<WendyApplication>().getSharedPreferences("device-appearance", 0).edit()
        if (value == null) preferences.remove(key) else preferences.putString(key, value.json().toString())
        preferences.apply()
        cache[key] = value; revision++
    }
}

@Composable
fun DeviceAvatar(model: WendyModel, device: Device, size: Dp = 46.dp, fallback: Color = LensStyle.green) {
    AppearancePreview(DeviceAppearances.get(model, device), device.lite, size, fallback)
}

@Composable
private fun AppearancePreview(value: DeviceAppearanceValue?, lite: Boolean, size: Dp, fallback: Color = LensStyle.green) {
    val shape = RoundedCornerShape(size * 0.28f)
    val photo = remember(value?.photo) { value?.photo?.let { runCatching { Base64.decode(it, Base64.DEFAULT).let { b -> BitmapFactory.decodeByteArray(b, 0, b.size) } }.getOrNull() } }
    Box(Modifier.size(size).clip(shape).background(value?.tint ?: fallback), contentAlignment = Alignment.Center) {
        when {
            value?.kind == "photo" && photo != null -> Image(photo.asImageBitmap(), null, Modifier.fillMaxSize(), contentScale = ContentScale.Crop)
            value?.kind == "emoji" -> Text(value.emoji, fontSize = (size.value * 0.5f).sp)
            else -> Icon(value?.let { DeviceAppearanceValue.symbols[it.symbol] } ?: if (lite) Icons.Outlined.Sensors else Icons.Outlined.Memory, null, Modifier.size(size * 0.5f), tint = Color.White)
        }
    }
}

/** Choose a photo, emoji or symbol and color for a device (iOS MobileDeviceAppearanceEditor). */
@Composable
fun DeviceAppearanceEditor(model: WendyModel, device: Device) {
    val nav = LocalNavigator.current
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    val saved = DeviceAppearances.get(model, device)
    var draft by remember { mutableStateOf(saved ?: DeviceAppearanceValue()) }
    var isDefault by remember { mutableStateOf(saved == null) }
    var error by remember { mutableStateOf<String?>(null) }
    val picker = rememberLauncherForActivityResult(ActivityResultContracts.PickVisualMedia()) { uri: Uri? ->
        if (uri == null) return@rememberLauncherForActivityResult
        scope.launch {
            val encoded = withContext(Dispatchers.IO) { runCatching { squarePhoto(context, uri) }.getOrNull() }
            if (encoded == null) error = "This photo could not be read. Choose another photo."
            else { draft = draft.copy(kind = "photo", photo = encoded); isDefault = false; error = null }
        }
    }
    PageColumn {
        item { Box(Modifier.fillMaxWidth(), contentAlignment = Alignment.Center) { AppearancePreview(if (isDefault) null else draft, device.lite, 96.dp) } }
        formSection("Photo") { ActionRow("Choose a photo", Icons.Outlined.Image) { picker.launch(PickVisualMediaRequest(ActivityResultContracts.PickVisualMedia.ImageOnly)) } }
        formSection("Emoji") {
            DeviceAppearanceValue.emojis.chunked(8).forEach { row ->
                Row { row.forEach { emoji ->
                    Box(Modifier.weight(1f).height(48.dp).clip(RoundedCornerShape(12.dp)).background(if (!isDefault && draft.kind == "emoji" && draft.emoji == emoji) draft.tint.copy(alpha = .2f) else Color.Transparent)
                        .clickable { draft = draft.copy(kind = "emoji", emoji = emoji, photo = null); isDefault = false }, contentAlignment = Alignment.Center) { Text(emoji, fontSize = 26.sp) }
                } }
            }
        }
        formSection("Symbol") {
            DeviceAppearanceValue.symbols.entries.chunked(6).forEach { row ->
                Row { row.forEach { (name, icon) ->
                    Box(Modifier.weight(1f).height(48.dp).clip(RoundedCornerShape(12.dp)).background(if (!isDefault && draft.kind == "symbol" && draft.symbol == name) draft.tint.copy(alpha = .2f) else Color.Transparent)
                        .clickable { draft = draft.copy(kind = "symbol", symbol = name, photo = null); isDefault = false }, contentAlignment = Alignment.Center) { Icon(icon, name, tint = draft.tint) }
                } }
            }
        }
        formSection("Color") {
            Row(Modifier.padding(vertical = 8.dp), horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                DeviceAppearanceValue.colors.forEach { (name, color) ->
                    Box(Modifier.size(32.dp).clip(CircleShape).background(color).clickable { draft = draft.copy(color = name); isDefault = false }, contentAlignment = Alignment.Center) {
                        if (draft.color == name && !isDefault) Icon(Icons.Outlined.Check, name, Modifier.size(18.dp), tint = Color.White)
                    }
                }
            }
        }
        error?.let { item { FormSection { FailureNote(it) } } }
        item {
            Row(horizontalArrangement = Arrangement.spacedBy(8.dp)) {
                LensButton("Use default", modifier = Modifier.weight(1f)) { DeviceAppearances.save(model, device, null); nav.pop() }
                LensButton("Save", primary = true, enabled = !isDefault, modifier = Modifier.weight(1f)) { DeviceAppearances.save(model, device, draft); nav.pop() }
            }
        }
        item { SecondaryText("Appearances stay on this phone.") }
    }
}

/** A center-cropped 256-pixel JPEG, small enough to keep with the appearance (iOS keeps under 600 KB). */
private fun squarePhoto(context: android.content.Context, uri: Uri): String {
    val bitmap = context.contentResolver.openInputStream(uri).use { BitmapFactory.decodeStream(it) } ?: error("unreadable")
    val side = minOf(bitmap.width, bitmap.height)
    val square = Bitmap.createBitmap(bitmap, (bitmap.width - side) / 2, (bitmap.height - side) / 2, side, side)
    val scaled = Bitmap.createScaledBitmap(square, 256, 256, true)
    val out = ByteArrayOutputStream(); scaled.compress(Bitmap.CompressFormat.JPEG, 85, out)
    return Base64.encodeToString(out.toByteArray(), Base64.NO_WRAP)
}
