package dev.wendy.android

import android.graphics.Bitmap
import android.graphics.BitmapFactory
import android.media.MediaMetadataRetriever
import android.util.Base64
import androidx.compose.runtime.*
import androidx.compose.ui.graphics.ImageBitmap
import androidx.compose.ui.graphics.asImageBitmap
import kotlinx.coroutines.*
import kotlinx.coroutines.channels.Channel
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import org.json.JSONArray
import org.json.JSONObject
import java.io.File
import java.net.URI
import java.util.UUID

// The recording library shared by Data, Activity and the Data gallery widget
// (iOS MobileRecordingLibrary, RecordingSources, RecordingCatalog and RecordingPosters).

/** The agent's episode kinds, in the iOS filter order (MobileRecordingKind). */
val recordingKinds = listOf("mixed", "video", "audio", "rosbag", "images", "dataset", "unknown")
fun recordingKindTitle(kind: String) = when (kind) { "mixed" -> "Mixed"; "video" -> "Video"; "audio" -> "Audio"; "rosbag" -> "ROS bag"; "images" -> "Images"; "dataset" -> "Dataset"; else -> "Recording" }
private fun recordingKind(agentKind: String) = if (agentKind in recordingKinds && agentKind != "unknown") agentKind else "unknown"

data class RecordingEpisode(
    val id: String, val title: String, val startedNanos: Long, val bytes: Long, val duration: Double?, val trigger: String?, val kind: String,
    val synced: Boolean?, val topics: List<String>, val detailsLoaded: Boolean, val campaign: String?, val previewFile: String?, val previewCounts: List<Long>,
) {
    /** Milliseconds since 1970, or null when the device did not report a start. */
    val started: Long? get() = if (startedNanos == 0L) null else startedNanos / 1_000_000
    companion object {
        const val DETAIL_BATCH_LIMIT = 64

        /** Agents keep the event name in the trigger reason; custom events stay filterable (iOS triggerLabel). */
        fun triggerLabel(reason: String): String? {
            val raw = reason.removePrefix("event:")
            val label = raw.replace('_', ' ').replace('-', ' ').split(Regex("\\s+")).filter { it.isNotEmpty() }.joinToString(" ")
            return when (label.lowercase()) {
                "" -> null
                "person", "person detected" -> "Person"
                "door opened", "door open" -> "Door opened"
                "motion", "motion detected" -> "Motion"
                "schedule", "scheduled" -> "Schedule"
                "manual" -> "Manual"
                "loud sound", "loud sound detected" -> "Loud sound"
                else -> label.take(1).uppercase() + label.drop(1)
            }
        }

        /** Parses a Go RecordingEpisode. With [index], the list row keeps its title, start and size, as iOS does. */
        fun parse(value: JSONObject, index: RecordingEpisode? = null): RecordingEpisode {
            val id = value.text("ID")
            val synced = when (value.text("UploadState")) { "uploaded" -> true; "local", "pending", "uploading", "failed" -> false; else -> null }
            val duration = value.optDouble("Duration", -1.0).takeIf { value.has("Duration") && it >= 0 }
            val counts = value.optJSONArray("PreviewCounts")?.let { a -> (0 until a.length()).map { a.optLong(it) } }.orEmpty()
            val topics = value.optJSONArray("Topics")?.let { a -> (0 until a.length()).map { a.optString(it) } }.orEmpty()
            return RecordingEpisode(id, index?.title ?: value.text("Title").ifEmpty { id }, index?.startedNanos ?: value.number("Started"), index?.bytes ?: value.number("Bytes"),
                duration, triggerLabel(value.text("Trigger")), recordingKind(value.text("Kind")), synced, topics, value.flag("DetailsLoaded"),
                value.text("Campaign").ifEmpty { null }, value.optJSONObject("PreviewFile")?.toString(), counts)
        }
    }
}

data class RecordingLibrary(val device: Device, val model: String?, val episodes: List<RecordingEpisode>, val warning: String?) {
    val labelCoverage: String? get() {
        val loaded = episodes.count { it.detailsLoaded }
        return if (loaded < episodes.size) "Details checked: $loaded of ${episodes.size} episodes" else null
    }
    val sizeText get() = sizeText(episodes)
    val labels: List<Pair<String, Int>> get() = episodes.mapNotNull { it.trigger }.groupingBy { it }.eachCount().entries
        .sortedWith(compareByDescending<Map.Entry<String, Int>> { it.value }.thenBy { it.key }).map { it.key to it.value }
    fun episodes(trigger: String?, kind: String? = null) = episodes.filter { (trigger == null || it.trigger == trigger) && (kind == null || it.kind == kind) }
    /** A filtered export waits until every episode's details are known, so nothing is left out silently. */
    fun exportTargets(trigger: String?, selectedID: String?, kind: String? = null): List<RecordingEpisode> {
        val visible = episodes(trigger, kind)
        if (selectedID != null) return visible.filter { it.id == selectedID }
        if ((trigger != null || kind != null) && labelCoverage != null) return emptyList()
        return visible
    }
    companion object {
        fun sizeText(episodes: List<RecordingEpisode>) = formatBytes(episodes.fold(0L) { sum, e -> val next = sum + maxOf(0L, e.bytes); if (next < sum) Long.MAX_VALUE else next })
    }
}

/**
 * One owner for recordings seen on the LAN and through Cloud. A display name or address alone cannot
 * show that two routes reach the same device (iOS recordingDeviceID).
 */
fun recordingDeviceID(device: Device): String {
    val uri = runCatching { URI(device.id) }.getOrNull() ?: return device.id
    val path = (uri.rawPath ?: return device.id).split("/")
    if (path.size != 5 || path[0].isNotEmpty() || path[1] != "tenant") return device.id
    val spiffe = uri.scheme == "spiffe" && uri.host == "wendy.sh" && path[3] == "device"
    val cloud = uri.scheme == "cloud" && path[3] == "asset"
    return if (spiffe || cloud) "${path[2]}/${path[4]}" else device.id
}

val Device.isCloud get() = selector.startsWith("cloud://") || transport.equals("cloud", ignoreCase = true)

data class RecordingTarget(val id: String, val routes: List<Device>)

/** Where recordings can be read now: nearby routes, then fresh online Cloud presence (iOS RecordingSources). */
class RecordingSources(val targets: List<RecordingTarget>, val unavailable: Int, val inventory: List<RecordingTarget>) {
    companion object {
        fun of(model: WendyModel): RecordingSources {
            if (!model.initialized) return RecordingSources(emptyList(), 0, emptyList())
            val routes = linkedMapOf<String, MutableList<Device>>()
            model.nearbyDevices.filter { !it.lite && !it.selector.startsWith("vm:") }.forEach { routes.getOrPut(recordingDeviceID(it)) { mutableListOf() }.add(it) }
            val fleet = model.presence
            val fresh = fleet != null && System.currentTimeMillis() - model.presenceAt < 60_000 && model.presenceError.isEmpty() && model.cloudError == null
            val unreachable = mutableSetOf<String>()
            fleet?.filter { !it.lite }?.forEach { row ->
                val device = row.device
                val id = recordingDeviceID(device)
                if (fresh && row.online) routes.getOrPut(id) { mutableListOf() }.add(device) else unreachable += id
            }
            // Include a device connected by address even if it was not advertised.
            model.selected?.takeIf { model.session != 0L && !model.lite && !it.isCloud && !it.selector.startsWith("vm:") }?.let { device ->
                routes.putIfAbsent(recordingDeviceID(device), mutableListOf(device))
            }
            val known = LinkedHashMap<String, List<Device>>(routes)
            fleet?.filter { !it.lite }?.forEach { known.putIfAbsent(recordingDeviceID(it.device), listOf(it.device)) }
            return RecordingSources(
                routes.map { (id, devices) -> RecordingTarget(id, devices.sortedWith(compareBy<Device> { it.isCloud }.thenBy { it.selector })) }.sortedBy { it.id },
                (unreachable - routes.keys).size,
                known.map { RecordingTarget(it.key, it.value) }.sortedBy { it.id })
        }
    }
}

data class RecordingRead(val target: RecordingTarget, val library: RecordingLibrary? = null, val problem: String? = null)

/** One device's recordings read without changing the connected device (iOS GoClient.readRecordings). */
suspend fun <T> WendyModel.readRecordings(device: Device, block: suspend (Long) -> T): T {
    require(!device.lite) { "Wendy Lite does not store recordings." }
    currentCoroutineContext().ensureActive()
    val id = (backend.call("BeginRecordingRead") as? Number)?.toLong() ?: 0L
    check(id != 0L) { "Recording access is paused. Refresh to try again." }
    try { return block(id) } finally { withContext(NonCancellable) { try { backend.call("EndRecordingRead", id) } catch (_: Exception) {} } }
}

suspend fun WendyModel.recordingLibrary(device: Device): RecordingLibrary = readRecordings(device) { id ->
    val value = backend.call("RecordingLibrary", id, device.selector, device.requiresMTLS).obj()
    RecordingLibrary(device, value.text("Model").ifEmpty { null }, value.list("Episodes").map { RecordingEpisode.parse(it) }, value.text("Warning").ifEmpty { null })
}

suspend fun WendyModel.recordingDetails(device: Device, episodes: List<RecordingEpisode>): List<RecordingEpisode> {
    if (episodes.isEmpty()) return emptyList()
    require(episodes.size <= RecordingEpisode.DETAIL_BATCH_LIMIT) { "Too many recordings in one request." }
    return readRecordings(device) { id ->
        val rows = backend.call("RecordingDetails", id, device.selector, device.requiresMTLS, JSONArray(episodes.map { it.id })).obj().list("Episodes")
        check(rows.size == episodes.size) { "The device returned an incomplete batch of recording details." }
        episodes.zip(rows).map { (existing, row) ->
            check(row.text("ID") == existing.id) { "The device returned details for another recording." }
            if (!row.flag("DetailsLoaded")) existing else RecordingEpisode.parse(row, existing)
        }
    }
}

/**
 * The libraries of every reachable device, kept for one account and organization. Several screens load it;
 * each has its own refresh counter so one screen's pull to refresh does not reset another's (iOS RecordingCatalog).
 */
class RecordingCatalog(private val model: WendyModel) {
    var reads by mutableStateOf(emptyMap<String, RecordingRead>()); private set
    var loading by mutableStateOf(false); private set
    var detailProblems by mutableStateOf(emptyMap<String, String>()); private set
    var unreadAlerts by mutableIntStateOf(0)
    private var generation = 0
    private val lastRefresh = mutableMapOf<String, Int>()
    private var refreshedAt = emptyMap<String, Long>()
    private val attempted = mutableMapOf<String, Set<String>>()

    suspend fun load(targets: List<RecordingTarget>, refresh: Int, priority: String?, consumer: String = "data", retaining: List<RecordingTarget>? = null) {
        val run = ++generation
        val known = (retaining ?: targets).map { it.id }.toSet()
        reads = reads.filterKeys { it in known }
        refreshedAt = refreshedAt.filterKeys { it in reads }
        val force = lastRefresh[consumer]?.let { it != refresh } ?: (refresh != 0)
        if (force) { attempted.clear(); detailProblems = emptyMap() }
        lastRefresh[consumer] = refresh
        val remaining = targets.filter { it.id != priority }
        // Nearby routes first so a Cloud fleet cannot delay a local device.
        val ordered = targets.filter { it.id == priority } + remaining.filter { it.routes.firstOrNull()?.isCloud != true } + remaining.filter { it.routes.firstOrNull()?.isCloud == true }
        val now = System.currentTimeMillis()
        val pending = ordered.filter { target ->
            val read = reads[target.id]
            force || read?.library == null || read.problem != null || refreshedAt[target.id]?.let { now - it >= 30_000 } != false
        }
        loading = pending.isNotEmpty()
        try {
            if (!force && priority != null && reads[priority]?.library != null) loadDetails(listOf(priority), run)
            coroutineScope {
                val results = Channel<RecordingRead>(Channel.UNLIMITED)
                val queue = pending.iterator()
                var running = 0
                fun next() { if (queue.hasNext()) { val target = queue.next(); running++; launch { results.send(fetch(target)) } } }
                repeat(3) { next() }
                while (running > 0) {
                    val read = results.receive(); running--
                    if (run != generation) { coroutineContext.cancelChildren(); return@coroutineScope }
                    merge(read)
                    next()
                }
            }
            if (run != generation) return
            loading = false
            loadDetails(ordered.map { it.id }, run)
        } finally { if (run == generation) loading = false }
    }

    private suspend fun fetch(target: RecordingTarget): RecordingRead {
        var problem: String? = null
        for (device in target.routes) {
            currentCoroutineContext().ensureActive()
            try { return RecordingRead(target, model.recordingLibrary(device)) }
            catch (e: CancellationException) { throw e } catch (e: Exception) { problem = e.message ?: "Could not read recordings." }
        }
        return RecordingRead(target, null, problem)
    }

    private fun merge(read: RecordingRead) {
        var current = read
        val previous = reads[read.target.id]?.library
        if (read.library == null && previous != null) current = read.copy(library = previous, problem = "Couldn’t refresh. Showing saved recordings. " + (read.problem ?: "Try again."))
        val library = read.library
        if (library != null && previous != null) {
            // Finalized IDs keep their metadata across refreshes unless the list reports a change.
            val cached = previous.episodes.filter { it.detailsLoaded }.associateBy { it.id }
            val episodes = library.episodes.map { row ->
                val saved = cached[row.id]
                if (!row.detailsLoaded && saved != null && row.bytes == saved.bytes && row.startedNanos == saved.startedNanos && row.title == saved.title) saved else row
            }
            current = current.copy(library = RecordingLibrary(library.device, library.model ?: previous.model, episodes, if (episodes.any { !it.detailsLoaded }) library.warning else null))
        }
        if (library != null) {
            refreshedAt = refreshedAt + (read.target.id to System.currentTimeMillis())
            attempted.remove(read.target.id)
            detailProblems = detailProblems - read.target.id
        }
        reads = reads + (read.target.id to current)
    }

    /** One batch per device per round keeps small libraries responsive while a large one is indexed. */
    private suspend fun loadDetails(ids: List<String>, run: Int) {
        while (currentCoroutineContext().isActive && run == generation) {
            var hadWork = false
            for (id in ids) {
                if (run != generation) return
                val read = reads[id] ?: continue
                val library = read.library ?: continue
                if (read.problem != null) continue
                val visited = attempted[id] ?: emptySet()
                val batch = library.episodes.asSequence().filter { !it.detailsLoaded && it.id !in visited }.take(RecordingEpisode.DETAIL_BATCH_LIMIT).toList()
                if (batch.isEmpty()) continue
                hadWork = true
                try {
                    val updated = model.recordingDetails(library.device, batch)
                    if (run != generation) return
                    val known = updated.filter { it.detailsLoaded }.associateBy { it.id }
                    reads = reads + (id to read.copy(library = library.copy(episodes = library.episodes.map { known[it.id] ?: it }, warning = null)))
                    attempted[id] = visited + batch.map { it.id }
                    if (known.size < batch.size) detailProblems = detailProblems + (id to "Some episode details could not be read on ${library.device.title}.")
                } catch (e: CancellationException) { throw e } catch (e: Exception) {
                    if (run != generation) return
                    detailProblems = detailProblems + (id to "${library.device.title}: ${PresentedFailure(e.message ?: "").details}")
                    attempted[id] = visited + library.episodes.filter { !it.detailsLoaded }.map { it.id }
                }
            }
            if (!hadWork) return
        }
    }
}

sealed interface RecordingPoster {
    class Still(val image: ImageBitmap) : RecordingPoster
    class Waveform(val peaks: List<Int>) : RecordingPoster
}

/**
 * Decoded thumbnails shared by the device cards, the picker and Activity. Only rows on screen ask for one,
 * one media read at a time, and the device identity is part of every key (iOS RecordingPosters).
 */
class RecordingPosters(private val model: WendyModel) {
    private data class Key(val device: String, val episode: String)
    private val entries = mutableStateMapOf<Key, RecordingPoster>()
    private val order = ArrayDeque<Key>()
    private val queue = Mutex()

    fun poster(device: Device, episode: String): RecordingPoster? = entries[Key(recordingDeviceID(device), episode)]

    suspend fun load(device: Device, episode: String, file: String) {
        val key = Key(recordingDeviceID(device), episode)
        if (entries.containsKey(key)) return
        try {
            // A row that only flashes past while scrolling should not start a transfer.
            delay(180)
            if (entries.containsKey(key)) return
            val result = queue.withLock {
                model.readRecordings(device) { id -> model.backend.call("RecordingThumbnail", id, device.selector, device.requiresMTLS, episode, JSONObject(file)).obj() }
            }
            val bytes = Base64.decode(result.text("bytes"), Base64.DEFAULT)
            val poster = withContext(Dispatchers.Default) {
                when (result.text("format")) {
                    "audio" -> RecordingPoster.Waveform(bytes.map { it.toInt() and 0xFF })
                    "image" -> still(bytes)?.let { RecordingPoster.Still(it.asImageBitmap()) }
                    "video" -> movie(bytes)?.let { RecordingPoster.Still(it.asImageBitmap()) }
                    else -> null
                }
            } ?: return
            currentCoroutineContext().ensureActive()
            insert(key, poster)
        } catch (e: CancellationException) { throw e } catch (_: Exception) {
            // Keep the kind's artwork; a later appearance can try again.
        }
    }

    private fun insert(key: Key, poster: RecordingPoster) {
        if (entries.containsKey(key)) return
        if (order.size >= 64) entries.remove(order.removeFirst())
        entries[key] = poster
        order.addLast(key)
    }

    private fun still(bytes: ByteArray): Bitmap? {
        val bounds = BitmapFactory.Options().apply { inJustDecodeBounds = true }
        BitmapFactory.decodeByteArray(bytes, 0, bytes.size, bounds)
        if (bounds.outWidth <= 0 || bounds.outHeight <= 0) return null
        var sample = 1
        while (maxOf(bounds.outWidth, bounds.outHeight) / (sample * 2) >= POSTER_SIZE) sample *= 2
        val bitmap = BitmapFactory.decodeByteArray(bytes, 0, bytes.size, BitmapFactory.Options().apply { inSampleSize = sample }) ?: return null
        return scaled(bitmap)
    }

    private fun movie(bytes: ByteArray): Bitmap? {
        val file = File(model.getApplication<WendyApplication>().cacheDir, "wendy-thumb-${UUID.randomUUID()}.mp4")
        val retriever = MediaMetadataRetriever()
        return try {
            file.writeBytes(bytes)
            retriever.setDataSource(file.absolutePath)
            retriever.getScaledFrameAtTime(0, MediaMetadataRetriever.OPTION_CLOSEST_SYNC, POSTER_SIZE, POSTER_SIZE)
        } catch (_: Exception) { null } finally { retriever.release(); file.delete() }
    }

    private fun scaled(bitmap: Bitmap): Bitmap {
        val largest = maxOf(bitmap.width, bitmap.height)
        if (largest <= POSTER_SIZE) return bitmap
        val scale = POSTER_SIZE.toFloat() / largest
        return Bitmap.createScaledBitmap(bitmap, maxOf(1, (bitmap.width * scale).toInt()), maxOf(1, (bitmap.height * scale).toInt()), true)
    }

    private companion object { const val POSTER_SIZE = 190 }
}

/** The catalog for the current account and organization; null outside the app's root. */
val LocalRecordingCatalog = staticCompositionLocalOf<RecordingCatalog?> { null }
/** Thumbnails, or null where a screen is hidden and must not start reads. */
val LocalRecordingPosters = compositionLocalOf<RecordingPosters?> { null }
