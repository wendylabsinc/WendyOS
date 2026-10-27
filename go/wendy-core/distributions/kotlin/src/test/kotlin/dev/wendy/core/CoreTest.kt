package dev.wendy.core

import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue

class CoreTest {
    @Test fun reportsTheEngineVersion() {
        val version = WendyCoreVersion()
        assertTrue(version.startsWith("0.1.0"))
        System.getenv("WENDY_CORE_VERSION")?.let { assertEquals(it, version) }
        repeat(100) { assertEquals(version, WendyCoreVersion()) }
    }
    @Test fun reachesTheSharedEngine() {
        repeat(100) { assertEquals(42, WendyHelloWorld()) }
    }
}
