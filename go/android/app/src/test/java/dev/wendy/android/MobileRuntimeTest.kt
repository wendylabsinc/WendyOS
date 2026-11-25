package dev.wendy.android

import kotlinx.coroutines.*
import org.junit.Assert.*
import org.junit.Test

class MobileRuntimeTest {
    @Test fun destroyedOwnerCannotDeactivateItsReplacement() = runBlocking {
        val changes = mutableListOf<Boolean>()
        val runtime = MobileRuntime(this) { active, _, _ -> changes.add(active) }
        val old = runtime.claim()
        assertTrue(runtime.update(old, true, "account", "org"))
        val replacement = runtime.claim()
        val retirement = runtime.release(old, "account", "org")
        assertTrue(runtime.update(replacement, true, "account", "org"))
        retirement.join()
        assertEquals(listOf(true, true), changes)
        runtime.release(replacement, "account", "org").join()
        assertEquals(listOf(true, true, false), changes)
    }
    @Test fun newActivationWaitsForAlreadyStartedCleanup(): Unit = runBlocking {
        val started = CompletableDeferred<Unit>()
        val finish = CompletableDeferred<Unit>()
        val changes = mutableListOf<Boolean>()
        val runtime = MobileRuntime(this) { active, _, _ ->
            if (!active) { started.complete(Unit); finish.await() }
            changes.add(active)
        }
        val old = runtime.claim()
        val retirement = runtime.release(old, "a", "o")
        started.await()
        val replacement = runtime.claim()
        val activation = async { runtime.update(replacement, true, "a", "o") }
        yield()
        assertTrue(changes.isEmpty())
        finish.complete(Unit)
        retirement.join()
        assertTrue(activation.await())
        assertEquals(listOf(false, true), changes)
    }
}
