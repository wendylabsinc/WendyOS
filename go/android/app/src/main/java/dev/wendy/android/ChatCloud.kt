package dev.wendy.android

import android.content.Context
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.text.format.DateUtils
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.alpha
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.launch
import org.json.JSONArray
import org.json.JSONObject
import java.io.File
import java.security.KeyStore
import java.security.MessageDigest
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec

enum class ChatInference(val title: String) { Device("On device"), Cloud("Cloud") }
enum class ChatCloudProvider(val raw: String, val title: String) { OpenAI("openai", "OpenAI"), Anthropic("anthropic", "Anthropic") }

/** Provider API keys. They stay on this phone and are read only to list models or send a turn (iOS ChatCredentialStore). */
interface ChatCredentialStore {
    fun read(provider: ChatCloudProvider): String?
    fun save(key: String, provider: ChatCloudProvider)
    fun remove(provider: ChatCloudProvider)
}

/** Validates a pasted key the way iOS ChatKeychain.save does. */
internal fun validCloudKey(key: String): String {
    val value = key.trim()
    require(value.isNotEmpty() && value.toByteArray().size <= 8192 && value.none { it == '\n' || it == '\r' }) { "Enter a valid API key." }
    return value
}

/**
 * Keys encrypted with an Android Keystore AES key that cannot leave the phone and only works while it is
 * unlocked (iOS kSecAttrAccessibleWhenUnlockedThisDeviceOnly). The ciphertext lives in no-backup storage.
 */
class ChatKeystore(context: Context) : ChatCredentialStore {
    private val directory = File(context.noBackupFilesDir, "ask-wendy").apply { mkdirs() }
    private fun file(provider: ChatCloudProvider) = File(directory, provider.raw + ".key")
    private fun key(): SecretKey {
        val store = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
        (store.getKey(ALIAS, null) as? SecretKey)?.let { return it }
        val generator = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, "AndroidKeyStore")
        generator.init(KeyGenParameterSpec.Builder(ALIAS, KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT)
            .setBlockModes(KeyProperties.BLOCK_MODE_GCM).setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE).setKeySize(256)
            .setUnlockedDeviceRequired(true).build())
        return generator.generateKey()
    }
    override fun read(provider: ChatCloudProvider): String? {
        val sealed = file(provider).takeIf { it.exists() }?.readBytes() ?: return null
        try {
            require(sealed.size > 12)
            val cipher = Cipher.getInstance("AES/GCM/NoPadding").apply { init(Cipher.DECRYPT_MODE, key(), GCMParameterSpec(128, sealed, 0, 12)) }
            return String(cipher.doFinal(sealed, 12, sealed.size - 12), Charsets.UTF_8)
        } catch (e: Exception) { throw IllegalStateException("Could not read the API key on this phone. Unlock the phone, or remove and add the key again.", e) }
    }
    override fun save(key: String, provider: ChatCloudProvider) {
        val value = validCloudKey(key)
        try {
            val cipher = Cipher.getInstance("AES/GCM/NoPadding").apply { init(Cipher.ENCRYPT_MODE, key()) }
            val sealed = cipher.iv + cipher.doFinal(value.toByteArray(Charsets.UTF_8))
            val temporary = File(directory, provider.raw + ".tmp")
            temporary.writeBytes(sealed)
            check(temporary.renameTo(file(provider)))
        } catch (e: Exception) { throw IllegalStateException("Could not save the API key on this phone.", e) }
    }
    override fun remove(provider: ChatCloudProvider) {
        val target = file(provider)
        check(!target.exists() || target.delete()) { "Could not remove the API key from this phone." }
    }
    private companion object { const val ALIAS = "dev.wendy.ask-wendy.api-key" }
}

data class RememberedAIModel(val id: String, val name: String, val port: Long)
data class RememberedAIDevice(val id: String, val name: String, val models: List<RememberedAIModel>, val checked: Long, val lastUsed: Long? = null, val lastModel: String? = null) {
    val preferred get() = models.firstOrNull { it.id == lastModel } ?: models.firstOrNull()
    fun json(): JSONObject = JSONObject().put("id", id).put("name", name).put("checked", checked)
        .put("models", JSONArray(models.map { JSONObject().put("id", it.id).put("name", it.name).put("port", it.port) }))
        .also { o -> lastUsed?.let { o.put("lastUsed", it) }; lastModel?.let { o.put("lastModel", it) } }
    companion object {
        fun parse(value: JSONObject) = RememberedAIDevice(value.getString("id"), value.optString("name"),
            value.optJSONArray("models")?.let { a -> (0 until a.length()).mapNotNull { a.optJSONObject(it) }.map { RememberedAIModel(it.getString("id"), it.optString("name"), it.optLong("port")) } }.orEmpty(),
            value.optLong("checked"), if (value.has("lastUsed")) value.optLong("lastUsed") else null, value.optString("lastModel").ifEmpty { null })
    }
}

/** Devices where Wendy verified a usable model. Only discovery metadata is kept, never prompts, keys or endpoints (iOS RememberedAIDevices). */
class RememberedAIDevices(context: Context) {
    private val preferences = context.getSharedPreferences("ai-devices", Context.MODE_PRIVATE)
    var revision by mutableIntStateOf(0); private set
    fun records(account: String, organization: String): List<RememberedAIDevice> {
        revision.let { }
        val text = preferences.getString(key(account, organization), null) ?: return emptyList()
        return runCatching { JSONArray(text).let { a -> (0 until a.length()).map { RememberedAIDevice.parse(a.getJSONObject(it)) } } }.getOrDefault(emptyList())
    }
    fun discovered(device: Device, models: List<ChatModelOption>, account: String, organization: String, now: Long = System.currentTimeMillis()) {
        val id = recordingDeviceID(device)
        val rows = records(account, organization).toMutableList()
        val previous = rows.firstOrNull { it.id == id }
        rows.removeAll { it.id == id }
        val usable = models.filter { it.unavailableReason.isEmpty() }.take(40).map { RememberedAIModel(it.id, it.displayName, it.port) }
        if (usable.isNotEmpty()) rows += RememberedAIDevice(id, device.title, usable, now, previous?.lastUsed, previous?.lastModel)
        persist(rows, account, organization)
    }
    fun used(deviceID: String, model: String, account: String, organization: String, now: Long = System.currentTimeMillis()) {
        val rows = records(account, organization).toMutableList()
        val index = rows.indexOfFirst { it.id == deviceID }
        if (index < 0 || rows[index].models.none { it.id == model }) return
        rows[index] = rows[index].copy(lastUsed = now, lastModel = model)
        persist(rows, account, organization)
    }
    private fun persist(rows: List<RememberedAIDevice>, account: String, organization: String) {
        val kept = rows.sortedByDescending { it.lastUsed ?: it.checked }.take(50)
        preferences.edit().putString(key(account, organization), JSONArray(kept.map { it.json() }).toString()).apply()
        revision++
    }
    companion object {
        fun key(account: String, organization: String): String =
            "wendy.aiDevices.v1." + MessageDigest.getInstance("SHA-256").digest(JSONArray(listOf(account, organization)).toString().toByteArray()).joinToString("") { "%02x".format(it) }
    }
}

/** Provider, API key and model for cloud inference (iOS ChatCloudSettings). */
@Composable
fun ChatCloudSettingsPage(chat: DeviceChat) {
    val scope = rememberCoroutineScope()
    var apiKey by remember { mutableStateOf("") }
    LaunchedEffect(Unit) { chat.refreshCloudKey() }
    DisposableEffect(Unit) { onDispose { apiKey = "" } }
    PageColumn {
        formSection("Provider") {
            ChoiceField("Cloud provider", ChatCloudProvider.entries.map { it.name to it.title }, chat.cloudProvider.name, enabled = !chat.loading && !chat.streaming) {
                chat.selectProvider(ChatCloudProvider.valueOf(it)); apiKey = ""
            }
        }
        formSection("Your API key") {
            SecondaryText(if (chat.hasCloudKey) "An API key is saved securely on this phone." else "Add your own provider API key to find available models.")
            OutlinedTextField(apiKey, { apiKey = it }, Modifier.fillMaxWidth(), label = { Text(if (chat.hasCloudKey) "Replacement API key" else "API key") }, singleLine = true,
                visualTransformation = PasswordVisualTransformation(), keyboardOptions = KeyboardOptions(autoCorrectEnabled = false, keyboardType = KeyboardType.Password))
            ActionRow(if (chat.hasCloudKey) "Replace key & find models" else "Save key & find models", enabled = apiKey.isNotBlank() && !chat.loading && !chat.streaming) {
                val key = apiKey; apiKey = ""
                scope.launch { if (chat.saveCloudKey(key)) chat.discover() }
            }
            if (chat.hasCloudKey) ActionRow("Remove API key", destructive = true, enabled = !chat.loading && !chat.streaming) { chat.removeCloudKey(); apiKey = "" }
        }
        formSection("Model") {
            if (chat.loading) LoadingRow("Finding models…")
            ChoiceField("Model", listOf("" to "Choose a model") + chat.models.map { it.id to it.displayName }, chat.selected, enabled = !chat.loading && !chat.streaming) { chat.choose(it) }
            ActionRow("Refresh models", enabled = chat.hasCloudKey && !chat.loading && !chat.streaming) { scope.launch { chat.discover() } }
            chat.error?.let { Text(it, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium) }
        }
        formSection("Cloud inference") {
            Text("Your messages and device information used to answer them—including requested logs and tool results—are sent to ${chat.cloudProvider.title}. Device changes still require confirmation.",
                Modifier.padding(vertical = 6.dp))
            SecondaryText("API usage is billed by your provider. A chat subscription is not automatically API access. Keys stay on this phone and are not sent to your Wendy device. Switching inference source starts a new chat.")
        }
    }
}

/** Remembered devices with verified models, available ones first (iOS MobileAIDeviceSuggestions). */
@Composable
fun AIDeviceSuggestions(model: WendyModel, remembered: RememberedAIDevices, excludedID: String?, chooseDevice: () -> Unit, select: (RememberedAIDevice, Device) -> Unit) {
    val devices = (model.nearbyDevices + model.cloudDevices).distinctBy { recordingDeviceID(it) }
    fun status(device: Device): String = when {
        model.session != 0L && model.selected?.selector == device.selector -> "Connected"
        !device.isCloud -> "Nearby"
        model.cloudError != null || model.presenceError.isNotEmpty() || System.currentTimeMillis() - model.presenceAt > 60_000 -> "Status unknown"
        else -> model.presence?.firstOrNull { it.selector == device.selector }?.let { if (it.online) "Online" else "Offline" } ?: "Status unknown"
    }
    val available = setOf("Online", "Nearby", "Connected")
    val suggestions = remembered.records(model.account, model.organization).mapNotNull { record ->
        if (record.id == excludedID) return@mapNotNull null
        val device = devices.firstOrNull { recordingDeviceID(it) == record.id } ?: return@mapNotNull null
        Triple(record, device, status(device))
    }.sortedWith(compareByDescending<Triple<RememberedAIDevice, Device, String>> { it.third in available }.thenByDescending { it.first.lastUsed ?: Long.MIN_VALUE })
    PageColumn {
        item { Text("Choose an AI device", style = MaterialTheme.typography.headlineSmall, fontWeight = FontWeight.Bold) }
        if (suggestions.isEmpty()) item { SecondaryText("Devices with models appear here after we’ve checked them.") }
        else {
            item {
                SecondaryText(if (suggestions.all { it.third == "Offline" }) "These devices are offline. Choose another device or try again when they’re online."
                    else "Previously found models. We’ll check again when you connect.")
            }
            item {
                LensSection(null) {
                    suggestions.forEachIndexed { index, (record, device, state) ->
                        if (index > 0) HorizontalDivider(Modifier.padding(start = 70.dp), color = MaterialTheme.colorScheme.outlineVariant)
                        val enabled = state != "Offline" && model.initialized
                        Row(Modifier.fillMaxWidth().clickable(enabled = enabled) { select(record, device) }.padding(16.dp), horizontalArrangement = Arrangement.spacedBy(12.dp), verticalAlignment = Alignment.CenterVertically) {
                            Box(Modifier.alpha(if (state == "Offline") .5f else 1f)) { DeviceAvatar(model, device, 42.dp, if (state == "Offline") LensStyle.offline else LensStyle.green) }
                            Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(3.dp)) {
                                Text(device.title, style = MaterialTheme.typography.titleMedium, maxLines = 1, overflow = TextOverflow.Ellipsis)
                                Text(record.preferred?.name ?: "Model", style = MaterialTheme.typography.bodyMedium, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1)
                                Text("$state · Checked ${DateUtils.getRelativeTimeSpanString(record.checked, System.currentTimeMillis(), DateUtils.MINUTE_IN_MILLIS)}", style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                            }
                            Icon(Icons.Outlined.ChevronRight, null, Modifier.size(16.dp), tint = MaterialTheme.colorScheme.onSurfaceVariant)
                        }
                    }
                }
            }
        }
        item { LensButton("Choose another device", modifier = Modifier.fillMaxWidth(), onClick = chooseDevice) }
        item { SecondaryText("For a cloud model, choose a device for context, then select Cloud.") }
    }
}

/** The process-wide stores for Ask Wendy: one keystore and one remembered-devices list. */
object AskWendyStores {
    @Volatile private var keys: ChatCredentialStore? = null
    @Volatile private var devices: RememberedAIDevices? = null
    fun credentials(context: Context): ChatCredentialStore = keys ?: synchronized(this) { keys ?: ChatKeystore(context.applicationContext).also { keys = it } }
    fun remembered(context: Context): RememberedAIDevices = devices ?: synchronized(this) { devices ?: RememberedAIDevices(context.applicationContext).also { devices = it } }
}

@Composable fun rememberAIDevices(): RememberedAIDevices { val context = LocalContext.current; return remember { AskWendyStores.remembered(context) } }
