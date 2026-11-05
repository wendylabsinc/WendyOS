package dev.wendy.android

import android.app.Application
import android.content.Intent
import androidx.core.content.ContextCompat
import androidx.compose.runtime.*
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import kotlinx.coroutines.*
import kotlinx.coroutines.flow.first
import org.json.JSONObject
import java.util.concurrent.atomic.AtomicLong

class WendyModel(app: Application) : AndroidViewModel(app) {
    val backend = (app as WendyApplication).backend
    var initialized by mutableStateOf(false); private set
    var devices by mutableStateOf(emptyList<Device>()); private set
    var selected by mutableStateOf<Device?>(null); private set
    var session by mutableLongStateOf(0); private set
    var lite by mutableStateOf(false); private set
    var source by mutableStateOf("Nearby")
    var error by mutableStateOf<String?>(null)
    var recovery by mutableStateOf<Device?>(null)
    var busy by mutableIntStateOf(0); private set
    var account by mutableStateOf(""); private set
    var organization by mutableStateOf(""); private set
    var accounts by mutableStateOf(emptyList<JSONObject>()); private set
    var organizations by mutableStateOf(emptyList<JSONObject>()); private set
    var browserURL by mutableStateOf<String?>(null)
    var appURL by mutableStateOf<String?>(null)
    var loginPending by mutableStateOf(false); private set
    var logs by mutableStateOf(emptyList<JSONObject>()); private set
    var activity by mutableStateOf(emptyList<String>()); private set
    val cameraEvents = mutableStateMapOf<Long, JSONObject>()
    val videoConsumers = mutableMapOf<Long, (JSONObject) -> Unit>()
    private var logStream = 0L
    fun beginLogs(stream: Long) { logStream = stream; logs = emptyList() }
    fun stopLogs(stream: Long) { if (logStream == stream) { logStream = 0; task { backend.call("StopLogs") } } }
    private val ids = AtomicLong(1)
    private var connectionGeneration = 0L
    private var foreground = false
    private var lifecycleJob: Job? = null
    private var eventJob: Job? = null
    private val preferences = app.getSharedPreferences("selection", 0)
    

    init {
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
                if (foreground) activate()
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
    fun record(message: String) { activity = (listOf(message) + activity).take(200) }
    suspend fun refreshAccounts() {
        accounts = backend.call("Accounts").items()
        if (accounts.none { it.text("Context") == account }) account = accounts.firstOrNull { it.flag("Active") }?.text("Context") ?: accounts.firstOrNull()?.text("Context") ?: ""
        organizations = if (account.isBlank()) emptyList() else backend.call("Organizations", account).items()
        if (organizations.none { it.text("id") == organization }) organization = organizations.firstOrNull()?.text("id") ?: ""
    }
    fun chooseAccount(id: String, org: String = "") = task {
        check(!WendyVpnService.engaged) { "Disconnect the Wendy VPN before changing accounts or organizations." }
        invalidateConnection()
        account = id; organization = org
        preferences.edit().putString("account", id).putString("organization", org).apply()
        refreshAccounts()
        backend.call("SetMobileState", foreground, account, organization)
        refresh()
    }
    suspend fun refresh() {
        val requestedSource = source
        val values = backend.call(if (requestedSource == "Cloud") "Cloud" else "Nearby").items().map(Device::parse)
        if (source == requestedSource) devices = values
    }
    fun changeSource(value: String) { source = value; devices = emptyList(); task { refresh() } }
    fun connect(device: Device, forget: Boolean = false) {
        val epoch = ++connectionGeneration
        selected = device; session = 0; cameraEvents.clear(); logs = emptyList()
        task {
            try {
                val result = backend.call("Connect", device.selector, forget).obj()
                if (epoch == connectionGeneration && foreground) {
                    session = result.number("session"); lite = result.flag("lite")
                    record("Connected to ${device.name}")
                }
            } catch (e: BackendFailure) {
                if (epoch == connectionGeneration && e.identityRecovery) recovery = device
                throw e
            }
        }
    }
    private fun invalidateConnection() { appURL = null; connectionGeneration++; session = 0; cameraEvents.clear(); logs = emptyList() }
    fun disconnect() { invalidateConnection(); selected = null; task { backend.call("Disconnect") } }
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
        foreground = active
        getApplication<WendyApplication>().visible = active
        if (!initialized) return
        updatePolling()
        lifecycleJob?.cancel()
        if (active) activate() else {
            invalidateConnection()
            lifecycleJob = viewModelScope.launch { try { backend.call("SetMobileState", false, account, organization); backend.call("NearbyClose", 0) } catch (e: Exception) { error = e.message } }
        }
    }
    private fun activate() {
        val previous = lifecycleJob
        lifecycleJob = viewModelScope.launch {
            previous?.join()
            try {
                refreshAccounts()
                backend.call("SetMobileState", true, account, organization)

                launch { try { refresh() } catch (e: Exception) { error = e.message } }
                selected?.let { connect(it) }
                updatePolling()
            } catch (e: CancellationException) { throw e }
            catch (e: Exception) { error = e.message }
        }
    }
    private fun updatePolling() {
        if (!initialized || (!foreground && !loginPending)) {
            eventJob?.cancel(); eventJob = null
        } else if (eventJob?.isActive != true) {
            eventJob = viewModelScope.launch {
                while (isActive && (foreground || loginPending)) {
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
                        if (foreground) { refreshAccounts(); backend.call("SetMobileState", true, account, organization) }
                        record("Signed in to Wendy Cloud")
                    } finally { loginPending = false; AuthSessionService.complete() }
                }
                3 -> { loginPending = false; error = event.text("Text"); AuthSessionService.complete() }
                4 -> if (event.number("Session") == session && event.number("Stream") == logStream) logs = (logs + event.child("Log")).takeLast(1000)
                5, 10 -> if (event.number("Session") == session) error = event.text("Text")
                6, 7, 8, 9 -> if (event.number("Session") == session) { cameraEvents[event.number("Stream")] = event; videoConsumers[event.number("Stream")]?.invoke(event) }
            }
        }
    }
    override fun onCleared() { AuthSessionService.cancelLogin(); super.onCleared() }
}
