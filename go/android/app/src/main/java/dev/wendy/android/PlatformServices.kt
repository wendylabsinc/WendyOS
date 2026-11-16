package dev.wendy.android

import android.Manifest
import android.annotation.SuppressLint
import android.bluetooth.*
import android.bluetooth.le.*
import android.content.Context
import android.content.pm.PackageManager
import android.net.nsd.NsdManager
import android.net.nsd.NsdServiceInfo
import android.os.Build
import android.os.Handler
import android.os.Looper
import android.os.ParcelUuid
import android.util.Base64
import org.json.JSONArray
import org.json.JSONObject
import java.io.IOException
import java.util.UUID
import java.util.concurrent.*
import java.util.concurrent.atomic.AtomicLong

/** Blocking, bounded system calls used only on Go worker threads. */
@SuppressLint("MissingPermission")
@Suppress("DEPRECATION", "OVERRIDE_DEPRECATION")
class PlatformServices(private val context: Context) {
    private val ids = AtomicLong(1)
    private val adapter get() = context.getSystemService(BluetoothManager::class.java)?.adapter ?: error("Bluetooth is unavailable")
    private val connections = ConcurrentHashMap<Long, Peripheral>()
    private val scans = ConcurrentHashMap<Long, RadioScan>()
    private val browsers = ConcurrentHashMap<Long, Browser>()
    private val main = Handler(Looper.getMainLooper())
    private val timeouts = Executors.newScheduledThreadPool(2)
    private val nsd = context.getSystemService(NsdManager::class.java)
    private val resolveQueue = ArrayDeque<() -> Unit>()
    private var resolving = false

    fun invoke(raw: ByteArray): ByteArray {
        val response = try {
            val request = JSONObject(raw.toString(Charsets.UTF_8))
            json("value" to dispatch(request.getString("method"), request.optJSONObject("args") ?: JSONObject()))
        } catch (e: Exception) { json("error" to (e.message ?: "Android service request failed")) }
        return response.toString().toByteArray(Charsets.UTF_8)
    }
    private fun checkBluetooth() {
        val permission = if (Build.VERSION.SDK_INT >= 31) Manifest.permission.BLUETOOTH_CONNECT else Manifest.permission.ACCESS_FINE_LOCATION
        check(context.checkSelfPermission(permission) == PackageManager.PERMISSION_GRANTED) { "Allow nearby device access in Wendy to use Bluetooth" }
        check(adapter.isEnabled) { "Turn on Bluetooth to discover nearby devices" }
    }
    private fun dispatch(method: String, args: JSONObject): Any? {
        when (method) {
            "ble.check" -> { checkBluetooth(); return true }
            "ble.scanStart" -> {
                checkBluetooth(); check(scans.size < 16) { "Too many Bluetooth scans" }
                val scan = RadioScan(args.optJSONArray("services")); val id = ids.getAndIncrement(); scans[id] = scan
                try { scan.start() } catch (e: Exception) { scans.remove(id); throw e }; return id
            }
            "ble.scanSnapshot" -> { val scan = scans[args.getLong("id")] ?: error("Bluetooth scan ended"); scan.error?.let { error(it) }; return JSONArray(scan.devices.values.toList()) }
            "ble.scanStop" -> { scans.remove(args.getLong("id"))?.close(); return null }
            "ble.connect" -> {
                checkBluetooth(); check(connections.size < 32) { "Too many Bluetooth connections" }
                val connection = Peripheral(adapter.getRemoteDevice(args.getString("address")))
                try { connection.connect(args.optInt("timeout", 10)) } catch (e: Exception) { connection.close(); throw e }
                val id = ids.getAndIncrement(); connections[id] = connection; return id
            }
            "mdns.start" -> { check(browsers.size < 32) { "Too many network discovery requests" }; val id = ids.getAndIncrement(); val browser = Browser(args.getString("type")); browsers[id] = browser; main.post { browser.start() }; return id }
            "mdns.snapshot" -> { val browser = browsers[args.getLong("id")] ?: error("Discovery ended"); browser.error?.let { error(it) }; return JSONArray(browser.rows.values.toList()) }
            "mdns.stop" -> { browsers.remove(args.getLong("id"))?.let { main.post { it.close() } }; return null }
        }
        val id = args.getLong("id")
        if (method == "ble.close") { connections.remove(id)?.close(); return null }
        val peripheral = connections[id] ?: error("disconnected")
        return peripheral.perform(method.removePrefix("ble."), args)
    }
    private inner class RadioScan(private val services: JSONArray?) : ScanCallback() {
        val devices = ConcurrentHashMap<String, JSONObject>()
        @Volatile var error: String? = null
        fun start() {
            val filters = (0 until (services?.length() ?: 0)).map { ScanFilter.Builder().setServiceUuid(ParcelUuid(uuid(services!!.getString(it)))).build() }
            adapter.bluetoothLeScanner.startScan(filters, ScanSettings.Builder().setScanMode(ScanSettings.SCAN_MODE_LOW_LATENCY).build(), this)
        }
        override fun onScanFailed(code: Int) { error = "Bluetooth scan failed ($code)" }
        override fun onBatchScanResults(results: MutableList<ScanResult>) { results.forEach { onScanResult(0, it) } }
        override fun onScanResult(type: Int, result: ScanResult) {
            val record = result.scanRecord ?: return
            if (devices.size >= 512 && !devices.containsKey(result.device.address)) return
            val data = JSONObject(); record.serviceData?.forEach { (key, value) -> data.put(key.toString().uppercase(), Base64.encodeToString(value, Base64.NO_WRAP)) }
            val serviceList = JSONArray(record.serviceUuids?.map { it.toString().uppercase() } ?: emptyList<String>())
            devices[result.device.address] = json("Address" to result.device.address, "Name" to (record.deviceName ?: ""), "RSSI" to result.rssi, "Seen" to System.currentTimeMillis(), "Services" to serviceList, "Data" to data)
        }
        fun close() { try { adapter.bluetoothLeScanner?.stopScan(this) } catch (_: Exception) {} }
    }
    private data class GattResult(val operation: String, val status: Int, val bytes: ByteArray = ByteArray(0))
    private inner class Peripheral(private val device: BluetoothDevice) {
        private var gatt: BluetoothGatt? = null
        private var socket: BluetoothSocket? = null
        private val results = ArrayBlockingQueue<GattResult>(16)
        private val notifications = ConcurrentHashMap<String, ArrayBlockingQueue<ByteArray>>()
        private val packets = ArrayBlockingQueue<ByteArray>(64)
        private val operationLock = Any()
        @Volatile private var closed = false
        @Volatile private var channelError: String? = null
        private val callback = object : BluetoothGattCallback() {
            override fun onConnectionStateChange(g: BluetoothGatt, status: Int, state: Int) {
                if (state == BluetoothProfile.STATE_CONNECTED && status == BluetoothGatt.GATT_SUCCESS) results.offer(GattResult("connect", status))
                else if (state == BluetoothProfile.STATE_DISCONNECTED || status != BluetoothGatt.GATT_SUCCESS) { closed = true; results.offer(GattResult("disconnected", status)); channelError = "disconnected"; packets.offer(ByteArray(0)) }
            }
            override fun onServicesDiscovered(g: BluetoothGatt, status: Int) { results.offer(GattResult("discover", status)) }
            override fun onCharacteristicRead(g: BluetoothGatt, c: BluetoothGattCharacteristic, value: ByteArray, status: Int) { results.offer(GattResult("read", status, value)) }
            override fun onCharacteristicRead(g: BluetoothGatt, c: BluetoothGattCharacteristic, status: Int) { if (Build.VERSION.SDK_INT < 33) results.offer(GattResult("read", status, c.value ?: ByteArray(0))) }
            override fun onCharacteristicWrite(g: BluetoothGatt, c: BluetoothGattCharacteristic, status: Int) { results.offer(GattResult("write", status)) }
            override fun onDescriptorWrite(g: BluetoothGatt, d: BluetoothGattDescriptor, status: Int) { results.offer(GattResult("subscribe", status)) }
            override fun onCharacteristicChanged(g: BluetoothGatt, c: BluetoothGattCharacteristic, value: ByteArray) { notification(c, value) }
            override fun onCharacteristicChanged(g: BluetoothGatt, c: BluetoothGattCharacteristic) { if (Build.VERSION.SDK_INT < 33) notification(c, c.value ?: ByteArray(0)) }
        }
        private fun notification(c: BluetoothGattCharacteristic, value: ByteArray) {
            val queue = notifications[c.service.uuid.toString() + "/" + c.uuid.toString()] ?: return
            if (!queue.offer(value.copyOf())) close()
        }
        fun connect(seconds: Int) { gatt = device.connectGatt(context, false, callback, BluetoothDevice.TRANSPORT_LE); await("connect", seconds) }
        private fun await(operation: String, seconds: Int = 10): ByteArray {
            val result = results.poll(seconds.coerceIn(1, 35).toLong(), TimeUnit.SECONDS) ?: run { close(); error("Bluetooth $operation timed out") }
            if (closed || result.operation == "disconnected") error("disconnected")
            if (result.operation != operation || result.status != BluetoothGatt.GATT_SUCCESS) { close(); error("Bluetooth $operation failed (${result.status})") }
            return result.bytes
        }
        private fun characteristic(args: JSONObject): BluetoothGattCharacteristic = gatt?.getService(uuid(args.getString("service")))?.getCharacteristic(uuid(args.getString("characteristic"))) ?: error("not_found")
        fun perform(method: String, args: JSONObject): Any? {
            check(!closed) { "disconnected" }
            when (method) {
                "receive" -> {
                    channelError?.let { error(it) }
                    val bytes = packets.poll(args.optInt("timeout", 2).coerceIn(1, 35).toLong(), TimeUnit.SECONDS) ?: error("timeout")
                    if (bytes.isEmpty()) error(channelError ?: "disconnected")
                    return Base64.encodeToString(bytes, Base64.NO_WRAP)
                }
                "send" -> {
                    val stream = socket ?: error("disconnected")
                    val cancel = timeouts.schedule({ close() }, 10, TimeUnit.SECONDS)
                    try { stream.outputStream.write(Base64.decode(args.getString("data"), Base64.DEFAULT)) } finally { cancel.cancel(false) }; return null
                }
                "notification" -> {
                    val key = uuid(args.getString("service")).toString() + "/" + uuid(args.getString("characteristic")).toString()
                    val bytes = notifications[key]?.poll(args.optInt("timeout", 10).coerceIn(1, 35).toLong(), TimeUnit.SECONDS) ?: error("timeout")
                    if (closed) error("disconnected")
                    return Base64.encodeToString(bytes, Base64.NO_WRAP)
                }
            }
            return synchronized(operationLock) {
                val client = gatt ?: error("disconnected")
                when (method) {
                    "discover" -> { check(client.discoverServices()) { "Could not discover Bluetooth services" }; await("discover", args.optInt("timeout", 10)); null }
                    "services" -> JSONArray(client.services.map { it.uuid.toString() })
                    "hasService" -> client.getService(uuid(args.getString("service"))) != null
                    "read" -> { check(client.readCharacteristic(characteristic(args))) { "Could not read Bluetooth characteristic" }; Base64.encodeToString(await("read"), Base64.NO_WRAP) }
                    "write" -> {
                        val c = characteristic(args); val bytes = Base64.decode(args.getString("data"), Base64.DEFAULT)
                        val type = if (args.optBoolean("noResponse")) BluetoothGattCharacteristic.WRITE_TYPE_NO_RESPONSE else BluetoothGattCharacteristic.WRITE_TYPE_DEFAULT
                        if (Build.VERSION.SDK_INT >= 33) check(client.writeCharacteristic(c, bytes, type) == BluetoothStatusCodes.SUCCESS) { "Could not write Bluetooth characteristic" }
                        else { c.writeType = type; c.value = bytes; check(client.writeCharacteristic(c)) { "Could not write Bluetooth characteristic" } }
                        await("write"); null
                    }
                    "subscribe" -> {
                        val c = characteristic(args); check(client.setCharacteristicNotification(c, true)) { "Could not enable notifications" }
                        notifications[c.service.uuid.toString() + "/" + c.uuid.toString()] = ArrayBlockingQueue(64)
                        val descriptor = c.getDescriptor(uuid("2902")) ?: error("not_found")
                        val bytes = if (c.properties and BluetoothGattCharacteristic.PROPERTY_NOTIFY != 0) BluetoothGattDescriptor.ENABLE_NOTIFICATION_VALUE else BluetoothGattDescriptor.ENABLE_INDICATION_VALUE
                        if (Build.VERSION.SDK_INT >= 33) check(client.writeDescriptor(descriptor, bytes) == BluetoothStatusCodes.SUCCESS) { "Could not subscribe" }
                        else { descriptor.value = bytes; check(client.writeDescriptor(descriptor)) { "Could not subscribe" } }
                        await("subscribe"); null
                    }
                    "l2cap" -> {
                        val psm = args.getInt("psm"); require(psm in 0x80..0xff)
                        val channel = device.createInsecureL2capChannel(psm); socket = channel
                        val cancel = timeouts.schedule({ close() }, args.optInt("timeout", 10).coerceIn(1, 35).toLong(), TimeUnit.SECONDS)
                        try { channel.connect() } finally { cancel.cancel(false) }
                        Thread({
                            try {
                                val buffer = ByteArray(16 * 1024)
                                while (!closed) { val n = channel.inputStream.read(buffer); if (n < 0) throw IOException("disconnected"); if (n > 0 && !packets.offer(buffer.copyOf(n), 10, TimeUnit.SECONDS)) throw IOException("Bluetooth receive buffer full") }
                            } catch (e: Exception) { channelError = e.message ?: "disconnected"; packets.offer(ByteArray(0)); close() }
                        }, "WendyL2CAP").apply { isDaemon = true; start() }; null
                    }
                    else -> error("Unknown Android Bluetooth operation")
                }
            }
        }
        fun close() { closed = true; try { socket?.close() } catch (_: Exception) {}; gatt?.disconnect(); gatt?.close(); results.offer(GattResult("disconnected", -1)); packets.offer(ByteArray(0)); notifications.values.forEach { it.offer(ByteArray(0)) } }
    }
    private inner class Browser(private val type: String) : NsdManager.DiscoveryListener {
        val rows = ConcurrentHashMap<String, JSONObject>()
        @Volatile var error: String? = null
        var closed = false
        fun start() { if (!closed) try { nsd.discoverServices(type, NsdManager.PROTOCOL_DNS_SD, this) } catch (e: Exception) { error = e.message } }
        fun close() { closed = true; try { nsd.stopServiceDiscovery(this) } catch (_: Exception) {} }
        override fun onDiscoveryStarted(type: String) {}
        override fun onDiscoveryStopped(type: String) {}
        override fun onStartDiscoveryFailed(type: String, code: Int) { error = "Network discovery failed ($code)" }
        override fun onStopDiscoveryFailed(type: String, code: Int) {}
        override fun onServiceLost(info: NsdServiceInfo) { rows.remove(info.serviceName) }
        override fun onServiceFound(info: NsdServiceInfo) { main.post {
            if (closed) return@post
            resolveQueue.add {
                if (closed) finishResolve() else nsd.resolveService(info, object : NsdManager.ResolveListener {
                    override fun onResolveFailed(service: NsdServiceInfo, code: Int) { main.post { finishResolve() } }
                    override fun onServiceResolved(service: NsdServiceInfo) { main.post {
                        if (!closed) {
                            val txt = JSONObject(); service.attributes.forEach { (key, value) -> txt.put(key, value.toString(Charsets.UTF_8)) }
                            service.host?.hostAddress?.let { address -> rows[service.serviceName] = json("InstanceName" to service.serviceName, "Hostname" to address, "IPAddress" to address, "Port" to service.port, "TXTRecords" to txt) }
                        }
                        finishResolve()
                    } }
                })
            }
            if (!resolving) nextResolve()
        } }
    }
    private fun nextResolve() { if (resolveQueue.isNotEmpty()) { resolving = true; resolveQueue.removeFirst().invoke() } }
    private fun finishResolve() { resolving = false; nextResolve() }
    private fun uuid(raw: String): UUID = UUID.fromString(when (raw.length) { 4 -> "0000$raw-0000-1000-8000-00805f9b34fb"; 8 -> "$raw-0000-1000-8000-00805f9b34fb"; else -> raw })
}
