package dev.wendy.android

import kotlinx.coroutines.*
import kotlinx.coroutines.sync.Mutex
import kotlinx.coroutines.sync.withLock
import java.util.concurrent.atomic.AtomicLong

/** Serializes Activity-owned runtime changes, including cleanup after its ViewModel is cleared. */
class MobileRuntime(private val scope: CoroutineScope, private val apply: suspend (Boolean, String, String) -> Unit) {
    private val owner = AtomicLong()
    private val changes = Mutex()
    fun claim(): Long = owner.incrementAndGet()
    suspend fun update(token: Long, active: Boolean, account: String, organization: String): Boolean = changes.withLock {
        if (owner.get() != token) return@withLock false
        apply(active, account, organization)
        owner.get() == token
    }
    fun release(token: Long, account: String, organization: String): Job = scope.launch {
        try { update(token, false, account, organization) }
        catch (e: CancellationException) { throw e }
        catch (e: Exception) { android.util.Log.w("WendyRuntime", "Could not retire Activity runtime", e) }
    }
}
