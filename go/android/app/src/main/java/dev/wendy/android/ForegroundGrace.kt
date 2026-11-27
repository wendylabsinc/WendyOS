package dev.wendy.android

import kotlinx.coroutines.*

/** Short external activity visits keep the current runtime; sustained backgrounding retires it. */
class ForegroundGrace(private val scope: CoroutineScope, private val apply: (Boolean) -> Unit,
    private val wait: suspend (Long) -> Unit = { delay(it) }) {
    private var pending: Job? = null
    private var generation = 0L
    fun update(active: Boolean, graceMillis: Long = 0) {
        val current = ++generation
        pending?.cancel()
        pending = null
        if (active || graceMillis <= 0) apply(active)
        else pending = scope.launch {
            wait(graceMillis)
            if (generation == current) apply(false)
        }
    }
}
