package dev.wendy.android

import kotlinx.coroutines.*
import org.junit.Assert.*
import org.junit.Test

class ForegroundGraceTest {
    @Test fun briefInterruptionKeepsRuntimeAndOldTimerCannotStopIt() = runBlocking {
        val applied = mutableListOf<Boolean>()
        val waits = mutableListOf<CompletableDeferred<Unit>>()
        val grace = ForegroundGrace(this, { applied.add(it) }) {
            assertEquals(5000L, it)
            val gate = CompletableDeferred<Unit>(); waits.add(gate)
            // Even a completion racing cancellation must not suspend the new generation.
            withContext(NonCancellable) { gate.await() }
        }
        grace.update(true)
        grace.update(false, 5000); yield()
        grace.update(true)
        waits[0].complete(Unit); yield()
        assertFalse(applied.contains(false))
        grace.update(false, 5000); yield()
        waits[1].complete(Unit); yield()
        assertEquals(listOf(true, true, false), applied)
    }
    @Test fun defaultPolicyRetiresImmediately() = runBlocking {
        val applied = mutableListOf<Boolean>()
        val grace = ForegroundGrace(this, { applied.add(it) }) { error("No grace requested") }
        grace.update(true); grace.update(false)
        assertEquals(listOf(true, false), applied)
    }
}
