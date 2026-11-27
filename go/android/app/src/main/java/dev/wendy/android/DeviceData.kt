package dev.wendy.android

import org.json.JSONObject
import java.text.Normalizer
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

// Typed views of the shared Go backend's JSON. Field names follow the Go
// structs and protobuf JSON tags; the iOS app reads the same values through C.

/** Hostnames stay the address; the interface shows a readable name (iOS DeviceDisplayName). */
object DeviceDisplayName {
    fun title(value: String): String {
        val prefix = listOf("wendyos-", "wendy-lite-").firstOrNull { value.lowercase().startsWith(it) } ?: return value
        val suffix = value.drop(prefix.length)
        if (suffix.isEmpty()) return "Wendy device"
        return suffix.split('-').filter { it.isNotEmpty() }.joinToString(" ") { word ->
            if (word.any(Char::isDigit)) word.uppercase() else word.replaceFirstChar { it.uppercase() }
        }
    }
}

/** Errors worth retrying automatically, matching iOS ConnectionRecovery. */
object ConnectionRecovery {
    fun canRetry(message: String): Boolean {
        val value = message.lowercase()
        if (listOf("permission", "unauthenticated", "sign in", "certificate", "identity", "pin mismatch", "not supported", "does not support", "update this device", "access denied").any(value::contains)) return false
        return listOf("timeout", "timed out", "deadline", "unavailable", "no route", "network is unreachable", "network is down", "connection refused", "connection reset", "connection ended", "connection closed", "disconnected", "offline", "broken pipe", "startup interrupted", "eof").any(value::contains)
    }
}

/** Plain-language message plus redacted diagnostics for backend errors (iOS PresentedFailure). */
class PresentedFailure(raw: String) {
    var title: String? = null; private set
    var blocked = false; private set
    val message: String
    val details: String
    init {
        val lower = raw.lowercase()
        message = when {
            "device is not in the device list yet" in lower -> "Your device has not appeared in the device list yet. Keep it online, then open it again."
            "device name is already in use" in lower -> "That device name is already in use. Choose another name."
            "cloud rejected the enrollment details" in lower -> "Check the device name and organization."
            "does not have permission to add devices" in lower -> "Your account cannot add devices to this organization. Choose another organization or ask an administrator for access."
            "enrollment is not available for this organization" in lower -> "Enrollment is not available for this organization. Contact your administrator."
            "agent does not support network traffic statistics" in lower -> "This device’s agent does not report network traffic. Update the agent to see these statistics."
            "valid app_id is required" in lower -> "Cloud rejected this campaign’s notification identity. Update the device agent, then enable campaign notifications again."
            "does not support direct online updates yet" in lower -> "This agent needs a newer version before it can download updates directly."
            "cloud does not offer tunnel service" in lower -> "This Cloud server needs an update before app ports can be reached through the VPN."
            "not sending video" in lower -> "This camera is not sending video. Check its source, or choose another camera."
            "sends vp8 video" in lower -> "This device sends VP8 video, which the Wendy app cannot play yet. Update the device’s Wendy agent, or install gstreamer1.0-plugins-bad on it."
            listOf("no route to host", "deadline exceeded", "connection refused", "offline").any(lower::contains) -> "The device could not be reached. Find it again to refresh its address. Its saved identity will be kept."
            listOf("not logged in", "no login", "sign in", "unauthenticated").any(lower::contains) -> "Sign in to Wendy Cloud, then try again."
            listOf("identity changed", "different certificate key", "has no enrolled identity").any(lower::contains) -> {
                title = "Connection blocked"; blocked = true
                "This device does not match the identity saved on this phone. Check the selected account and refresh Cloud devices, then try again. If you reset or re-enrolled it, forget its saved identity and reconnect."
            }
            listOf("pin mismatch", "identity mismatch", "certificate").any(lower::contains) -> {
                title = "Connection blocked"; blocked = true
                "Wendy could not verify this device. Check its identity and your sign-in before reconnecting."
            }
            "permissiondenied" in lower || "permission denied" in lower -> "Your account does not have access to this device. Check the selected organization and try again."
            else -> "Wendy could not complete this request. Try again."
        }
        // Diagnostics remain available without putting credentials on screen.
        var safe = raw
        listOf("(?s)-----BEGIN [^-]+-----.*?-----END [^-]+-----", "(?i)(?:Bearer|DPoP)\\s+[A-Za-z0-9._~+/=-]+",
            "eyJ[A-Za-z0-9_-]+\\.[A-Za-z0-9_-]+\\.[A-Za-z0-9_-]+", "(?i)(?:access_token|refresh_token|api_key|token)=([^&\\s]+)").forEach {
            safe = safe.replace(Regex(it), "[redacted]")
        }
        details = safe.take(4000)
    }
}

data class WifiNetwork(val ssid: String, val security: Int, val signal: Int?, val known: Boolean, val connected: Boolean, val priority: Int) {
    val id get() = "$ssid|$security"
    val securityLabel get() = when (security) { 1 -> "Open"; 2 -> "WEP"; 3 -> "WPA"; 4 -> "WPA2"; 5 -> "WPA3"; 6 -> "Enterprise"; else -> "Unknown security" }
}

data class MeshLink(val id: Long, val principal: String, val deviceID: String, val address: String, val transport: String, val name: String, val cost: Long) {
    val title get() = name.ifEmpty { deviceID.ifEmpty { principal } }
}
data class MeshRoute(val id: Int, val prefix: String, val origin: String, val nextHop: String, val metric: Long, val local: Boolean, val unreachable: Boolean, val authorized: Boolean) {
    val reachable get() = !local && !unreachable && metric < 65535
}
data class MeshAnnouncement(val principal: String, val deviceID: String, val name: String, val prefix: String)
data class MeshTopology(val available: Boolean, val runtime: Boolean, val truncated: Boolean, val capturedMs: Long, val error: String,
                        val principal: String, val address: String, val links: List<MeshLink>, val routes: List<MeshRoute>, val announcements: List<MeshAnnouncement>)

data class NetworkSnapshot(
    val wifiAvailable: Boolean, val wifiConnected: Boolean, val ssid: String,
    val wifiError: String, val scanError: String, val knownError: String, val meshError: String,
    val networks: List<WifiNetwork>, val known: List<WifiNetwork>,
    val meshAvailable: Boolean, val runtimeAvailable: Boolean, val meshConfig: Int, val peers: Long,
    val meshState: String, val meshDetail: String, val gateway: String, val topology: MeshTopology,
) {
    /** Networks shown as available: one row per SSID, saved and connected state merged in, connected first. */
    fun visibleNetworks(): List<WifiNetwork> {
        val rows = networks.map { n -> n.copy(known = n.known || known.any { it.ssid == n.ssid }, connected = n.connected || (wifiConnected && ssid == n.ssid)) }.toMutableList()
        if (wifiConnected && rows.none { it.ssid == ssid }) rows.add(0, WifiNetwork(ssid, known.firstOrNull { it.ssid == ssid }?.security ?: 0, null, known.any { it.ssid == ssid }, true, 0))
        return rows.sortedByDescending { it.connected }
    }
    fun savedOnly(): List<WifiNetwork> { val visible = visibleNetworks(); return known.filter { k -> visible.none { it.ssid == k.ssid } } }

    companion object {
        // Bits follow the iOS bridge: participate, roam, share uplink, Wi-Fi Aware, Bluetooth, Ethernet, Wi-Fi network.
        val meshKeys = listOf("participate", "roam", "share_uplink", "nan", "ble", "ethernet", "infrastructure_wifi")

        private fun wifiRows(items: List<JSONObject>, known: Boolean): List<WifiNetwork> {
            val rows = items.map { WifiNetwork(it.text("ssid"), it.number("security").toInt(), if (known || !it.has("signal_strength")) null else it.number("signal_strength").toInt(),
                known || it.flag("is_known"), !known && it.flag("is_connected"), it.number("priority").toInt()) }
            // An SSID may be reported by several access points. Keep the strongest observation.
            return rows.filter { it.ssid.isNotEmpty() }.groupBy { it.id }.values.map { group -> group.maxBy { it.signal ?: -1 } }
                .sortedWith(compareByDescending<WifiNetwork> { it.connected }.thenByDescending { it.signal ?: -1 }.thenBy(String.CASE_INSENSITIVE_ORDER) { it.ssid })
        }

        fun parse(value: JSONObject): NetworkSnapshot {
            val wifi = value.optJSONObject("WiFi")
            val mesh = value.optJSONObject("Mesh")
            val configured = mesh?.optJSONObject("configured")
            var config = 0
            meshKeys.forEachIndexed { index, key -> if (configured?.flag(key) == true) config = config or (1 shl index) }
            val topology = value.optJSONObject("Topology")
            val names = topology?.list("announcements")?.associate { it.text("principal") to it.text("name") } ?: emptyMap()
            return NetworkSnapshot(
                wifiAvailable = wifi != null && value.text("WiFiError").isEmpty(), wifiConnected = wifi?.flag("connected") == true, ssid = wifi?.text("ssid") ?: "",
                wifiError = value.text("WiFiError"), scanError = value.text("ScanError"), knownError = value.text("KnownError"), meshError = value.text("MeshError"),
                networks = wifiRows(value.list("Networks"), false), known = wifiRows(value.list("Known"), true),
                meshAvailable = configured != null, runtimeAvailable = mesh?.flag("runtime_available") == true, meshConfig = config,
                peers = mesh?.number("authenticated_peers") ?: 0, meshState = mesh?.text("state") ?: "", meshDetail = mesh?.text("detail") ?: "", gateway = mesh?.text("gateway_principal") ?: "",
                topology = MeshTopology(
                    available = topology != null, runtime = topology?.flag("runtime_available") == true, truncated = topology?.flag("truncated") == true,
                    capturedMs = topology?.number("captured_at_unix_ms") ?: 0, error = value.text("TopologyError"),
                    principal = topology?.text("self_principal") ?: "", address = topology?.text("self_mesh_address") ?: "",
                    links = topology?.list("peers")?.map { MeshLink(it.number("link_id"), it.text("principal"), it.text("device_id"), it.text("mesh_address"), it.text("transport"), names[it.text("principal")] ?: "", it.number("cost")) } ?: emptyList(),
                    routes = topology?.list("routes")?.mapIndexed { i, r -> MeshRoute(i, r.text("prefix"), r.text("origin_principal"), r.text("next_hop_principal"), r.number("metric"), r.flag("local"), r.flag("unreachable"), r.flag("origin_authorized")) } ?: emptyList(),
                    announcements = topology?.list("announcements")?.map { MeshAnnouncement(it.text("principal"), it.text("device_id"), it.text("name"), it.text("prefix")) } ?: emptyList(),
                ),
            )
        }
    }
}

data class MeshGraphNode(val id: String, val title: String, val address: String, val direct: Boolean, val nextHop: String?, val subtitle: String)

/** A snapshot lists direct links and routes via a next hop; it cannot show links beyond that hop. */
class MeshGraph(topology: MeshTopology) {
    val nodes: List<MeshGraphNode>
    val direct get() = nodes.filter { it.direct }
    val indirect get() = nodes.filter { !it.direct && it.nextHop != null }
    val unrouted get() = nodes.filter { !it.direct && it.nextHop == null }
    init {
        val links = topology.links.groupBy { it.principal }
        val announcements = topology.announcements.groupBy { it.principal }
        val routes = topology.routes.filter { !it.local }.groupBy { it.origin }
        val principals = (links.keys + announcements.keys + routes.keys - setOf("", topology.principal)).sorted()
        fun title(principal: String): String = announcements[principal]?.firstOrNull { it.name.isNotEmpty() }?.name
            ?: links[principal]?.firstOrNull()?.title
            ?: announcements[principal]?.firstOrNull { it.deviceID.isNotEmpty() }?.deviceID
            ?: principal.substringAfterLast('/')
        nodes = principals.map { principal ->
            val directLinks = links[principal].orEmpty()
            val originRoutes = routes[principal].orEmpty()
            val route = originRoutes.filter { it.reachable && links[it.nextHop] != null && it.nextHop != principal }
                .sortedWith(compareBy<MeshRoute> { it.metric }.thenBy { it.nextHop }.thenBy { it.id }).firstOrNull()
            val nextHop = if (directLinks.isEmpty()) route?.nextHop else null
            val subtitle = when {
                directLinks.isNotEmpty() -> directLinks.map { it.transport.ifEmpty { "Unknown transport" } }.toSortedSet().joinToString(", ")
                nextHop != null -> "Via ${title(nextHop)}"
                originRoutes.isNotEmpty() && originRoutes.all { !it.reachable } -> "Unreachable"
                originRoutes.isNotEmpty() -> "Next hop unavailable"
                else -> "No route reported"
            }
            val address = directLinks.firstOrNull { it.address.isNotEmpty() }?.address
                ?: announcements[principal]?.firstOrNull { it.prefix.isNotEmpty() }?.prefix ?: originRoutes.firstOrNull()?.prefix ?: ""
            MeshGraphNode(principal, title(principal), address, directLinks.isNotEmpty(), nextHop, subtitle)
        }
    }
}

/** Enrollment rules shared with iOS DeviceEnrollmentForm. */
object EnrollmentForm {
    fun suggestedName(displayName: String): String {
        val folded = Normalizer.normalize(displayName, Normalizer.Form.NFD).replace(Regex("\\p{M}+"), "").lowercase(Locale.ROOT)
        var name = folded.split(Regex("[^a-z0-9]+")).filter { it.isNotEmpty() }.joinToString("-")
        if (name.isEmpty()) name = "wendy-device"
        if (name.first().isDigit()) name = "device-$name"
        return name.take(63).trim('-')
    }
    fun validName(name: String) = Regex("^[a-z]([a-z0-9-]{0,61}[a-z0-9])?$").matches(name)
    /** Only explicit pre-mint refusals prove that a retry cannot spend a second credential. */
    fun canRetry(message: String): Boolean {
        val prefix = "Cloud refused this enrollment before minting anything, so no credential was spent: rpc error: code = "
        return listOf("AlreadyExists", "InvalidArgument").any { message.startsWith("$prefix$it desc = ") }
    }
    /** The device failed after Cloud reserved the name; joining Wi-Fi must not start another registration. */
    fun needsWiFiRecovery(message: String): Boolean {
        val value = message.lowercase()
        return value.contains("device pki enrollment failed") && listOf("network is unreachable", "no route to host", "network is down", "temporary failure in name resolution", "no such host", "i/o timeout").any(value::contains)
    }
    fun defaultOrganization(organizations: List<JSONObject>, account: String): String =
        organizations.firstOrNull { it.text("Account") == account }?.text("Account") ?: if (organizations.size == 1) organizations[0].text("Account") else ""
}

data class UpdateSnapshot(val installed: String, val latest: String, val phase: String, val detail: String, val available: Boolean, val downloaded: Long, val total: Long) {
    val busy get() = phase in setOf("downloading", "installing", "restarting")
    companion object {
        fun parse(value: JSONObject) = UpdateSnapshot(value.text("installed_version"), value.text("latest_version"), value.text("phase"), value.text("detail"),
            value.flag("update_available"), value.number("downloaded_bytes"), value.number("total_bytes"))
    }
}

enum class LogLevel(val title: String) { Unknown("Default"), Trace("Trace"), Debug("Debug"), Info("Info"), Warning("Warning"), Error("Error"), Fatal("Fault") }

data class LogEntry(val id: Long, val timestamp: Long, val level: LogLevel, val body: String, val app: String, val service: String) {
    val time: String get() = if (timestamp == 0L) "—" else SimpleDateFormat("HH:mm:ss.SSS", Locale.getDefault()).format(Date(timestamp / 1_000_000))
    val message: String get() = body.replace(Regex("\u001B\\[[0-?]*[ -/]*[@-~]"), "").trim('\n', '\r')
    val source: String get() = listOf(app, service).filter { it.isNotEmpty() }.joinToString(" / ")
    val copyText: String get() = listOf(time, level.title, source, message).filter { it.isNotEmpty() }.joinToString("  ")
    companion object {
        fun parse(id: Long, value: JSONObject) = LogEntry(id, value.optLong("Timestamp"), LogLevel.entries.getOrElse(value.number("Level").toInt()) { LogLevel.Unknown },
            value.text("Body"), value.text("App"), value.text("Service"))
    }
}

enum class AppState(val title: String) {
    Stopped("Stopped"), Running("Running"), CrashLooping("Crash looping"), Unknown("Unknown");
    companion object { fun of(value: Long) = when (value) { 0L -> Stopped; 1L -> Running; 2L -> CrashLooping; else -> Unknown } }
}

data class DeviceApp(val name: String, val version: String, val state: AppState, val httpPort: Long, val terminationReason: String, val services: List<Pair<String, AppState>>) {
    val hasWebpage get() = httpPort != 0L
    companion object {
        fun parse(value: JSONObject) = DeviceApp(value.text("app_name"), value.text("app_version"), AppState.of(value.number("running_state")), value.number("http_port"),
            value.text("termination_reason"), value.list("services").map { it.text("name") to AppState.of(it.number("running_state")) })
    }
}

class ResourceSnapshot(value: JSONObject) {
    data class Gpu(val index: Long, val name: String, val utilization: Double, val memoryUsed: Long, val memoryTotal: Long, val temperature: Double?, val power: Double?)
    data class Row(val id: String, val app: String, val name: String, val state: AppState, val cpu: Double?, val memory: Long, val isService: Boolean)
    data class Battery(val percent: Double, val state: String, val seconds: Long?)
    private val host = value.optJSONObject("Host")
    val hasHost = host != null
    val cpu: Double? = if (host != null && value.flag("HasCPU")) value.optDouble("CPUPercent").takeIf { it.isFinite() } else null
    val cpuCount = host?.number("cpu_count") ?: 0
    val memoryTotal = host?.number("mem_total_bytes") ?: 0
    val memoryUsed = (memoryTotal - (host?.number("mem_available_bytes") ?: 0)).coerceAtLeast(0)
    private val disk = value.optJSONObject("Storage")
    val hasDisk = disk != null
    val diskMount = disk?.text("mountpoint") ?: ""
    val diskUsed = disk?.number("used_bytes") ?: 0
    val diskTotal = disk?.number("total_bytes") ?: 0
    val diskError = value.text("StorageError")
    val gpus = host?.list("gpus")?.map { Gpu(it.number("index"), it.text("name"), it.optDouble("util_percent", 0.0), it.number("mem_used_bytes"), it.number("mem_total_bytes"),
        it.optDouble("temp_c").takeIf { _ -> it.has("temp_c") }, it.optDouble("power_w").takeIf { _ -> it.has("power_w") }) } ?: emptyList()
    val thermals = host?.list("thermal_zones")?.map { it.text("name") to it.optDouble("temp_c", 0.0) } ?: emptyList()
    val battery = host?.optJSONObject("battery")?.let { b ->
        val states = listOf("Unknown", "Charging", "On battery", "Full", "Not charging")
        Battery(b.optDouble("percent", 0.0), states.getOrElse(b.number("state").toInt()) { "Unknown" }, if (b.has("seconds_remaining")) b.number("seconds_remaining") else null)
    }
    val rows = value.list("Rows").map { Row(it.text("ID"), it.text("App"), it.text("Name"), AppState.of(it.number("State")), if (it.flag("HasCPU")) it.optDouble("CPUPercent") else null, it.number("MemoryBytes"), it.flag("IsService")) }
    val hasTemperatureDetail get() = thermals.isNotEmpty() || gpus.any { it.temperature != null || it.power != null }
}

data class TrafficRow(val name: String, val virtual: Boolean, val loopback: Boolean, val agent: Boolean, val received: Long, val sent: Long, val receiveRate: Double?, val sendRate: Double?)
class TrafficSnapshot(value: JSONObject) {
    private fun row(it: JSONObject, agent: Boolean) = TrafficRow(it.text("name"), it.flag("virtual"), it.flag("loopback"), agent, it.number("receivedBytes"), it.number("sentBytes"),
        if (it.has("receiveBytesPerSecond")) it.optDouble("receiveBytesPerSecond") else null, if (it.has("sendBytesPerSecond")) it.optDouble("sendBytesPerSecond") else null)
    val rows = value.list("interfaces").map { row(it, false) } + listOfNotNull(value.optJSONObject("agent")?.let { row(it, true) })
    val interfacesError = value.text("interfacesUnavailableReason")
    val agentError = value.text("agentUnavailableReason")
}

fun percent(value: Double?) = value?.let { "%.1f%%".format(it) } ?: "—"

/** Binary units, as the iOS traffic view prints them. */
fun binarySize(value: Double): String {
    var amount = maxOf(0.0, value)
    val units = listOf("B", "KiB", "MiB", "GiB", "TiB", "PiB", "EiB")
    var index = 0
    while (amount >= 1024 && index < units.lastIndex) { amount /= 1024; index++ }
    return if (index == 0) "%.0f %s".format(amount, units[index]) else "%.1f %s".format(amount, units[index])
}
