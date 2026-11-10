package dev.wendy.android

import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.outlined.ArrowBack
import androidx.compose.material.icons.outlined.*
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.semantics.Role
import androidx.compose.ui.semantics.selected
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp

/**
 * A tall bottom sheet with its own page stack, as iOS presents a NavigationStack in a sheet.
 * [title] names the first page; pushed pages show a back arrow.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun NavigationSheet(title: String, dismiss: () -> Unit, actions: @Composable RowScope.() -> Unit = {}, content: @Composable () -> Unit) {
    val nav = remember { Navigator() }
    ModalBottomSheet(onDismissRequest = dismiss, sheetState = rememberModalBottomSheetState(skipPartiallyExpanded = true), containerColor = MaterialTheme.colorScheme.background) {
        CompositionLocalProvider(LocalNavigator provides nav) {
            Column(Modifier.fillMaxWidth().fillMaxHeight(0.94f)) {
                Row(Modifier.fillMaxWidth().padding(horizontal = 12.dp).heightIn(min = 48.dp), verticalAlignment = Alignment.CenterVertically) {
                    val top = nav.top
                    if (top != null) IconButton(onClick = nav::pop) { Icon(Icons.AutoMirrored.Outlined.ArrowBack, "Back") }
                    Text(top?.title ?: title, Modifier.weight(1f).padding(horizontal = 8.dp), style = MaterialTheme.typography.titleLarge, fontWeight = FontWeight.Bold, maxLines = 1, overflow = TextOverflow.Ellipsis)
                    if (top == null) actions() else top.actions?.invoke(this)
                    TextButton(onClick = dismiss) { Text("Done") }
                }
                androidx.activity.compose.BackHandler(nav.top != null) { nav.pop() }
                Box(Modifier.weight(1f)) { NavigationHost(nav, content) }
            }
        }
    }
}

/**
 * Settings (iOS MobileAccountsView): organization, primary lens, organization management,
 * connection settings, appearance and accounts.
 */
@Composable
fun SettingsSheet(model: WendyModel, dismiss: () -> Unit) {
    var addMenu by remember { mutableStateOf(false) }
    fun signIn(development: Boolean) { dismiss(); model.signIn(development) }
    NavigationSheet("Settings", dismiss, actions = {
        Box {
            IconButton(onClick = { addMenu = true }, enabled = !model.loginPending) { Icon(Icons.Outlined.Add, "Add account") }
            DropdownMenu(addMenu, onDismissRequest = { addMenu = false }) {
                DropdownMenuItem(text = { Text("Wendy Cloud") }, onClick = { addMenu = false; signIn(false) })
                DropdownMenuItem(text = { Text("Development Cloud") }, onClick = { addMenu = false; signIn(true) })
            }
        }
    }) { SettingsContent(model, ::signIn) }
}

@Composable
private fun SettingsContent(model: WendyModel, signIn: (Boolean) -> Unit) {
    val nav = LocalNavigator.current
    var loading by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    LaunchedEffect(Unit) {
        loading = true
        try { model.refreshAccounts(); error = null } catch (e: kotlinx.coroutines.CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { loading = false }
    }
    val signedIn = model.accounts.isNotEmpty()
    Column(Modifier.fillMaxSize().verticalScroll(rememberScrollState()).padding(horizontal = 16.dp).padding(bottom = 32.dp), verticalArrangement = Arrangement.spacedBy(20.dp)) {
        if (model.organizations.isNotEmpty()) LensSection("Organization") {
            model.organizations.forEachIndexed { index, organization ->
                if (index > 0) HorizontalDivider(Modifier.padding(start = 84.dp), color = MaterialTheme.colorScheme.outlineVariant)
                val id = organization.text("id"); val name = organization.text("name").ifEmpty { id }
                val current = id == model.organization
                Row(Modifier.fillMaxWidth().clickable(enabled = !loading, role = Role.RadioButton) { if (!current) model.chooseAccount(model.account, id) }
                    .semantics { selected = current }.padding(horizontal = 20.dp, vertical = 14.dp), horizontalArrangement = Arrangement.spacedBy(16.dp), verticalAlignment = Alignment.CenterVertically) {
                    Box(Modifier.size(48.dp).background(Color(0xFF171C23), CircleShape), contentAlignment = Alignment.Center) {
                        Text(name.take(1).uppercase(), color = Color.White, style = MaterialTheme.typography.titleMedium)
                    }
                    Text(name, Modifier.weight(1f), style = MaterialTheme.typography.titleMedium, maxLines = 2, overflow = TextOverflow.Ellipsis)
                    if (current) Icon(Icons.Outlined.Check, "Selected", tint = LensStyle.green)
                }
            }
        }
        LensSection(null) {
            var lensMenu by remember { mutableStateOf(false) }
            Box {
                LensRow("Primary lens", "", (model.primaryLens ?: Lens.Devices).icon, (model.primaryLens ?: Lens.Devices).color, trailing = (model.primaryLens ?: Lens.Devices).title, navigates = false) { lensMenu = true }
                DropdownMenu(lensMenu, onDismissRequest = { lensMenu = false }) {
                    Lens.entries.forEach { lens -> DropdownMenuItem(text = { Text(lens.title) }, onClick = { lensMenu = false; model.choosePrimaryLens(lens) },
                        trailingIcon = if (lens == model.primaryLens) ({ Icon(Icons.Outlined.Check, null) }) else null) }
                }
            }
            LensDivider()
            LensRow("Manage organization", "", Icons.Outlined.Groups, enabled = signedIn) { nav.push("Organization") { OrganizationScreen(model) } }
            LensDivider()
            LensRow("Connection settings", "", Icons.Outlined.Lan) { nav.push("Private network") { VpnScreen(model) { nav.pop() } } }
        }
        Column(verticalArrangement = Arrangement.spacedBy(6.dp)) {
            Text("APPEARANCE", Modifier.padding(start = 16.dp), style = MaterialTheme.typography.labelMedium, fontWeight = FontWeight.SemiBold, color = MaterialTheme.colorScheme.onSurfaceVariant)
            LensSegments(listOf("System", "Light", "Dark"), model.appearance, { it }) { model.chooseAppearance(it) }
            SecondaryText("System follows your phone. Light or Dark keeps Wendy in that appearance.", Modifier.padding(horizontal = 16.dp))
        }
        if (!signedIn) {
            SecondaryText("Add an account to access your fleet and organization.", Modifier.padding(horizontal = 16.dp))
            LensButton("Sign in to Wendy Cloud", primary = true, enabled = !model.loginPending, modifier = Modifier.fillMaxWidth()) { signIn(false) }
        } else LensSection("Accounts") {
            model.accounts.forEachIndexed { index, value ->
                if (index > 0) LensDivider()
                val current = value.text("Context") == model.account
                Row(Modifier.fillMaxWidth().clickable(enabled = !loading) { if (!current) model.chooseAccount(value.text("Context")) }.semantics { selected = current }
                    .padding(horizontal = 16.dp, vertical = 12.dp), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                    Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(3.dp)) {
                        Text(value.text("Email").ifEmpty { "Email unavailable" }, style = MaterialTheme.typography.bodyLarge, maxLines = 1, overflow = TextOverflow.Ellipsis)
                        Text(value.text("Cloud").removePrefix("https://"), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant, maxLines = 1, overflow = TextOverflow.Ellipsis)
                    }
                    if (current) Icon(Icons.Outlined.CheckCircle, "Selected account", tint = LensStyle.online)
                }
            }
        }
        if (model.loginPending) OutlinedButton(onClick = model::cancelLogin, modifier = Modifier.fillMaxWidth()) { Text("Cancel sign-in") }
        if (loading) LensLoading("Loading accounts…")
        error?.let { FailureNote(it) }
    }
}
