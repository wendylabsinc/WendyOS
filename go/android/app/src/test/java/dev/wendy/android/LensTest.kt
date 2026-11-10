package dev.wendy.android

import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test

class LensTest {
    @Test fun dropScopedRemovesDevicePagesAndEverythingAboveThem() {
        val nav = Navigator()
        nav.push("Devices") {}
        nav.pushScoped("pi-5") {}
        nav.push("Logs") {}
        nav.dropScoped()
        assertEquals(listOf("Devices"), nav.stack.map { it.title })
        // Without a scoped page nothing changes.
        nav.dropScoped()
        assertEquals(listOf("Devices"), nav.stack.map { it.title })
    }

    @Test fun lensNamesRoundTrip() {
        Lens.entries.forEach { assertEquals(it, Lens.of(it.name)) }
        assertNull(Lens.of(null))
        assertNull(Lens.of("Unknown"))
    }

    @Test fun recordingEpisodesParseFromTheGoLibrary() {
        val episode = RecordingEpisode.parse(JSONObject("""{"ID":"e1","Title":"","Trigger":"event:person_detected","Kind":"video","UploadState":"uploaded","Campaign":"door","Started":1700000000123456789,"Bytes":2048,"Duration":12.5,"DetailsLoaded":true,"PreviewCounts":[3,4],"PreviewFile":{"path":"cameras/a.jpg","size":10,"sha256":"00"}}"""))
        assertEquals("e1", episode.id)
        // An untitled episode shows its ID, as on iOS.
        assertEquals("e1", episode.title)
        assertEquals("Person", episode.trigger)
        assertEquals(1700000000123L, episode.started)
        assertEquals(12.5, episode.duration!!, 1e-9)
        assertEquals(true, episode.synced)
        assertEquals("door", episode.campaign)
        assertEquals(listOf(3L, 4L), episode.previewCounts)
        assertEquals("cameras/a.jpg", JSONObject(episode.previewFile!!).getString("path"))
        // The list index has no details: no duration, kind or campaign yet.
        val bare = RecordingEpisode.parse(JSONObject("""{"ID":"e2","Kind":"later-kind","Duration":-1}"""))
        assertNull(bare.duration); assertNull(bare.campaign); assertNull(bare.synced); assertNull(bare.started)
        assertEquals("unknown", bare.kind)
        // Details keep the index row's title, start and size.
        val detailed = RecordingEpisode.parse(JSONObject("""{"ID":"e2","Title":"other","Started":5,"Bytes":9,"Kind":"audio","DetailsLoaded":true}"""), bare.copy(title = "Kept"))
        assertEquals("Kept", detailed.title); assertEquals(0L, detailed.startedNanos); assertEquals("audio", detailed.kind)
    }

    @Test fun triggerLabelsNormalizeLikeIOS() {
        assertNull(RecordingEpisode.triggerLabel(""))
        assertEquals("Door opened", RecordingEpisode.triggerLabel("door-open"))
        assertEquals("Loud sound", RecordingEpisode.triggerLabel("event:loud_sound_detected"))
        assertEquals("Forklift near dock", RecordingEpisode.triggerLabel("forklift  near_dock"))
    }

    @Test fun recordingOwnersJoinLanAndCloudRoutes() {
        val lan = Device("spiffe://wendy.sh/tenant/76/device/42", "pi", "pi.local:50051", "LAN")
        val cloud = Device("cloud://api.wendy.dev:443/tenant/76/asset/42", "pi", "cloud://api.wendy.dev:443/tenant/76/asset/42", "Cloud")
        assertEquals("76/42", recordingDeviceID(lan))
        assertEquals(recordingDeviceID(lan), recordingDeviceID(cloud))
        assertEquals("pi.local", recordingDeviceID(Device("pi.local", "pi", "pi.local", "LAN")))
        assertEquals("spiffe://other/tenant/1/device/2", recordingDeviceID(Device("spiffe://other/tenant/1/device/2", "x", "x", "LAN")))
    }

    @Test fun filteredExportsWaitForEveryDetail() {
        val device = Device("d", "pi", "pi.local", "LAN")
        fun episode(id: String, trigger: String?, loaded: Boolean) = RecordingEpisode(id, id, 1, 10, null, trigger, "video", null, emptyList(), loaded, null, null, emptyList())
        val library = RecordingLibrary(device, null, listOf(episode("a", "Person", true), episode("b", null, false)), null)
        assertEquals("Details checked: 1 of 2 episodes", library.labelCoverage)
        assertEquals(emptyList<RecordingEpisode>(), library.exportTargets("Person", null))
        assertEquals(listOf("a"), library.exportTargets("Person", "a").map { it.id })
        assertEquals(2, library.exportTargets(null, null).size)
        assertEquals(listOf("Person" to 1), library.labels)
    }

    @Test fun activityReceiptsRoundTripAndExpire() {
        val now = 40L * 24 * 3600 * 1000
        val old = ArchivedAlert(AlertSummary("a", "Person", "pi", 5), now - 31L * 24 * 3600 * 1000)
        val fresh = ArchivedAlert(AlertSummary("b", "Update", "pi", null), now - 1000)
        val receipts = ActivityReceipts(setOf("a"), mapOf("a" to old, "b" to fresh))
        assertEquals(receipts, ActivityReceipts.parse(receipts.json()))
        val expired = receipts.expire(now)
        assertEquals(setOf("b"), expired.archived.keys)
        assertEquals(setOf("a"), expired.expired)
        assertEquals(ActivityReceipts(), ActivityReceipts.parse("not json"))
        assertNotEquals(ActivityReceipts.key("a", "b"), ActivityReceipts.key("a|b", ""))
    }

    @Test fun alertIDsAndExportNamesAreStable() {
        val device = Device("spiffe://wendy.sh/tenant/7/device/9", "pi", "pi.local", "LAN")
        assertEquals("3:7/9:clip.e1", alertID(device, "clip.e1"))
        assertEquals("Door-cam 2", exportTitle("Door/cam 2"))
        assertEquals("Recording", exportTitle(""))
        assertEquals(100, exportTitle("x".repeat(300)).length)
    }

    @Test fun recordingKindsAndLabelsMatchIOS() {
        assertEquals("ROS bag", recordingKindTitle("rosbag"))
        assertEquals(listOf("mixed", "video", "audio", "rosbag", "images", "dataset", "unknown"), recordingKinds)
        assertEquals("Recording", recordingKindTitle("unknown"))
        assertEquals(labelColor("Person"), labelColor("person"))
        assertNotEquals(labelColor("person"), labelColor("motion"))
        assertEquals(labelColor(null), labelColor("something else"))
    }

    @Test fun widgetsRoundTripAndValidateLikeIOS() {
        val widget = HomeWidget(kind = HomeWidgetKind.Portal, size = HomeWidgetSize.Large, devices = listOf(HomeWidgetDevice("d1", "Pi")), resource = "web", resourceTitle = "Dashboard")
        val parsed = HomeWidget.parse(widget.json())
        assertEquals(widget, parsed)
        assertTrue(widget.valid)
        assertEquals("Dashboard", widget.displayTitle)
        // A portal needs exactly one device and an app.
        assertFalse(widget.copy(resource = "").valid)
        assertFalse(widget.copy(devices = listOf(HomeWidgetDevice("d1", "Pi"), HomeWidgetDevice("d2", "Jetson"))).valid)
        // Favorites take up to six unique devices.
        val favorites = HomeWidget(kind = HomeWidgetKind.Favorites, devices = (1..6).map { HomeWidgetDevice("d$it", "Device $it") })
        assertTrue(favorites.valid)
        assertFalse(favorites.copy(devices = favorites.devices + HomeWidgetDevice("d7", "Seven")).valid)
        assertFalse(favorites.copy(devices = listOf(HomeWidgetDevice("d1", "A"), HomeWidgetDevice("d1", "B"))).valid)
        // Built-in widgets only allow their sizes, and unknown kinds are skipped.
        assertFalse(HomeWidget(kind = HomeWidgetKind.Actions, size = HomeWidgetSize.Large).valid)
        assertNull(HomeWidget.parse(JSONObject("""{"kind":"later.kind"}""")))
        assertEquals(HomeWidgetSize.Small, HomeWidgetSize.of("Compact"))
    }

    @Test fun smallWidgetsFillTheOpenHalfRowLikeIOS() {
        val (s, m, l) = Triple(HomeWidgetSize.Small, HomeWidgetSize.Medium, HomeWidgetSize.Large)
        // A later small widget sits beside the first one, above the medium widget between them.
        assertEquals(listOf(HomeWidgetPlacement(0, 0, 2, 2), HomeWidgetPlacement(0, 2, 4, 2), HomeWidgetPlacement(2, 0, 2, 2)), packWidgets(listOf(s, m, s)))
        assertEquals(listOf(HomeWidgetPlacement(0, 0, 2, 2), HomeWidgetPlacement(0, 2, 4, 4), HomeWidgetPlacement(2, 0, 2, 2), HomeWidgetPlacement(0, 6, 2, 2)), packWidgets(listOf(s, l, s, s)))
        // Widgets are identified by their recording owner, so one device on the LAN and in Cloud matches.
        assertEquals("76/42", widgetDeviceID(Device("spiffe://wendy.sh/tenant/76/device/42", "pi", "pi.local", "LAN")))
        assertEquals("sel", widgetDeviceID(Device("", "pi", "sel", "LAN")))
    }

    @Test fun homeLayoutKeysSeparateAccountsAndOrganizations() {
        val key = HomeWidgetStore.storageKey("a", "b")
        assertTrue(key.startsWith("wendy.home.widgets.v1."))
        assertNotEquals(key, HomeWidgetStore.storageKey("ab", ""))
        assertEquals(key, HomeWidgetStore.storageKey("a", "b"))
    }

    @Test fun deviceAppearanceKeysAreLengthPrefixed() {
        val device = Device("id", "pi", "pi.local:50051", "LAN")
        val key = DeviceAppearanceValue.key("acct", "org", device)
        assertEquals(64, key.length)
        assertNotEquals(key, DeviceAppearanceValue.key("acc", "torg", device))
        val value = DeviceAppearanceValue(kind = "emoji", emoji = "🦾", color = "blue")
        assertEquals(value, DeviceAppearanceValue.parse(value.json()))
    }
}
