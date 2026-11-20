package dev.wendy.android

import androidx.compose.foundation.BorderStroke
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp

/**
 * Accounts and organization (iOS MobileAccountsView): organization management and
 * connection settings, saved accounts, the organization picker and sign-in.
 */
@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun AccountSheet(model: WendyModel, dismiss: () -> Unit, open: (String, @Composable () -> Unit) -> Unit) {
    val signedIn = model.accounts.isNotEmpty()
    var addMenu by remember { mutableStateOf(false) }
    var loading by remember { mutableStateOf(false) }
    var error by remember { mutableStateOf<String?>(null) }
    LaunchedEffect(Unit) {
        loading = true
        try { model.refreshAccounts(); error = null } catch (e: kotlinx.coroutines.CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { loading = false }
    }
    ModalBottomSheet(onDismissRequest = dismiss, containerColor = MaterialTheme.colorScheme.background) {
        Column(Modifier.verticalScroll(rememberScrollState()).padding(start = 24.dp, end = 24.dp, bottom = 32.dp), verticalArrangement = Arrangement.spacedBy(16.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                PageTitle(if (signedIn) "Accounts" else "Your world, connected", if (signedIn) "Accounts and organizations" else "Sign in to reach your devices from anywhere.")
                Spacer(Modifier.weight(1f))
                Box {
                    IconButton(onClick = { addMenu = true }, enabled = !model.loginPending) { Icon(Icons.Outlined.Add, "Add account") }
                    DropdownMenu(addMenu, onDismissRequest = { addMenu = false }) {
                        DropdownMenuItem(text = { Text("Wendy Cloud") }, onClick = { addMenu = false; dismiss(); model.signIn() })
                        DropdownMenuItem(text = { Text("Development Cloud") }, onClick = { addMenu = false; dismiss(); model.signIn(true) })
                    }
                }
            }
            FormSection {
                NavRow("Manage organization", Icons.Outlined.Groups, enabled = signedIn) { open("Organization") { OrganizationScreen(model) } }
                NavRow("Connection settings", Icons.Outlined.Lan) { open("Private network") { val nav = LocalNavigator.current; VpnScreen(model) { nav.pop() } } }
            }
            if (!signedIn) SecondaryText("Add an account to access your fleet and organization.")
            else {
                Eyebrow("Accounts")
                model.accounts.forEach { value ->
                    val selected = value.text("Context") == model.account
                    Surface(onClick = { if (!selected) model.chooseAccount(value.text("Context")) }, shape = RoundedCornerShape(20.dp), color = MaterialTheme.colorScheme.surface,
                        border = BorderStroke(1.dp, if (selected) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.outlineVariant)) {
                        Row(Modifier.fillMaxWidth().padding(18.dp), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(12.dp)) {
                            IconTile(Icons.Outlined.PersonOutline)
                            Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(4.dp)) {
                                Text(value.text("Email").ifEmpty { "Email unavailable" }, style = MaterialTheme.typography.titleSmall)
                                Text(value.text("Cloud").removePrefix("https://"), style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
                            }
                            if (selected) Icon(Icons.Outlined.CheckCircle, "Selected account", tint = MaterialTheme.colorScheme.primary, modifier = Modifier.size(20.dp))
                        }
                    }
                }
                if (model.organizations.isNotEmpty()) {
                    ChoiceField("Organization", model.organizations.map { it.text("id") to it.text("name") }, model.organization, enabled = !loading) { id ->
                        if (id != model.organization) model.chooseAccount(model.account, id)
                    }
                }
            }
            if (loading) LoadingRow("Loading accounts…")
            error?.let { FailureNote(it) }
            if (!signedIn) Button(onClick = { dismiss(); model.signIn() }, enabled = !model.loginPending, modifier = Modifier.fillMaxWidth().heightIn(min = 52.dp)) { Text("Sign in to Wendy Cloud") }
            if (model.loginPending) OutlinedButton(onClick = model::cancelLogin, modifier = Modifier.fillMaxWidth()) { Text("Cancel sign-in") }
        }
    }
}
