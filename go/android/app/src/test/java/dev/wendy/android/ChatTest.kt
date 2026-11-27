package dev.wendy.android

import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test

class ChatTest {
    @Test fun readableModelNamesMatchIOS() {
        assertEquals("Qwen 3 · 4B", ChatModelOption.readableName("qwen3:4b"))
        assertEquals("Qwen 2.5 Coder · 7B Instruct", ChatModelOption.readableName("Qwen/Qwen2.5-Coder-7B-Instruct-GGUF"))
        assertEquals("Llama 3.2 · 3B Instruct", ChatModelOption.readableName("bartowski/Llama-3.2-3B-Instruct-Q4_K_M.gguf"))
        assertEquals("custom", ChatModelOption.readableName("custom"))
    }

    @Test fun markdownBlocksKeepCodeLiteral() {
        val blocks = MarkdownBlock.parse("# Title\nSome *text*\n\n- one\n  - two\n1. first\n```\n# not a heading\n```\n> quote\n---\n| a | b |\n| --- | --- |\n| 1 | 2 |")
        assertEquals(MarkdownBlock.Heading(1, "Title"), blocks[0])
        assertEquals(MarkdownBlock.Paragraph("Some *text*"), blocks[1])
        assertEquals(MarkdownBlock.Item("•", 0, "one"), blocks[2])
        assertEquals(MarkdownBlock.Item("•", 1, "two"), blocks[3])
        assertEquals(MarkdownBlock.Item("1.", 0, "first"), blocks[4])
        assertEquals(MarkdownBlock.Code("# not a heading"), blocks[5])
        assertEquals(MarkdownBlock.Quote("quote"), blocks[6])
        assertEquals(MarkdownBlock.Rule, blocks[7])
        assertEquals(MarkdownBlock.Table(listOf(listOf("a", "b"), listOf("1", "2"))), blocks[8])
        // An unterminated fence while streaming keeps the rest literal.
        assertEquals(listOf(MarkdownBlock.Code("partial **code**")), MarkdownBlock.parse("```kotlin\npartial **code**"))
    }

    @Test fun inlineMarkdownStripsMarkersAndKeepsUnmatchedOnes() {
        val style = androidx.compose.ui.text.SpanStyle()
        assertEquals("bold and code and link", MarkdownBlock.inline("**bold** and `code` and [link](https://wendy.sh)", style, style).text)
        assertEquals("2 * 3 = 6", MarkdownBlock.inline("2 * 3 = 6", style, style).text)
    }

    @Test fun modelFitUsesMeasuredCapacity() {
        val gib = 1L shl 30
        val jetson = ModelHardware(JSONObject("""{"OS":"linux","Architecture":"arm64","CUDA":true,"JetPack":"6.2","SharedMemory":true,"MemoryTotal":${16 * gib},"MemoryAvailable":${12 * gib},"DiskKnown":true,"DiskFree":${100 * gib}}"""))
        assertTrue(jetson.canInstall)
        assertEquals(3, jetson.accelerator)
        assertEquals(ModelFit.Comfortable, jetson.fit(HubFile("a", "Q4", 2 * gib, ""), true))
        assertEquals(ModelFit.Tight, jetson.fit(HubFile("a", "Q4", 5 * gib, ""), true))
        assertEquals(ModelFit.TooLarge, jetson.fit(HubFile("a", "Q4", 7 * gib, ""), true))
        assertEquals(ModelFit.TooLarge, jetson.fit(HubFile("a", "Q4", 2 * gib, ""), true).let { ModelHardware(JSONObject("""{"DiskKnown":true,"DiskFree":${5 * gib},"MemoryTotal":${16 * gib},"MemoryAvailable":${16 * gib}}""")).fit(HubFile("a", "Q4", 2 * gib, ""), true) })
        val unknown = ModelHardware(JSONObject("{}"))
        assertEquals(0, unknown.accelerator)
        assertFalse(unknown.canInstall)
        assertEquals(ModelFit.Unknown, unknown.fit(HubFile("a", "Q4", gib, ""), false))
    }

    @Test fun approvalsDescribeCampaignArguments() {
        val approval = ChatApproval(1, "create_person_detection_campaign", """{"name":"door","camera":"*","before_seconds":5,"after_seconds":20,"notify":true}""")
        assertTrue(approval.isCampaign)
        assertEquals("door", approval.campaignName)
        assertEquals("All current and future cameras", approval.cameraName)
        assertEquals("5s before · 20s after detection", approval.clipSummary)
        assertEquals("Create campaign", approval.confirmLabel)
        assertEquals("this campaign", ChatApproval(2, "enable_campaign_notifications", "not json").campaignName)
    }

    @Test fun cloudKeysAreValidatedLikeIOS() {
        assertEquals("sk-abc", validCloudKey("  sk-abc \n".trimEnd('\n')))
        assertThrows(IllegalArgumentException::class.java) { validCloudKey("   ") }
        assertThrows(IllegalArgumentException::class.java) { validCloudKey("a\nb") }
        assertThrows(IllegalArgumentException::class.java) { validCloudKey("x".repeat(8193)) }
        assertEquals(listOf("openai", "anthropic"), ChatCloudProvider.entries.map { it.raw })
    }

    @Test fun rememberedAIDevicesKeepOnlyVerifiedMetadata() {
        val record = RememberedAIDevice("76/42", "Pi", listOf(RememberedAIModel("qwen", "Qwen 3", 11434), RememberedAIModel("llama", "Llama 3", 11435)), 10, 20, "llama")
        assertEquals(record, RememberedAIDevice.parse(record.json()))
        assertEquals("llama", record.preferred?.id)
        assertEquals("qwen", record.copy(lastModel = "gone").preferred?.id)
        assertEquals(setOf("id", "name", "checked", "models", "lastUsed", "lastModel"), record.json().keys().asSequence().toSet())
        assertNotEquals(RememberedAIDevices.key("a", "b"), RememberedAIDevices.key("ab", ""))
        assertTrue(RememberedAIDevices.key("a", "b").startsWith("wendy.aiDevices.v1."))
    }
}
