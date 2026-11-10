package dev.wendy.android

import androidx.compose.foundation.layout.RowScope
import androidx.compose.runtime.Composable
import androidx.compose.runtime.mutableStateListOf
import androidx.compose.runtime.staticCompositionLocalOf
import java.util.concurrent.atomic.AtomicLong

/**
 * A pushed page inside the device or account flows. It mirrors the iOS
 * NavigationStack: the top bar shows [title] and a back arrow, system back pops
 * the page, and [actions] supplies page-specific toolbar buttons.
 */
class NavPage(val title: String, val actions: (@Composable RowScope.() -> Unit)? = null, val content: @Composable () -> Unit) {
    val key: Long = keys.incrementAndGet()
    private companion object { val keys = AtomicLong() }
}

class Navigator {
    val stack = mutableStateListOf<NavPage>()
    val top: NavPage? get() = stack.lastOrNull()
    fun push(title: String, actions: (@Composable RowScope.() -> Unit)? = null, content: @Composable () -> Unit) {
        stack.add(NavPage(title, actions, content))
    }
    fun pop() { if (stack.isNotEmpty()) stack.removeAt(stack.lastIndex) }
    /** Removes [page] and every page above it, for pages whose subject disappeared. */
    fun popTo(page: NavPage) { val index = stack.indexOf(page); if (index >= 0) while (stack.size > index) stack.removeAt(stack.lastIndex) }
    fun clear() { stack.clear() }
}

val LocalNavigator = staticCompositionLocalOf<Navigator> { error("Navigator is not available") }
