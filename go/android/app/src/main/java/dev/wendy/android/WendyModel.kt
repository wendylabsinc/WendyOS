package dev.wendy.android

import android.app.Application
import android.content.Intent
import androidx.core.content.ContextCompat
import androidx.compose.runtime.*
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.*
import kotlinx.coroutines.flow.first
import org.json.JSONArray
import org.json.JSONObject
import java.util.concurrent.atomic.AtomicLong

/** One line of the Activity tab: a connection or an app state change, as on iOS. */
data class ExperienceEvent(val id: Long, val deviceID: String, val deviceName: String, val title: String, val kind: String, val time: Long = System.currentTimeMillis())

class WendyModel(app: Application) : AndroidViewModel(app) {
    val backend = (app as WendyApplication).backend
    var initialized by mutableStateOf(false); private set
    // Nearby (LAN, mesh, USB) and Cloud devices are separate lists, as on iOS.
    var nearbyDevices by mutableStateOf(emptyList<Device>()); private set
    var cloudDevices by mutableStateOf(emptyList<Device>()); private set
    var nearbyError by mutableStateOf<String?>(null); private set
    var cloudError by mutableStateOf<String?>(null); private set
    var nearbyLoaded by mutableStateOf(false); private set
    var cloudLoaded by mutableStateOf(false); private set
    var nearbyLoading by mutableStateOf(false); private set
    var cloudLoading by mutableStateOf(false); private set
    /** Cloud presence for the selected organization, with when it was read; stale after a minute. */
    var presence by mutableStateOf<List<FleetDevice>?>(null); private set
    var presenceError by mutableStateOf(""); private set
    var presenceAt by mutableLongStateOf(0L); private set
    val devices get() = (nearbyDevices + cloudDevices).distinctBy { it.selector }
    var selected by mutableStateOf<Device?>(null); private set
    var session by mutableLongStateOf(0); private set
    var lite by mutableStateOf(false); private set
    var connecting by mutableStateOf(false); private set
    /** The last connection failure for [selected]. The device page shows it instead of an alert. */
    var detailError by mutableStateOf<String?>(null); private set
    var canForgetIdentity by mutableStateOf(false); private set
    var error by mutableStateOf<String?>(null)
    var busy by mutableIntStateOf(0); private set
    var account by mutableStateOf(""); private set
    var organization by mutableStateOf(""); private set
    var accounts by mutableStateOf(emptyList<JSONObject>()); private set
    var organizations by mutableStateOf(emptyList<JSONObject>()); private set
    var browserURL by mutableStateOf<String?>(null)
    var appURL by mutableStateOf<String?>(null)
    /** A link the activity should open; loopback app links stay in the in-app browser. */
    var linkRequest by mutableStateOf<String?>(null)
    var loginPending by mutableStateOf(false); private set
    var requestNearbySetup by mutableStateOf(false)
    /** Home asks the root to present Settings or Ask Wendy, as iOS presents them as sheets. */
    var requestSettings by mutableStateOf(false)
    var requestChat by mutableStateOf(false)
    /** Increments each time the app returns to the foreground, so pages can refresh. */
    var foregroundGeneration by mutableIntStateOf(0); private set
    var experienceEvents by mutableStateOf(emptyList<ExperienceEvent>()); private set

    // Apps on the selected device.
    var apps by mutableStateOf(emptyList<DeviceApp>()); private set
    var appsError by mutableStateOf<String?>(null); private set
    var appActions by mutableStateOf(emptySet<String>()); private set
    var appDeletion by mutableStateOf<String?>(null)
    var showHiddenApps by mutableStateOf(false)
    private var hiddenApps by mutableStateOf(emptyMap<String, Set<String>>())
    val hiddenAppCount get() = apps.count { isAppHidden(it.name) }
    val visibleApps get() = apps.filter { showHiddenApps || !isAppHidden(it.name) }

    // Cameras on the selected device. Tiles own their streams.
    var cameras by mutableStateOf(emptyList<JSONObject>()); private set
    var camerasError by mutableStateOf<String?>(null); private set
    val cameraEvents = mutableStateMapOf<Long, JSONObject>()
    val videoConsumers = mutableMapOf<Long, (JSONObject) -> Unit>()

    /** The selected device's conversation; it survives switching panes. */
    var deviceChat by mutableStateOf<DeviceChat?>(null); private set

    // Logs. One stream at a time, filtered by [logApp].
    var logApp by mutableStateOf("")
    var logs by mutableStateOf(emptyList<LogEntry>())
    var logError by mutableStateOf<String?>(null); private set
    private var logStream = 0L

    private val ids = AtomicLong(1)
    private var connectionGeneration = 0L
    /** True while the activity is started; background work stops otherwise. */
    var inForeground by mutableStateOf(false); private set
    private var lifecycleJob: Job? = null
    private var eventJob: Job? = null
    private var maintainJob: Job? = null
    private val preferences = app.getSharedPreferences("selection", 0)
    private val appPreferences = app.getSharedPreferences("apps", 0)
    /** The lens Home is built around; chosen on first launch and in Settings (iOS wendy.primaryLens). */
    var primaryLens by mutableStateOf(Lens.of(preferences.getString("primaryLens", null))); private set
    fun choosePrimaryLens(lens: Lens) { primaryLens = lens; preferences.edit().putString("primaryLens", lens.name).apply() }
    /** System, Light or Dark (iOS wendy.appearance). */
    var appearance by mutableStateOf(preferences.getString("appearance", "System") ?: "System"); private set
    fun chooseAppearance(value: String) { appearance = value; preferences.edit().putString("appearance", value).apply() }

    init {
        hiddenApps = appPreferences.all.mapNotNull { (key, value) -> (value as? String)?.let { key to JSONArray(it).let { a -> (0 until a.length()).map(a::optString).toSet() } } }.toMap()
        viewModelScope.launch {
            AuthSessionService.running.collect { running ->
                if (!running) { loginPending = false; browserURL = null }
                updatePolling()
            }
        }
        viewModelScope.launch {
            AuthSessionService.failure.collect { if (it != null) error = it }
        }
        viewModelScope.launch {
            try {
                backend.initialize(); initialized = true
                account = preferences.getString("account", "") ?: ""
                organization = preferences.getString("organization", "") ?: ""
                refreshAccounts()
                if (inForeground) activate()
            } catch (e: Exception) { error = e.message }
        }
    }
    fun nextID(): Long = ids.getAndIncrement()
    fun task(block: suspend () -> Unit) {
        viewModelScope.launch {
            busy++
            try { block() } catch (e: CancellationException) { throw e }
            catch (e: Exception) { error = e.message ?: "Request failed" }
            finally { busy-- }
        }
    }
    private fun recordExperience(title: String, kind: String, device: Device) {
        val recent = experienceEvents.firstOrNull()
        if (recent != null && recent.deviceID == device.id && recent.title == title && System.currentTimeMillis() - recent.time < 60_000) return
        experienceEvents = (listOf(ExperienceEvent(nextID(), device.id, device.title, title, kind)) + experienceEvents).take(100)
    }
    /** Records a user action on the selected device in the Activity tab. */
    fun record(message: String) { selected?.let { recordExperience(message, "action", it) } }

    suspend fun refreshAccounts() {
        accounts = backend.call("Accounts").items()
        if (accounts.none { it.text("Context") == account }) account = accounts.firstOrNull { it.flag("Active") }?.text("Context") ?: accounts.firstOrNull()?.text("Context") ?: ""
        organizations = if (account.isBlank()) emptyList() else backend.call("Organizations", account).items()
        if (organizations.none { it.text("id") == organization }) organization = organizations.firstOrNull()?.text("id") ?: ""
    }
    fun chooseAccount(id: String, org: String = "") = task {
        check(!WendyVpnService.engaged) { "Disconnect the Wendy VPN before changing accounts or organizations." }
        invalidateConnection()
        cloudDevices = emptyList(); cloudLoaded = false; cloudError = null; presence = null
        account = id; organization = org
        preferences.edit().putString("account", id).putString("organization", org).apply()
        refreshAccounts()
        backend.call("SetMobileState", inForeground, account, organization)
        refresh()
    }
    suspend fun refresh() { refreshNearby(); if (organization.isNotEmpty()) refreshCloud() }
    suspend fun refreshNearby() {
        nearbyLoading = true
        try { nearbyDevices = backend.call("Nearby").items().map(Device::parse); nearbyError = null; nearbyLoaded = true }
        catch (e: CancellationException) { throw e } catch (e: Exception) { nearbyError = e.message } finally { nearbyLoading = false }
    }
    suspend fun refreshCloud() {
        val account = account; val org = organization
        if (org.isEmpty()) { cloudDevices = emptyList(); cloudLoaded = false; presence = null; return }
        cloudLoading = true
        try {
            val values = backend.call("Cloud").items().map(Device::parse)
            if (account == this.account && org == organization) { cloudDevices = values; cloudError = null; cloudLoaded = true }
        } catch (e: CancellationException) { throw e } catch (e: Exception) { if (org == organization) cloudError = e.message } finally { cloudLoading = false }
        try {
            val fleet = backend.call("Fleet", account, org).obj()
            if (account == this.account && org == organization) {
                presence = fleet.list("Devices").map { FleetDevice(it.text("ID"), it.text("Name"), it.text("Selector"), it.text("SiteID"), it.text("Site"), it.text("Hardware"), it.text("Address"), it.flag("Online")) }
                presenceError = fleet.text("PresenceError"); presenceAt = System.currentTimeMillis()
            }
        } catch (e: CancellationException) { throw e } catch (_: Exception) { presence = null }
    }
    /** Whether Cloud presence is fresh enough to call a device offline (iOS: under a minute old, no presence error). */
    fun isOffline(device: Device): Boolean {
        if (!device.selector.startsWith("cloud://") || selected?.selector == device.selector && session != 0L) return false
        val rows = presence ?: return false
        if (cloudError != null || presenceError.isNotEmpty() || System.currentTimeMillis() - presenceAt > 60_000) return false
        return rows.firstOrNull { it.selector == device.selector }?.online == false
    }
    /** Finds a device in the current list, then in Cloud and nearby discovery, without changing the visible list. */
    suspend fun lookupDevice(match: (Device) -> Boolean): Device? {
        devices.firstOrNull(match)?.let { return it }
        for (method in listOf("Cloud", "Nearby")) {
            val found = try { backend.call(method).items().map(Device::parse).firstOrNull(match) } catch (e: CancellationException) { throw e } catch (_: Exception) { null }
            if (found != null) return found
        }
        return null
    }


    fun connect(device: Device, forget: Boolean = false) {
        val epoch = ++connectionGeneration
        val sameDevice = selected?.selector == device.selector && session != 0L
        if (sameDevice && !forget) return
        selected = device; session = 0; connecting = true; detailError = null; canForgetIdentity = false
        clearDeviceState()
        viewModelScope.launch {
            try {
                val result = backend.call("Connect", device.selector, forget).obj()
                if (epoch != connectionGeneration || !inForeground) return@launch
                lite = result.flag("lite")
                val resolved = device.copy(lite = lite)
                selected = resolved
                session = result.number("session")
                deviceChat = DeviceChat(this@WendyModel, session, viewModelScope)
                recordExperience("Connected", "connected", resolved)
                refreshDetails()
            } catch (e: CancellationException) { throw e }
            catch (e: Exception) {
                if (epoch != connectionGeneration) return@launch
                canForgetIdentity = (e as? BackendFailure)?.identityRecovery == true
                detailError = e.message ?: "Could not connect"
            } finally { if (epoch == connectionGeneration) connecting = false }
        }
    }
    /** Refreshes the device's address through discovery, then reconnects (iOS retryConnection). */
    fun retryConnection() {
        val device = selected ?: return
        val epoch = connectionGeneration
        connecting = true; detailError = null
        viewModelScope.launch {
            try {
                val refreshed = backend.call("Nearby").items().map(Device::parse)
                if (epoch != connectionGeneration) return@launch
                nearbyDevices = refreshed
                val current = refreshed.firstOrNull { it.id == device.id } ?: device
                session = 0
                connect(current, false)
            } catch (e: CancellationException) { throw e } catch (e: Exception) {
                if (epoch == connectionGeneration) { connecting = false; detailError = e.message }
            }
        }
    }
    /** Reconnects to the selected device, for example after it restarted. */
    fun reconnect() { val device = selected ?: return; session = 0; connect(device) }
    private fun clearDeviceState() {
        appURL = null; cameraEvents.clear(); apps = emptyList(); appsError = null; cameras = emptyList(); camerasError = null
        logs = emptyList(); logError = null; appActions = emptySet()
        deviceChat?.stop(); deviceChat = null
    }
    private fun invalidateConnection() { connectionGeneration++; session = 0; connecting = false; clearDeviceState() }
    fun disconnect() { invalidateConnection(); selected = null; detailError = null; task { backend.call("Disconnect") } }

    /** Reloads the app and camera lists and records app state changes in Activity. */
    suspend fun refreshDetails() {
        val captured = session
        if (captured == 0L) return
        try {
            val result = if (lite) emptyList() else backend.call("Apps").items().map(DeviceApp::parse)
            if (session != captured) return
            val previous = apps.associate { it.name to it.state }
            selected?.let { device -> result.filter { previous[it.name] != null && previous[it.name] != it.state }.forEach { recordExperience("${it.name} · ${it.state.title.lowercase()}", "app", device) } }
            apps = result; appsError = null
        } catch (e: CancellationException) { throw e } catch (e: Exception) { if (session == captured) appsError = e.message }
        try {
            val result = backend.call("Cameras").items()
            if (session == captured) { cameras = result; camerasError = null }
        } catch (e: CancellationException) { throw e } catch (e: Exception) { if (session == captured) camerasError = e.message }
    }

    // MARK: Apps
    private fun deviceKey() = selected?.id ?: ""
    fun isAppHidden(app: String) = hiddenApps[deviceKey()]?.contains(app) == true
    fun setAppHidden(app: String, hidden: Boolean) {
        val key = deviceKey().ifEmpty { return }
        val names = (hiddenApps[key] ?: emptySet()).let { if (hidden) it + app else it - app }
        hiddenApps = hiddenApps + (key to names)
        appPreferences.edit().putString(key, JSONArray(names.sorted()).toString()).apply()
    }
    fun setAppRunning(app: String, running: Boolean) {
        if (app in appActions) return
        val captured = session
        appActions = appActions + app
        task {
            try {
                backend.call(if (running) "StartApp" else "StopApp", app, captured)
                if (session == captured) refreshDetails()
            } finally { appActions = appActions - app }
        }
    }
    /** Stops then starts an app on the same session (iOS restartApp). */
    fun restartApp(app: String) {
        if (app in appActions) return
        val captured = session
        appActions = appActions + app
        task {
            try {
                backend.call("StopApp", app, captured)
                if (session == captured) backend.call("StartApp", app, captured)
                if (session == captured) refreshDetails()
            } finally { appActions = appActions - app }
        }
    }
    fun requestAppDeletion(app: String) { appDeletion = app }
    fun deleteApp(app: String) {
        val captured = session
        appActions = appActions + app
        task {
            try {
                backend.call("DeleteApp", app, captured)
                record("Removed $app")
                if (session == captured) refreshDetails()
            } finally { appActions = appActions - app }
        }
    }
    /** Opens [app] once the connection [connect] just started is ready (iOS widget portals). */
    fun openAppWhenReady(app: String) {
        val epoch = connectionGeneration
        viewModelScope.launch {
            withTimeoutOrNull(70_000) { snapshotFlow { Triple(session, connecting, detailError) }.first { (s, c, e) -> epoch != connectionGeneration || s != 0L || (!c && e != null) } }
            if (epoch == connectionGeneration && session != 0L && !lite) openApp(app)
        }
    }
    fun openApp(app: String) = task {
        val captured = session
        val url = backend.call("OpenApp", app, captured) as? String ?: return@task
        if (session == captured) linkRequest = url
    }

    // MARK: Logs
    /** Streams logs for [logApp] while the caller is active, reconnecting after transient failures. */
    suspend fun observeLogs(clear: Boolean = true) {
        val captured = session
        suspend fun start(clearEntries: Boolean): Long {
            if (clearEntries) logs = emptyList()
            logError = null
            val stream = nextID()
            logStream = stream
            try { backend.call("StartLogs", logApp, stream) } catch (e: CancellationException) { throw e } catch (e: Exception) { if (logStream == stream) logError = e.message }
            return stream
        }
        var owned = start(clear)
        try {
            while (currentCoroutineContext().isActive && session == captured && logStream == owned) {
                delay(5_000)
                val failure = logError
                if (failure != null && ConnectionRecovery.canRetry(failure)) owned = start(false)
            }
        } finally {
            if (logStream == owned) { logStream = 0; withContext(NonCancellable) { try { backend.call("StopLogs") } catch (_: Exception) {} } }
        }
    }

    fun signIn(development: Boolean = false) {
        if (loginPending || AuthSessionService.running.value) return
        loginPending = true
        task {
            try {
                val app = getApplication<WendyApplication>()
                ContextCompat.startForegroundService(app, Intent(app, AuthSessionService::class.java))
                // Do not open the browser until Android grants foreground service network access.
                withTimeout(5_000) { AuthSessionService.running.first { it } }
                backend.call("Login", development)
                updatePolling()
            } catch (e: Exception) {
                loginPending = false
                AuthSessionService.cancelLogin()
                throw e
            }
        }
    }
    fun cancelLogin() { browserURL = null; AuthSessionService.cancelLogin() }
    fun setForeground(active: Boolean) {
        inForeground = active
        getApplication<WendyApplication>().visible = active
        if (!initialized) return
        updatePolling()
        lifecycleJob?.cancel()
        maintainJob?.cancel()
        if (active) activate() else {
            invalidateConnection()
            lifecycleJob = viewModelScope.launch { try { backend.call("SetMobileState", false, account, organization); backend.call("NearbyClose", 0) } catch (e: Exception) { error = e.message } }
        }
    }
    private fun activate() {
        val previous = lifecycleJob
        foregroundGeneration++
        lifecycleJob = viewModelScope.launch {
            previous?.join()
            try {
                refreshAccounts()
                backend.call("SetMobileState", true, account, organization)

                launch { refresh() }
                selected?.let { connect(it) }
                updatePolling()
            } catch (e: CancellationException) { throw e }
            catch (e: Exception) { error = e.message }
        }
        maintainJob = viewModelScope.launch { maintainExperience() }
    }
    /** Keeps the selected device and lists current while visible (iOS maintainExperience). */
    private suspend fun maintainExperience() {
        var failures = 0
        var lastDiscovery = 0L
        while (currentCoroutineContext().isActive && inForeground) {
            delay(if (failures == 0) 15_000L else minOf(30_000L, 1000L shl minOf(failures, 5)))
            val device = selected
            val failure = detailError
            if (device != null && !connecting && failure != null && ConnectionRecovery.canRetry(failure)) {
                retryConnection()
                failures++
            } else if (session != 0L) {
                failures = 0
                val captured = session
                refreshDetails()
                val appError = appsError; val cameraError = camerasError
                if (session == captured && device != null && appError != null && cameraError != null && ConnectionRecovery.canRetry(appError) && ConnectionRecovery.canRetry(cameraError)) {
                    session = 0; connect(device)
                }
            } else failures = 0
            if (System.currentTimeMillis() - lastDiscovery >= 30_000) { refresh(); lastDiscovery = System.currentTimeMillis() }
        }
    }
    private fun updatePolling() {
        if (!initialized || (!inForeground && !loginPending)) {
            eventJob?.cancel(); eventJob = null
        } else if (eventJob?.isActive != true) {
            eventJob = viewModelScope.launch {
                while (isActive && (inForeground || loginPending)) {
                    try { poll() }
                    catch (e: CancellationException) { throw e }
                    catch (e: Exception) { error = e.message; delay(1_000) }
                    delay(40)
                }
            }
        }
    }
    private suspend fun poll() {
        for (event in backend.call("Poll").items()) {
            when (event.number("Kind").toInt()) {
                1 -> if (loginPending) browserURL = event.text("Text")
                2 -> {
                    try {
                        if (inForeground) { refreshAccounts(); backend.call("SetMobileState", true, account, organization) }
                    } finally { loginPending = false; AuthSessionService.complete() }
                }
                3 -> { loginPending = false; error = event.text("Text"); AuthSessionService.complete() }
                4 -> if (event.number("Session") == session && event.number("Stream") == logStream) {
                    logs = (logs + LogEntry.parse(nextID(), event.child("Log"))).takeLast(2000)
                }
                5 -> if (event.number("Session") == session && event.number("Stream") == logStream) logError = event.text("Text")
                10 -> error = event.text("Text")
                6, 7, 8, 9 -> if (event.number("Session") == session) { cameraEvents[event.number("Stream")] = event; videoConsumers[event.number("Stream")]?.invoke(event) }
            }
        }
    }
    override fun onCleared() { AuthSessionService.cancelLogin(); super.onCleared() }
}
