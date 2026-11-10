package dev.wendy.android

import androidx.compose.runtime.*
import org.json.JSONArray
import org.json.JSONObject
import java.io.File
import java.security.MessageDigest
import java.util.UUID
import kotlin.math.abs
import kotlin.math.cos
import kotlin.math.sin
import kotlin.math.tan

// Sites and their 3D models, kept on this phone per account and organization (iOS MobileSitesStore and
// SiteTwin's Site, SiteStore and SiteArchive). The JSON matches Swift's Codable output so .wendysite
// archives move between Android, iOS and Wendy devices unchanged.

class SiteException(message: String) : Exception(message)

private fun uuid() = UUID.randomUUID().toString().uppercase()
private fun validUUID(value: String) = runCatching { UUID.fromString(value) }.isSuccess

/** Metres, right-handed, Y up. A manually placed sensor location, not a measured position. */
data class SitePoint(val x: Double, val y: Double, val z: Double) {
    val valid get() = listOf(x, y, z).all { it.isFinite() && abs(it) <= 1_000_000 }
    fun json(): JSONObject = JSONObject().put("x", x).put("y", y).put("z", z)
    companion object { fun parse(value: JSONObject) = SitePoint(value.getDouble("x"), value.getDouble("y"), value.getDouble("z")) }
}

/** A person's visual alignment of a camera, saved with the model; it is not a measured calibration (iOS SiteManualAlignment). */
data class SiteManualAlignment(val modelRevision: String, val yaw: Double, val pitch: Double, val roll: Double, val horizontalFOV: Double, val width: Int, val height: Int) {
    val valid get() = listOf(yaw, pitch, roll, horizontalFOV).all { it.isFinite() } && horizontalFOV in 20.0..140.0 && width in 1..8192 && height in 1..8192
    fun json(): JSONObject = JSONObject().put("modelRevision", modelRevision).put("yaw", yaw).put("pitch", pitch).put("roll", roll)
        .put("horizontalFOV", horizontalFOV).put("width", width).put("height", height)
    /** The camera's world rotation (yaw about Y, then pitch about X, then roll about Z), column-major 3×3. */
    fun rotation(): DoubleArray = SiteMath.multiply3(SiteMath.multiply3(SiteMath.axis(1, yaw), SiteMath.axis(0, pitch)), SiteMath.axis(2, roll))
    val verticalFOV get() = Math.toDegrees(2 * kotlin.math.atan(tan(Math.toRadians(horizontalFOV) / 2) * height / width))
    companion object {
        fun parse(value: JSONObject) = SiteManualAlignment(value.getString("modelRevision"), value.getDouble("yaw"), value.getDouble("pitch"), value.getDouble("roll"),
            value.getDouble("horizontalFOV"), value.getInt("width"), value.getInt("height"))
    }
}

data class SiteCamera(val id: String = uuid(), val gatewayID: String, val sourceID: String, val name: String, val position: SitePoint? = null, val manualAlignment: SiteManualAlignment? = null) {
    fun json(): JSONObject = JSONObject().put("id", id).put("gatewayID", gatewayID).put("sourceID", sourceID).put("name", name)
        .also { o -> position?.let { o.put("position", it.json()) }; manualAlignment?.let { o.put("manualAlignment", it.json()) } }
    companion object {
        fun parse(value: JSONObject) = SiteCamera(value.getString("id"), value.getString("gatewayID"), value.getString("sourceID"), value.optString("name"),
            value.optJSONObject("position")?.let(SitePoint::parse), value.optJSONObject("manualAlignment")?.let(SiteManualAlignment::parse))
    }
}

/** One 3D model of a site with its camera placements (iOS Site). */
data class Site(val id: String = uuid(), val name: String, val modelRevision: String = uuid(), val metresPerModelUnit: Double = 1.0, val cameras: List<SiteCamera> = emptyList(), val version: Int = 1) {
    fun validate() {
        if (version != 1) throw SiteException("This site uses a newer file format. Update Wendy and try again.")
        if (name.isBlank() || name.length > 200 || !metresPerModelUnit.isFinite() || metresPerModelUnit <= 0 || metresPerModelUnit > 1000 || cameras.size > 256 || !validUUID(id) || !validUUID(modelRevision)) throw invalid()
        val ids = HashSet<String>(); val sources = HashSet<String>()
        for (camera in cameras) {
            if (!ids.add(camera.id) || camera.gatewayID.isEmpty() || camera.gatewayID.length > 1024 || camera.sourceID.isEmpty() || camera.sourceID.length > 1024 ||
                camera.name.length > 200 || !sources.add("${camera.gatewayID.length}:${camera.gatewayID}${camera.sourceID}") || camera.position?.valid == false) throw invalid()
            camera.manualAlignment?.let { manual -> if (camera.position == null || manual.modelRevision != modelRevision || !manual.valid) throw invalid() }
        }
    }
    fun json(): JSONObject = JSONObject().put("version", version).put("id", id).put("name", name).put("modelRevision", modelRevision)
        .put("metresPerModelUnit", metresPerModelUnit).put("cameras", JSONArray(cameras.map { it.json() }))
    companion object {
        fun invalid() = SiteException("The site contains invalid settings. Choose a valid site file and try again.")
        fun parse(value: JSONObject) = Site(value.getString("id"), value.getString("name"), value.getString("modelRevision"), value.getDouble("metresPerModelUnit"),
            value.optJSONArray("cameras")?.let { a -> (0 until a.length()).map { SiteCamera.parse(a.getJSONObject(it)) } }.orEmpty(), value.getInt("version"))
    }
}

/**
 * A .wendysite file: "WENDYST1", the manifest length, the manifest and the USDZ; "WENDYST2" adds the model
 * and visual map lengths and a visual reference map. Neither contains credentials (iOS SiteArchive).
 */
class SiteArchive(val site: Site, val model: ByteArray, val visualMap: ByteArray? = null) {
    init {
        site.validate()
        if (model.size > SiteModelStore.MAXIMUM_MODEL_BYTES || !isZip(model)) throw SiteException("Choose a self-contained USDZ model of the site.")
        if (visualMap != null && visualMap.size > MAXIMUM_MAP_BYTES) throw SiteException("This site’s reference map is too large.")
    }
    fun data(): ByteArray {
        val manifest = site.json().toString().toByteArray()
        if (manifest.size > 1_048_576) throw Site.invalid()
        val out = java.io.ByteArrayOutputStream()
        out.write((if (visualMap == null) "WENDYST1" else "WENDYST2").toByteArray())
        fun length(n: Int) { out.write(byteArrayOf((n ushr 24).toByte(), (n ushr 16).toByte(), (n ushr 8).toByte(), n.toByte())) }
        length(manifest.size)
        if (visualMap != null) { length(model.size); length(visualMap.size) }
        out.write(manifest); out.write(model); visualMap?.let(out::write)
        return out.toByteArray()
    }
    companion object {
        const val MAXIMUM_MAP_BYTES = 64 * 1024 * 1024
        val MAXIMUM_BYTES = SiteModelStore.MAXIMUM_MODEL_BYTES.toLong() + MAXIMUM_MAP_BYTES + 1_048_576 + 20
        fun isZip(bytes: ByteArray) = bytes.size >= 4 && bytes[0] == 0x50.toByte() && bytes[1] == 0x4b.toByte() && bytes[2] == 3.toByte() && bytes[3] == 4.toByte()
        fun parse(data: ByteArray): SiteArchive {
            if (data.size > MAXIMUM_BYTES || data.size < 16) throw Site.invalid()
            fun length(at: Int) = ((data[at].toInt() and 0xFF) shl 24) or ((data[at + 1].toInt() and 0xFF) shl 16) or ((data[at + 2].toInt() and 0xFF) shl 8) or (data[at + 3].toInt() and 0xFF)
            val header = String(data, 0, 8, Charsets.US_ASCII)
            val manifestSize = length(8)
            if (manifestSize <= 0 || manifestSize > 1_048_576) throw Site.invalid()
            return when (header) {
                "WENDYST1" -> {
                    if (12 + manifestSize >= data.size) throw Site.invalid()
                    SiteArchive(Site.parse(JSONObject(String(data, 12, manifestSize))), data.copyOfRange(12 + manifestSize, data.size))
                }
                "WENDYST2" -> {
                    val modelSize = length(12); val mapSize = length(16)
                    if (modelSize <= 0 || modelSize > SiteModelStore.MAXIMUM_MODEL_BYTES || mapSize <= 0 || mapSize > MAXIMUM_MAP_BYTES || 20L + manifestSize + modelSize + mapSize != data.size.toLong()) throw Site.invalid()
                    val modelStart = 20 + manifestSize; val mapStart = modelStart + modelSize
                    SiteArchive(Site.parse(JSONObject(String(data, 20, manifestSize))), data.copyOfRange(modelStart, mapStart), data.copyOfRange(mapStart, data.size))
                }
                else -> throw Site.invalid()
            }
        }
    }
}

class StoredSite(val site: Site, val model: File, val visualMap: File?) { val id get() = site.id }

/** Models live in their own folders; removal moves a folder aside so placements stay recoverable (iOS SiteStore). */
class SiteModelStore(private val directory: File) {
    private fun folder(id: String): File { if (!validUUID(id)) throw SiteException("The saved site is unavailable. Import the site again."); return File(directory, id.uppercase()) }
    fun load(id: String): StoredSite {
        val folder = folder(id)
        val manifest = File(folder, "site.json")
        if (!manifest.isFile || manifest.length() > 1_048_576) throw SiteException("The saved site is unavailable. Import the site again.")
        val site = Site.parse(JSONObject(manifest.readText())).also { it.validate() }
        if (!site.id.equals(id, ignoreCase = true)) throw Site.invalid()
        val model = File(folder, "model.usdz")
        validateModel(model)
        return StoredSite(site, model, File(folder, "visual-map.json").takeIf { it.isFile })
    }
    fun create(name: String, model: ByteArray): StoredSite {
        val site = Site(name = name).also { it.validate() }
        if (!SiteArchive.isZip(model) || model.size > MAXIMUM_MODEL_BYTES) throw SiteException("Choose a self-contained USDZ model of the site.")
        return write(site, model, null)
    }
    fun importArchive(archive: SiteArchive): StoredSite = write(archive.site.copy(id = uuid()), archive.model, archive.visualMap)
    private fun write(site: Site, model: ByteArray, map: ByteArray?): StoredSite {
        site.validate()
        val destination = folder(site.id)
        if (!destination.mkdirs() && !destination.isDirectory) throw SiteException("Could not save the model on this phone.")
        try {
            atomic(File(destination, "model.usdz"), model)
            map?.let { atomic(File(destination, "visual-map.json"), it) }
            save(site)
            return load(site.id)
        } catch (e: Exception) { destination.deleteRecursively(); throw e }
    }
    fun save(site: Site) {
        site.validate()
        if (!File(folder(site.id), "model.usdz").isFile) throw SiteException("The saved site is unavailable. Import the site again.")
        atomic(File(folder(site.id), "site.json"), site.json().toString(2).toByteArray())
    }
    fun remove(id: String) {
        val removed = File(directory, "Recently Removed").apply { mkdirs() }
        if (!folder(id).renameTo(File(removed, id.uppercase()))) throw SiteException("Could not remove the model.")
    }
    fun archive(id: String): SiteArchive { val saved = load(id); return SiteArchive(saved.site, saved.model.readBytes(), saved.visualMap?.readBytes()) }
    companion object {
        const val MAXIMUM_MODEL_BYTES = 128 * 1024 * 1024
        fun validateModel(file: File) {
            if (!file.isFile || file.length() <= 0) throw SiteException("Choose a self-contained USDZ model of the site.")
            if (file.length() > MAXIMUM_MODEL_BYTES) throw SiteException("This model is too large. Export a USDZ model smaller than 128 MB.")
            val head = file.inputStream().use { input -> ByteArray(4).also { input.read(it) } }
            if (!SiteArchive.isZip(head)) throw SiteException("Choose a self-contained USDZ model of the site.")
        }
        fun atomic(file: File, bytes: ByteArray) {
            val temporary = File(file.parentFile, file.name + ".tmp")
            temporary.writeBytes(bytes)
            if (!temporary.renameTo(file)) { temporary.delete(); throw SiteException("Could not save the model on this phone.") }
        }
        /** The share file name iOS uses: path characters become dashes, at most 40 characters. */
        fun shareFilename(name: String): String {
            val clean = name.map { if (it in "/\\:" || it.isISOControl()) '-' else it }.joinToString("").trim().trim('.')
            return clean.take(40).ifEmpty { "Digital Twin" } + ".wendysite"
        }
    }
}

data class SiteAsset(val id: String, val name: String, val robot: Boolean) {
    fun json(): JSONObject = JSONObject().put("id", id).put("name", name).put("robot", robot)
    companion object { fun parse(v: JSONObject) = SiteAsset(v.getString("id"), v.getString("name"), v.optBoolean("robot")) }
}
data class SiteModelLink(val id: String, val name: String, val covers: String = "Whole site") {
    fun json(): JSONObject = JSONObject().put("id", id).put("name", name).put("covers", covers)
    companion object { fun parse(v: JSONObject) = SiteModelLink(v.getString("id"), v.getString("name"), v.optString("covers", "Whole site")) }
}
data class SiteRecord(val id: String = uuid(), val name: String, val assets: List<SiteAsset>, val models: List<SiteModelLink> = emptyList(), val defaultModel: String? = null, val hideSetup: Boolean = false) {
    val assetSummary get() = assets.count { it.robot }.let { robots -> "${assets.size - robots} devices · $robots robots" }
    val primaryModel get() = models.firstOrNull { it.id == defaultModel } ?: models.firstOrNull()
    fun json(): JSONObject = JSONObject().put("id", id).put("name", name).put("assets", JSONArray(assets.map { it.json() })).put("models", JSONArray(models.map { it.json() }))
        .put("hideSetup", hideSetup).also { o -> defaultModel?.let { o.put("defaultModel", it) } }
    companion object {
        fun parse(v: JSONObject) = SiteRecord(v.getString("id"), v.getString("name"), v.optJSONArray("assets")?.let { a -> (0 until a.length()).map { SiteAsset.parse(a.getJSONObject(it)) } }.orEmpty(),
            v.optJSONArray("models")?.let { a -> (0 until a.length()).map { SiteModelLink.parse(a.getJSONObject(it)) } }.orEmpty(), v.optString("defaultModel").ifEmpty { null }, v.optBoolean("hideSetup"))
    }
}
data class SiteEvent(val id: String = uuid(), val siteID: String, val text: String, val date: Long = System.currentTimeMillis()) {
    fun json(): JSONObject = JSONObject().put("id", id).put("siteID", siteID).put("text", text).put("date", date)
    companion object { fun parse(v: JSONObject) = SiteEvent(v.getString("id"), v.getString("siteID"), v.getString("text"), v.optLong("date")) }
}

/**
 * The person's sites for one account and organization. A site links any number of models; it is never
 * inferred from a saved capture, and nothing here changes Cloud (iOS MobileSitesStore).
 */
class SitesStore(base: File, account: String, organization: String) {
    var sites by mutableStateOf(emptyList<SiteRecord>()); private set
    var events by mutableStateOf(emptyList<SiteEvent>()); private set
    var selected by mutableStateOf<String?>(null); private set
    var loadError by mutableStateOf<String?>(null); private set
    var revision by mutableIntStateOf(0); private set
    private val directory = File(base, scope(account, organization))
    val models = SiteModelStore(File(directory, "Models"))
    val current: SiteRecord? get() = sites.firstOrNull { it.id == selected } ?: sites.firstOrNull()

    init {
        val file = File(directory, "sites.json")
        if (file.isFile) try {
            val document = JSONObject(file.readText())
            if (document.optInt("version") != 1) throw Site.invalid()
            val parsed = document.optJSONArray("sites")?.let { a -> (0 until a.length()).map { SiteRecord.parse(a.getJSONObject(it)) } }.orEmpty()
            if (parsed.map { it.id }.toSet().size != parsed.size) throw Site.invalid()
            sites = parsed
            events = document.optJSONArray("events")?.let { a -> (0 until a.length()).map { SiteEvent.parse(a.getJSONObject(it)) } }.orEmpty()
            selected = document.optString("selected").ifEmpty { null }
        } catch (e: Exception) { loadError = "Saved sites could not be read. Your files have not been replaced. " + (e.message ?: "") }
    }

    fun create(name: String, assets: List<SiteAsset>): String {
        val trimmed = name.trim()
        if (trimmed.isEmpty() || trimmed.length > 200 || assets.map { it.id }.toSet().size != assets.size) throw Site.invalid()
        val site = SiteRecord(name = trimmed, assets = assets)
        commit(sites + site, events + SiteEvent(siteID = site.id, text = "$trimmed created"), site.id)
        return site.id
    }
    fun setAssets(assets: List<SiteAsset>, siteID: String) {
        val site = sites.firstOrNull { it.id == siteID } ?: throw missing()
        if (assets.map { it.id }.toSet().size != assets.size) throw Site.invalid()
        if (site.assets == assets) return
        commit(sites.map { if (it.id == siteID) it.copy(assets = assets) else it }, events + SiteEvent(siteID = siteID, text = "Devices and robots updated for ${site.name}"), selected)
    }
    fun select(id: String) { if (sites.none { it.id == id }) throw missing(); commit(sites, events, id) }
    fun setSetupHidden(hidden: Boolean, siteID: String) { if (sites.none { it.id == siteID }) throw missing(); commit(sites.map { if (it.id == siteID) it.copy(hideSetup = hidden) else it }, events, selected) }
    fun setDefault(modelID: String, siteID: String) {
        if (sites.none { it.id == siteID && it.models.any { m -> m.id == modelID } }) throw missing()
        commit(sites.map { if (it.id == siteID) it.copy(defaultModel = modelID) else it }, events, selected)
    }
    fun renameModel(modelID: String, siteID: String, name: String) {
        val trimmed = name.trim()
        if (trimmed.isEmpty() || trimmed.length > 200) throw SiteException("Use a model name between 1 and 200 characters.")
        if (loadError != null || sites.none { it.id == siteID && it.models.any { m -> m.id == modelID } }) throw missing()
        val previous = models.load(modelID).site
        models.save(previous.copy(name = trimmed))
        try {
            commit(sites.map { s -> if (s.id == siteID) s.copy(models = s.models.map { if (it.id == modelID) it.copy(name = trimmed) else it }) else s },
                events + SiteEvent(siteID = siteID, text = "3D model renamed to $trimmed"), selected)
        } catch (e: Exception) { models.save(previous); throw e }
    }
    /** Imports a USDZ or .wendysite file after checking that its geometry can be read. */
    fun addModel(name: String, bytes: ByteArray, siteID: String): StoredSite {
        if (loadError != null || sites.none { it.id == siteID }) throw missing()
        val saved = if (!SiteArchive.isZip(bytes)) {
            val archive = SiteArchive.parse(bytes)
            UsdReader.read(archive.model)
            models.importArchive(archive)
        } else {
            UsdReader.read(bytes)
            models.create(name.ifBlank { "3D model" }.take(200), bytes)
        }
        try {
            commit(sites.map { s -> if (s.id == siteID) s.copy(models = s.models + SiteModelLink(saved.id, saved.site.name), defaultModel = s.defaultModel ?: saved.id) else s },
                events + SiteEvent(siteID = siteID, text = "${saved.site.name} linked to ${sites.first { it.id == siteID }.name}"), selected)
        } catch (e: Exception) { runCatching { models.remove(saved.id) }; throw e }
        return saved
    }
    /** Saves placements for one model revision; a changed model is refused instead of moving cameras onto new geometry. */
    fun saveCameraModel(value: Site, siteID: String, event: String) {
        if (loadError != null || sites.none { it.id == siteID && it.models.any { m -> m.id == value.id } }) throw missing()
        val previous = models.load(value.id).site
        if (previous.modelRevision != value.modelRevision) throw Site.invalid()
        models.save(value)
        try { commit(sites, events + SiteEvent(siteID = siteID, text = event), selected) } catch (e: Exception) { models.save(previous); throw e }
    }
    private fun commit(next: List<SiteRecord>, nextEvents: List<SiteEvent>, nextSelected: String?) {
        if (loadError != null) throw Site.invalid()
        val bounded = nextEvents.takeLast(1000)
        val document = JSONObject().put("version", 1).put("sites", JSONArray(next.map { it.json() })).put("events", JSONArray(bounded.map { it.json() }))
            .also { o -> nextSelected?.let { o.put("selected", it) } }
        directory.mkdirs()
        SiteModelStore.atomic(File(directory, "sites.json"), document.toString().toByteArray())
        sites = next; events = bounded; selected = nextSelected; revision++
    }
    private fun missing() = SiteException("The saved site is unavailable. Import the site again.")
    companion object {
        fun scope(account: String, organization: String): String =
            MessageDigest.getInstance("SHA-256").digest(JSONArray(listOf(account, organization)).toString().toByteArray()).joinToString("") { "%02x".format(it) }
    }
}

/**
 * The stable identity of a camera's gateway: its Cloud asset, its enrolled device or its serial within the
 * organization. Placements match only this identity, never an address or display name (iOS SiteGatewayIdentity).
 */
data class SiteGatewayIdentity(val id: String = "", val aliases: Set<String> = emptySet()) {
    fun matches(saved: String) = id.isNotEmpty() && saved.isNotEmpty() && (saved == id || saved in aliases)
    companion object {
        fun cloud(selector: String): String? {
            val uri = runCatching { java.net.URI(selector) }.getOrNull() ?: return null
            if (uri.scheme != "cloud" || uri.userInfo != null || uri.query != null || uri.fragment != null || uri.host.isNullOrEmpty()) return null
            val path = uri.path.split('/').filter { it.isNotEmpty() }
            if (path.size != 4 || path[0] != "tenant" || path[2] != "asset" || !validUUID(path[1]) || !validUUID(path[3])) return null
            return "cloud-v2/${path[1].lowercase()}/${path[3].lowercase()}"
        }
        fun resolve(cloud: String?, enrolled: Boolean, organizationID: String, deviceID: String, serial: String?): SiteGatewayIdentity {
            val enrollment = if (enrolled && validUUID(organizationID) && validUUID(deviceID)) "cloud-v2/${organizationID.lowercase()}/${deviceID.lowercase()}" else null
            if (cloud != null && enrollment != null && cloud != enrollment) throw SiteException("The connected gateway's enrollment does not match its Cloud identity. Reconnect to the gateway.")
            val hardware = if (!serial.isNullOrEmpty() && (!enrolled || organizationID.isNotEmpty())) "hardware-v1/$organizationID/$serial" else null
            val id = cloud ?: enrollment ?: hardware ?: throw SiteException("This gateway has no persistent identity. Connect through its Cloud entry to save camera placements.")
            return SiteGatewayIdentity(id, setOfNotNull(id, hardware))
        }
    }
}

/** Connects to a site device and reads its gateway identity from device info and enrollment. */
suspend fun WendyModel.siteGateway(device: Device): SiteGatewayIdentity {
    if (!connectAndWait(device)) throw SiteException(detailError ?: "Could not connect to the camera device.")
    camerasError?.let { throw SiteException(it) }
    val session = session
    val info = backend.call("DeviceInfo", session).obj()
    val enrollment = backend.call("DeviceEnrollment", session).obj()
    if (this.session != session) throw SiteException("The device connection changed. Try again.")
    return SiteGatewayIdentity.resolve(SiteGatewayIdentity.cloud(device.selector), enrollment.flag("Enrolled"), enrollment.text("OrganizationID"), enrollment.text("DeviceID"), info.text("serial_number", "serial"))
}

/** Small 3D helpers shared by site geometry and the renderer. Matrices are column-major. */
object SiteMath {
    fun axis(axis: Int, degrees: Double): DoubleArray {
        val r = Math.toRadians(degrees); val c = cos(r); val s = sin(r)
        return when (axis) {
            0 -> doubleArrayOf(1.0, 0.0, 0.0, 0.0, c, s, 0.0, -s, c)
            1 -> doubleArrayOf(c, 0.0, -s, 0.0, 1.0, 0.0, s, 0.0, c)
            else -> doubleArrayOf(c, s, 0.0, -s, c, 0.0, 0.0, 0.0, 1.0)
        }
    }
    fun multiply3(a: DoubleArray, b: DoubleArray) = DoubleArray(9) { i -> val col = i / 3; val row = i % 3; (0..2).sumOf { a[it * 3 + row] * b[col * 3 + it] } }
    fun apply3(m: DoubleArray, v: DoubleArray) = DoubleArray(3) { row -> m[row] * v[0] + m[3 + row] * v[1] + m[6 + row] * v[2] }
}
