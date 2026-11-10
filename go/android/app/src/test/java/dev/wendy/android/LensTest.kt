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
        val episode = RecordingItem.parse(JSONObject("""{"ID":"e1","Title":"","Trigger":"person","Kind":"video","Campaign":"door","Started":1700000000123456789,"Bytes":2048,"Duration":12.5}"""))
        assertEquals("e1", episode.id)
        assertEquals("Recording", episode.title)
        assertEquals("person", episode.trigger)
        assertEquals(1700000000123L, episode.started)
        assertEquals(12.5, episode.duration, 1e-9)
        // A missing duration is unavailable, as Go reports -1.
        assertEquals(-1.0, RecordingItem.parse(JSONObject("""{"ID":"e2"}""")).duration, 1e-9)
    }

    @Test fun recordingKindsAndLabelsMatchIOS() {
        assertEquals("ROS bag", recordingKindTitle("rosbag"))
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
