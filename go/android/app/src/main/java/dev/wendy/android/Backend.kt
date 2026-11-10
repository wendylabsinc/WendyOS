package dev.wendy.android

import android.app.Application
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import org.json.JSONArray
import org.json.JSONObject

internal object NativeBridge {
    init { System.loadLibrary("wendy") }
    external fun invoke(request: ByteArray): ByteArray
    external fun attach(platform: PlatformServices)
}

class BackendFailure(message: String, val identityRecovery: Boolean = false) : Exception(message)

class Backend(private val app: Application) {
    suspend fun initialize() = call("Initialize", app.noBackupFilesDir.resolve("wendy").absolutePath, app.cacheDir.absolutePath)
    suspend fun call(method: String, vararg args: Any?): Any? = withContext(Dispatchers.IO) {
        val request = JSONObject().put("method", method).put("args", JSONArray(args.toList()))
        val result = JSONObject(NativeBridge.invoke(request.toString().toByteArray(Charsets.UTF_8)).toString(Charsets.UTF_8))
        val error = result.optString("error")
        if (error.isNotEmpty()) throw BackendFailure(error, result.optBoolean("identityRecovery"))
        result.opt("value").takeUnless { it == JSONObject.NULL }
    }
}

class WendyApplication : Application() {
    @Volatile var visible = false
    override fun onCreate() { super.onCreate(); NativeBridge.attach(PlatformServices(this)) }
    val backend by lazy { Backend(this) }
}

fun Any?.obj(): JSONObject = this as? JSONObject ?: JSONObject()
fun Any?.items(): List<JSONObject> = (this as? JSONArray)?.let { array ->
    (0 until array.length()).mapNotNull { array.optJSONObject(it) }
} ?: emptyList()
fun JSONObject.text(vararg names: String): String = names.firstNotNullOfOrNull { name ->
    opt(name)?.takeUnless { it == JSONObject.NULL }?.toString()?.takeIf { it.isNotBlank() }
} ?: ""
fun JSONObject.number(vararg names: String): Long = names.firstOrNull { has(it) }?.let { optLong(it) } ?: 0L
fun JSONObject.child(vararg names: String): JSONObject = names.firstNotNullOfOrNull { optJSONObject(it) } ?: JSONObject()
fun JSONObject.list(vararg names: String): List<JSONObject> = names.firstNotNullOfOrNull { optJSONArray(it) }.items()
fun JSONObject.flag(vararg names: String): Boolean = names.firstOrNull { has(it) }?.let { optBoolean(it) } ?: false

internal fun json(vararg entries: Pair<String, Any?>): JSONObject = JSONObject().apply { entries.forEach { (k,v) -> put(k,v) } }

data class Device(val id: String, val name: String, val selector: String, val transport: String, val lite: Boolean = false) {
    /** The readable name shown in the interface; [name] and [selector] stay the address. */
    val title: String get() = DeviceDisplayName.title(name.ifBlank { selector })
    companion object {
        fun parse(value: JSONObject) = Device(value.text("id", "ID"), value.text("name", "Name"), value.text("selector", "Selector"), value.text("transport", "Transport"), value.flag("lite", "Lite"))
    }
}
