package dev.wendy.android

import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test

class DeviceDataTest {
    @Test fun displayNameDropsWendyPrefixesOnly() {
        assertEquals("Orin Nano 2", DeviceDisplayName.title("wendyos-orin-nano-2"))
        assertEquals("Thermal ESP32", DeviceDisplayName.title("wendy-lite-thermal-esp32"))
        assertEquals("Wendy device", DeviceDisplayName.title("wendyos-"))
        assertEquals("kitchen-pi", DeviceDisplayName.title("kitchen-pi"))
    }

    @Test fun connectionRecoveryRetriesOnlyTransientFailures() {
        assertTrue(ConnectionRecovery.canRetry("rpc error: code = Unavailable desc = connection refused"))
        assertTrue(ConnectionRecovery.canRetry("context deadline exceeded"))
        assertFalse(ConnectionRecovery.canRetry("certificate signed by unknown authority: connection refused"))
        assertFalse(ConnectionRecovery.canRetry("permission denied"))
        assertFalse(ConnectionRecovery.canRetry("invalid argument"))
    }

    @Test fun presentedFailureRedactsCredentials() {
        val raw = "token=abc.def failed: Bearer eyJhbGciOi.eyJzdWIi.c2ln -----BEGIN PRIVATE KEY-----\nsecret\n-----END PRIVATE KEY----- pin mismatch"
        val failure = PresentedFailure(raw)
        assertEquals("Connection blocked", failure.title)
        assertTrue(failure.blocked)
        assertFalse(failure.details.contains("secret"))
        assertFalse(failure.details.contains("eyJhbGciOi"))
        assertFalse(failure.details.contains("abc.def"))
        assertTrue(failure.details.contains("[redacted]"))
    }

    @Test fun networkSettingsMergesScanKnownAndConnectedState() {
        val value = JSONObject("""{
            "WiFi":{"connected":true,"ssid":"Office"},
            "Networks":[{"ssid":"Office","security":4,"signal_strength":40},{"ssid":"Office","security":4,"signal_strength":80,"is_connected":true},{"ssid":"Lab","security":5,"signal_strength":60},{"ssid":"","security":1}],
            "Known":[{"ssid":"Lab","security":5,"priority":2},{"ssid":"Home","security":4}],
            "Mesh":{"configured":{"participate":true,"ble":true,"infrastructure_wifi":true},"runtime_available":true,"state":"running","authenticated_peers":3,"gateway_principal":"p/gw"},
            "Topology":{"self_principal":"p/self","peers":[{"principal":"p/a","transport":"ble","link_id":7}],"announcements":[{"principal":"p/a","name":"Arm"}]}
        }""")
        val snapshot = NetworkSnapshot.parse(value)
        assertTrue(snapshot.wifiAvailable)
        val visible = snapshot.visibleNetworks()
        assertEquals(listOf("Office", "Lab"), visible.map { it.ssid })
        assertEquals(80, visible[0].signal)
        assertTrue(visible[0].connected)
        assertTrue(visible[1].known)
        assertEquals(listOf("Home"), snapshot.savedOnly().map { it.ssid })
        // participate=bit0, ble=bit4, infrastructure_wifi=bit6
        assertEquals(1 or (1 shl 4) or (1 shl 6), snapshot.meshConfig)
        assertEquals(3L, snapshot.peers)
        assertEquals("p/gw", snapshot.gateway)
        assertEquals("Arm", snapshot.topology.links.single().name)
    }

    @Test fun connectedNetworkMissingFromScanIsShownFirst() {
        val snapshot = NetworkSnapshot.parse(JSONObject("""{"WiFi":{"connected":true,"ssid":"Hidden"},"Networks":[{"ssid":"Cafe","security":1}],"Known":[{"ssid":"Hidden","security":4}],"WiFiError":""}"""))
        val visible = snapshot.visibleNetworks()
        assertEquals("Hidden", visible.first().ssid)
        assertTrue(visible.first().connected && visible.first().known)
        assertNull(visible.first().signal)
    }

    @Test fun meshWithoutConfigurationIsUnavailable() {
        val snapshot = NetworkSnapshot.parse(JSONObject("""{"MeshError":"unimplemented"}"""))
        assertFalse(snapshot.meshAvailable)
        assertFalse(snapshot.wifiAvailable)
        assertFalse(snapshot.topology.available)
        assertEquals("unimplemented", snapshot.meshError)
    }

    @Test fun meshGraphRoutesIndirectPeersThroughDirectLinks() {
        val topology = MeshTopology(true, true, false, 0, "", "self", "",
            links = listOf(MeshLink(1, "a", "", "fd00::a", "wifi", "Alpha", 1)),
            routes = listOf(
                MeshRoute(0, "fd00::b/128", "b", "a", 2, false, false, true),
                MeshRoute(1, "fd00::c/128", "c", "x", 2, false, false, true),
                MeshRoute(2, "fd00::d/128", "d", "a", 65535, false, false, true),
            ),
            announcements = listOf(MeshAnnouncement("b", "dev-b", "Bravo", "")))
        val graph = MeshGraph(topology)
        assertEquals(listOf("a"), graph.direct.map { it.id })
        assertEquals("Via Alpha", graph.nodes.first { it.id == "b" }.subtitle)
        assertEquals("Bravo", graph.nodes.first { it.id == "b" }.title)
        assertEquals("Next hop unavailable", graph.nodes.first { it.id == "c" }.subtitle)
        assertEquals("Unreachable", graph.nodes.first { it.id == "d" }.subtitle)
        assertEquals(setOf("c", "d"), graph.unrouted.map { it.id }.toSet())
    }

    @Test fun enrollmentNamesFollowCloudRules() {
        assertEquals("orin-nano-2", EnrollmentForm.suggestedName("Orin Nano 2"))
        assertEquals("device-3d-printer", EnrollmentForm.suggestedName("3D Printer"))
        assertEquals("cafe-rover", EnrollmentForm.suggestedName("Café Rover"))
        assertEquals("wendy-device", EnrollmentForm.suggestedName("!!!"))
        assertTrue(EnrollmentForm.validName("a"))
        assertTrue(EnrollmentForm.validName("lab-pi-4"))
        assertFalse(EnrollmentForm.validName("lab-"))
        assertFalse(EnrollmentForm.validName("4lab"))
        assertFalse(EnrollmentForm.validName("Lab"))
        assertFalse(EnrollmentForm.validName("a".repeat(64)))
    }

    @Test fun enrollmentRetriesOnlyAfterPreMintRefusal() {
        val prefix = "Cloud refused this enrollment before minting anything, so no credential was spent: rpc error: code = "
        assertTrue(EnrollmentForm.canRetry(prefix + "AlreadyExists desc = name taken"))
        assertTrue(EnrollmentForm.canRetry(prefix + "InvalidArgument desc = bad"))
        assertFalse(EnrollmentForm.canRetry(prefix + "Internal desc = boom"))
        assertFalse(EnrollmentForm.canRetry("context deadline exceeded"))
        assertTrue(EnrollmentForm.needsWiFiRecovery("device PKI enrollment failed: dial tcp: i/o timeout"))
        assertFalse(EnrollmentForm.needsWiFiRecovery("dial tcp: i/o timeout"))
    }

    @Test fun updateSnapshotReportsBusyPhases() {
        val update = UpdateSnapshot.parse(JSONObject("""{"installed_version":"1.0","latest_version":"1.1","phase":"downloading","update_available":true,"downloaded_bytes":5,"total_bytes":10}"""))
        assertTrue(update.busy)
        assertTrue(update.available)
        assertEquals(10L, update.total)
        assertFalse(UpdateSnapshot.parse(JSONObject("""{"phase":"current"}""")).busy)
    }

    @Test fun logEntryStripsTerminalEscapesAndNamesItsSource() {
        val entry = LogEntry.parse(1, JSONObject("""{"Timestamp":0,"Level":5,"Body":"\u001b[31mfailed\u001b[0m\n","App":"camera","Service":"web"}"""))
        assertEquals(LogLevel.Error, entry.level)
        assertEquals("failed", entry.message)
        assertEquals("camera / web", entry.source)
        assertEquals("—", entry.time)
        assertEquals(LogLevel.Unknown, LogEntry.parse(2, JSONObject("""{"Level":99}""")).level)
    }

    @Test fun resourceSnapshotReadsHostDiskGpusAndRows() {
        val value = JSONObject("""{
            "Host":{"cpu_count":8,"mem_total_bytes":1000,"mem_available_bytes":250,"gpus":[{"index":0,"name":"Orin","util_percent":12.5,"mem_total_bytes":0,"temp_c":51.0}],
                    "thermal_zones":[{"name":"cpu","temp_c":48.0}],"battery":{"percent":80,"state":1}},
            "HasCPU":true,"CPUPercent":42.0,
            "Storage":{"mountpoint":"/","used_bytes":30,"total_bytes":100},
            "Rows":[{"ID":"a","App":"a","Name":"a","State":1,"HasCPU":true,"CPUPercent":2.5,"MemoryBytes":64},{"ID":"a/web","App":"a","Name":"web","State":1,"IsService":true}]
        }""")
        val snapshot = ResourceSnapshot(value)
        assertEquals(42.0, snapshot.cpu!!, 0.0)
        assertEquals(750L, snapshot.memoryUsed)
        assertEquals("/", snapshot.diskMount)
        assertEquals(51.0, snapshot.gpus.single().temperature!!, 0.0)
        assertNull(snapshot.gpus.single().power)
        assertEquals("Charging", snapshot.battery!!.state)
        assertTrue(snapshot.hasTemperatureDetail)
        assertNull(snapshot.rows[1].cpu)
        assertTrue(snapshot.rows[1].isService)
        assertNull(ResourceSnapshot(JSONObject("""{"HasCPU":true,"CPUPercent":5}""")).cpu)
    }

    @Test fun deviceAppsReadServicesAndWebpages() {
        val app = DeviceApp.parse(JSONObject("""{"app_name":"viewer","running_state":2,"http_port":8080,"services":[{"name":"web","running_state":1}]}"""))
        assertEquals(AppState.CrashLooping, app.state)
        assertTrue(app.hasWebpage)
        assertEquals(listOf("web" to AppState.Running), app.services)
    }

    @Test fun trafficRowsIncludeTheAgentLast() {
        val traffic = TrafficSnapshot(JSONObject("""{"interfaces":[{"name":"eth0","receivedBytes":10,"sentBytes":5,"receiveBytesPerSecond":1.5}],"agent":{"name":"wendy-agent","receivedBytes":1,"sentBytes":2}}"""))
        assertEquals(listOf(false, true), traffic.rows.map { it.agent })
        assertEquals(1.5, traffic.rows[0].receiveRate!!, 0.0)
        assertNull(traffic.rows[0].sendRate)
        assertEquals("1.5 KiB", binarySize(1536.0))
        assertEquals("12 B", binarySize(12.0))
    }
}
