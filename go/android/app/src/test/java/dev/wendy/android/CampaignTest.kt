package dev.wendy.android

import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test

class CampaignTest {
    @Test fun inferenceCampaignNeedsAPinnedRevisionAndLabels() {
        val draft = CampaignDraft()
        assertFalse("no revision yet", draft.valid)
        val pinned = draft.copy(modelRevision = "a".repeat(40))
        assertTrue(pinned.valid)
        assertFalse(pinned.copy(labels = emptySet()).valid)
        assertFalse(pinned.copy(labels = (1..33).map { "l$it" }.toSet()).valid)
        assertFalse(pinned.copy(modelRevision = "main").valid)
    }

    @Test fun eventCampaignWithoutCameraNeedsAnotherSource() {
        val event = CampaignDraft().withTrigger("event").copy(camera = "")
        assertFalse(event.valid)
        assertTrue(event.copy(telemetry = true).valid)
        assertTrue(event.copy(audio = setOf("mic")).valid)
        assertTrue(CampaignDraft().withTrigger("event").valid)
    }

    @Test fun triggerChangeResetsModelDefaults() {
        val classification = CampaignDraft().copy(modelRevision = "a".repeat(40)).withTrigger("classification")
        assertEquals("google/mobilenet_v2_1.0_224", classification.model)
        assertTrue(classification.labels.isEmpty())
        assertEquals("", classification.modelRevision)
        assertEquals(setOf("person"), classification.withTrigger("detection").labels)
    }

    @Test fun inputMatchesDesktopCampaignInputFields() {
        val input = CampaignDraft(modelRevision = "b".repeat(40), labels = setOf("person", "dog"), audio = setOf("mic"), notify = false).input()
        val fields = input.keys().asSequence().toSet()
        // The Go bridge rejects unknown fields, so these must match commands.DesktopCampaignInput.
        assertEquals(setOf("Name", "Camera", "Event", "RunInference", "Classification", "Model", "ModelRevision", "Labels", "Audio", "ROS2", "Telemetry",
            "Notify", "Upload", "Threshold", "Cooldown", "Before", "After"), fields)
        assertTrue(input.getBoolean("RunInference"))
        assertFalse(input.getBoolean("Classification"))
        assertFalse(input.getBoolean("Notify"))
        assertEquals("[\"dog\",\"person\"]", input.getJSONArray("Labels").toString())
    }

    @Test fun clipsAreNewestFirstWithoutDuplicates() {
        val clips = CampaignClip.parseAll(JSONObject("""{"Items":[
            {"ID":"a","Campaign":"c","Started":1000000000,"Duration":75.4},
            {"ID":"b","Campaign":"c","Started":3000000000,"Duration":-1},
            {"ID":"a","Campaign":"c","Started":1000000000,"Duration":75.4}]}"""))
        assertEquals(listOf("b", "a"), clips.map { it.id })
        assertEquals("—", clips[0].durationLabel)
        assertEquals("1:15", clips[1].durationLabel)
        assertEquals(1000L, clips[1].started)
    }
}
