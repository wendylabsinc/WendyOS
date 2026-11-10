package dev.wendy.android

import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.RowScope
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.runtime.*
import androidx.compose.ui.Modifier
import androidx.compose.ui.layout.layout
import java.util.concurrent.atomic.AtomicLong

/**
 * A pushed page inside the device or account flows. It mirrors the iOS
 * NavigationStack: the top bar shows [title] and a back arrow, system back pops
 * the page, and [actions] supplies page-specific toolbar buttons.
 */
class NavPage(title: String, val actions: (@Composable RowScope.() -> Unit)? = null, val scoped: Boolean = false, val content: @Composable () -> Unit) {
    /** Pages may retitle themselves, for example a lens whose title follows its tab. */
    var title by mutableStateOf(title)
    val key: Long = keys.incrementAndGet()
    private companion object { val keys = AtomicLong() }
}

class Navigator {
    val stack = mutableStateListOf<NavPage>()
    val top: NavPage? get() = stack.lastOrNull()
    fun push(title: String, actions: (@Composable RowScope.() -> Unit)? = null, content: @Composable () -> Unit) {
        stack.add(NavPage(title, actions, content = content))
    }
    /** A page that belongs to the current account and organization, such as a device. */
    fun pushScoped(title: String, actions: (@Composable RowScope.() -> Unit)? = null, content: @Composable () -> Unit) {
        stack.add(NavPage(title, actions, scoped = true, content = content))
    }
    /** Removes the first scoped page and everything above it (iOS drops device routes when the organization changes). */
    fun dropScoped() {
        val index = stack.indexOfFirst { it.scoped }
        if (index >= 0) while (stack.size > index) stack.removeAt(stack.lastIndex)
    }
    fun pop() { if (stack.isNotEmpty()) stack.removeAt(stack.lastIndex) }
    fun clear() { stack.clear() }
}

val LocalNavigator = staticCompositionLocalOf<Navigator> { error("Navigator is not available") }

/** False for pages covered by a pushed page. Polling and streams pause while covered, as iOS views do on disappear. */
val LocalPageVisible = compositionLocalOf { true }

/** The page being composed, so it can retitle itself. Null for the root. */
val LocalNavPage = compositionLocalOf<NavPage?> { null }

/**
 * Shows [base] and every pushed page. Covered layers stay composed, so their
 * state survives a back navigation, but they are not placed, drawn or touchable.
 */
@Composable
fun NavigationHost(navigator: Navigator, base: @Composable () -> Unit) {
    val layers: List<NavPage?> = listOf(null) + navigator.stack
    Box(Modifier.fillMaxSize()) {
        layers.forEachIndexed { index, page ->
            val visible = index == layers.lastIndex
            key(page?.key ?: 0L) {
                CompositionLocalProvider(LocalPageVisible provides (visible && LocalPageVisible.current), LocalNavPage provides page) {
                    Box(Modifier.fillMaxSize().then(if (visible) Modifier else Modifier.unplaced())) { if (page == null) base() else page.content() }
                }
            }
        }
    }
}

private fun Modifier.unplaced() = layout { measurable, constraints ->
    val placeable = measurable.measure(constraints)
    layout(placeable.width, placeable.height) {}
}
