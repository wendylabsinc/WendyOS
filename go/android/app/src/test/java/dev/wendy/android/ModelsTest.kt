package dev.wendy.android
import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test
class ModelsTest {
    @Test fun deviceIdentityUsesSelectorRatherThanDisplayName() {
        val value = JSONObject("""{"id":"one","name":"Robot","selector":"cloud://cloud/tenant/a/asset/b","transport":"Cloud","lite":true}""")
        val device = Device.parse(value)
        assertEquals("cloud://cloud/tenant/a/asset/b", device.selector)
        assertTrue(device.lite)
        assertEquals("Robot", device.name)
    }
    @Test fun absentProtoDefaultsDoNotBecomeStrings() {
        val value = JSONObject("""{"name":null,"name2":"camera","list":null}""")
        assertEquals("camera", value.text("name", "name2"))
        assertEquals(0, value.number("missing"))
        assertFalse(value.flag("missing"))
        assertTrue(value.list("list").isEmpty())
    }
}
