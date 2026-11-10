package dev.wendy.android

import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.content.Intent
import androidx.compose.foundation.background
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.text.selection.SelectionContainer
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.*
import androidx.compose.material3.*
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.unit.dp
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.launch
import org.json.JSONObject
import java.text.DateFormat
import java.util.Calendar
import java.util.Date

data class CloudMember(val id: String, val name: String, val email: String, val roleID: String, val profileError: String) {
    val title get() = name.ifEmpty { email.ifEmpty { id } }
}
data class CloudRole(val id: String, val name: String, val description: String, val permissions: List<String>)
data class CloudAccessRule(val id: String, val principalID: String, val principalName: String, val principalType: Int, val targetID: String, val targetName: String, val targetType: Int, val effect: Int) {
    val principal get() = principalName.ifEmpty { principalID }
    val target get() = if (targetType == 4) "All devices" else targetName.ifEmpty { targetID }
    val scope get() = when (targetType) { 1 -> "Device"; 2 -> "Site"; 3 -> "Tag"; 4 -> "Organization"; else -> "Unknown" }
    val effectName get() = when (effect) { 1 -> "Allow"; 2 -> "Deny"; else -> "Unknown" }
}
data class CloudInvitation(val id: String, val role: String, val expiresSeconds: Long, val revoked: Boolean) {
    fun state(now: Long = System.currentTimeMillis() / 1000) = if (revoked) "Revoked" else if (expiresSeconds <= now) "Expired" else "Active"
}

/** An organization overview: one request, with per-section errors (iOS CloudOrganizationSnapshot). */
class CloudOrganizationSnapshot(value: JSONObject) {
    private fun strings(o: JSONObject?, key: String) = o?.optJSONArray(key)?.let { a -> (0 until a.length()).map(a::optString) } ?: emptyList()
    val name = value.child("Organization").text("name")
    val members = value.list("Members").map { CloudMember(it.child("Member").text("user_id"), it.text("Name"), it.text("Email"), it.child("Member").text("role_id"), it.text("ProfileError")) }
    val roles = value.list("Roles").map { CloudRole(it.text("id"), it.text("name"), it.text("description"), strings(it, "permissions")) }
    val devices = value.list("Devices").filter { it.flag("is_compute_device") || !it.has("is_compute_device") }.map { it.text("id") to it.text("display_name", "name").ifEmpty { "Unnamed device" } }
    val rules = value.list("Access").map { CloudAccessRule(it.text("id"), it.text("principal_id"), it.text("principal_display_name"), it.number("principal_type").toInt(), it.text("target_ref"), it.text("target_display_name"), it.number("target_type").toInt(), it.number("effect").toInt()) }
    val invitations = value.list("Invites").map { CloudInvitation(it.text("id"), it.text("role"), it.child("expires_at").number("seconds"), it.flag("revoked")) }
    val permissions = strings(value, "Permissions").toSet()
    val teamError = value.text("TeamError"); val rolesError = value.text("RolesError"); val devicesError = value.text("DevicesError")
    val accessError = value.text("AccessError"); val invitesError = value.text("InvitesError"); val permissionsError = value.text("PermissionsError")
    fun roleName(id: String) = roles.firstOrNull { it.id == id }?.name ?: id.ifEmpty { "No assigned role" }
}

/** Shared state for the organization pages; every change refreshes the overview. */
class CloudManagementState(val model: WendyModel) {
    var snapshot by mutableStateOf<CloudOrganizationSnapshot?>(null); private set
    var loading by mutableStateOf(false); private set
    var saving by mutableStateOf(false); private set
    var error by mutableStateOf<String?>(null); private set
    var actionError by mutableStateOf<String?>(null)
    val account get() = model.account
    val organization get() = model.organization
    fun can(permission: String) = snapshot?.permissions?.contains(permission) == true && !saving && !loading
    suspend fun refresh() {
        val account = model.account; val org = model.organization
        if (account.isEmpty() || org.isEmpty()) { snapshot = null; return }
        loading = true; error = null
        try {
            val value = CloudOrganizationSnapshot(model.backend.call("Organization", account, org).obj())
            if (account == model.account && org == model.organization) snapshot = value
        } catch (e: CancellationException) { throw e } catch (e: Exception) { error = e.message } finally { loading = false }
    }
    /** Runs a change, closes its form on success, then refreshes. A slow refresh must not leave a submitted form open. */
    suspend fun save(work: suspend () -> Unit, onSuccess: () -> Unit = {}): Boolean {
        if (saving) return false
        saving = true; actionError = null
        try { work(); onSuccess() } catch (e: CancellationException) { throw e } catch (e: Exception) { actionError = e.message; return false } finally { saving = false }
        refresh()
        return true
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun OrganizationScreen(model: WendyModel) {
    val nav = LocalNavigator.current
    val scope = rememberCoroutineScope()
    val state = remember { CloudManagementState(model) }
    var refreshing by remember { mutableStateOf(false) }
    var menu by remember { mutableStateOf(false) }
    LaunchedEffect(model.account, model.organization) { state.refresh() }
    PullToRefreshBox(refreshing, onRefresh = { scope.launch { refreshing = true; model.refreshAccounts(); state.refresh(); refreshing = false } }) {
        PageColumn {
            if (model.accounts.isEmpty()) {
                item { EmptyMessage("Connect to Wendy Cloud", "Sign in to manage your organization.", Icons.Outlined.CloudQueue) }
                item { Button(onClick = { model.signIn() }, Modifier.fillMaxWidth(), enabled = !model.loginPending) { Text(if (model.loginPending) "Signing in…" else "Sign in") } }
                if (model.loginPending) item { OutlinedButton(onClick = model::cancelLogin, Modifier.fillMaxWidth()) { Text("Cancel sign-in") } }
                return@PageColumn
            }
            item {
                Row(verticalAlignment = Alignment.CenterVertically) {
                    PageTitle(state.snapshot?.name?.ifEmpty { null } ?: "Organization", model.organizations.firstOrNull { it.text("id") == model.organization }?.text("name") ?: "")
                    Spacer(Modifier.weight(1f))
                    Box {
                        IconButton(onClick = { menu = true }, enabled = state.snapshot != null && !state.saving) { Icon(Icons.Outlined.PersonAdd, "Add teammate") }
                        DropdownMenu(menu, onDismissRequest = { menu = false }) {
                            DropdownMenuItem(text = { Text("Add by email") }, leadingIcon = { Icon(Icons.Outlined.PersonAdd, null) }, enabled = state.can("member:add"),
                                onClick = { menu = false; nav.push("Add teammate") { TeammateForm(state) } })
                            DropdownMenuItem(text = { Text("Create invite link") }, leadingIcon = { Icon(Icons.Outlined.Link, null) }, enabled = state.can("invite:create"),
                                onClick = { menu = false; nav.push("Create invitation") { InvitationForm(state) } })
                            HorizontalDivider()
                            DropdownMenuItem(text = { Text("Manage invitations") }, leadingIcon = { Icon(Icons.Outlined.Mail, null) }, onClick = { menu = false; nav.push("Invitations") { InvitationsPage(state) } })
                        }
                    }
                }
            }
            state.error?.let { item { FormSection { FailureNote(it) } } }
            state.actionError?.let { item { FormSection { FailureNote(it) } } }
            val snapshot = state.snapshot
            if (snapshot == null) {
                item { if (state.loading) LoadingRow("Loading organization…") else if (state.error == null) SecondaryText("No organizations") }
                return@PageColumn
            }
            if (snapshot.permissionsError.isNotEmpty()) item { FormSection { FailureNote(snapshot.permissionsError) } }
            if (snapshot.teamError.isNotEmpty()) item { FormSection { FailureNote(snapshot.teamError) } }
            else formSection("Teammates") {
                snapshot.members.forEach { member ->
                    Row(Modifier.fillMaxWidth().clickable { nav.push(member.title) { MemberPage(state, member.id) } }.padding(vertical = 8.dp), verticalAlignment = Alignment.CenterVertically) {
                        Column(Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(2.dp)) {
                            Text(member.title, style = MaterialTheme.typography.bodyLarge)
                            if (member.email.isNotEmpty() && member.email != member.title) SecondaryText(member.email)
                            SecondaryText(snapshot.roleName(member.roleID))
                        }
                        Icon(Icons.Outlined.ChevronRight, null, tint = MaterialTheme.colorScheme.onSurfaceVariant)
                    }
                }
                if (snapshot.members.isEmpty()) SecondaryText("No teammates")
            }
        }
    }
}

@Composable
private fun MemberPage(state: CloudManagementState, memberID: String) {
    val nav = LocalNavigator.current
    val scope = rememberCoroutineScope()
    var removal by remember { mutableStateOf<Pair<String, suspend () -> Unit>?>(null) }
    val snapshot = state.snapshot
    val member = snapshot?.members?.firstOrNull { it.id == memberID }
    PageColumn {
        if (snapshot == null || member == null) { item { EmptyMessage("Teammate not found", "This person is no longer in the organization.", Icons.Outlined.PersonOff) }; return@PageColumn }
        state.actionError?.let { item { FormSection { FailureNote(it) } } }
        formSection {
            if (member.email.isNotEmpty()) LabeledRow("Email", member.email, selectable = true)
            if (member.profileError.isNotEmpty()) FailureNote(member.profileError)
        }
        formSection("Role") {
            LabeledRow("Assigned role", snapshot.roleName(member.roleID))
            ActionRow("Change role", enabled = state.can("member:set-role") && snapshot.rolesError.isEmpty()) { nav.push("Change role") { RoleForm(state, member) } }
            snapshot.roles.firstOrNull { it.id == member.roleID }?.let { role ->
                NavRow("Role permissions") { nav.push("Role permissions") { PageColumn { formSection { role.permissions.sorted().forEach { Text(it, Modifier.padding(vertical = 6.dp), fontFamily = FontFamily.Monospace, style = MaterialTheme.typography.bodyMedium) } } } } }
            }
            if (snapshot.rolesError.isNotEmpty()) FailureNote(snapshot.rolesError)
        }
        formSection("Direct device access", "Group rules may also apply. A deny overrides an allow. Device access also requires the appropriate role permissions.") {
            if (snapshot.accessError.isNotEmpty()) FailureNote(snapshot.accessError)
            else {
                val rules = snapshot.rules.filter { it.principalType == 1 && it.principalID == member.id }
                if (rules.isEmpty()) SecondaryText("No direct rules")
                rules.forEach { rule ->
                    AccessRuleRow(rule, enabled = state.can("device-access:delete")) {
                        removal = "Remove the ${rule.effectName.lowercase()} rule for ${rule.principal} on ${rule.target}. Other rules still apply." to { state.save({ state.model.backend.call("DeleteAccess", state.account, state.organization, rule.id) }) }
                    }
                }
            }
            ActionRow("Add device access", Icons.Outlined.Add, enabled = state.can("device-access:create")) { nav.push("Add device access") { AccessForm(state, member.id) } }
        }
        formSection {
            ActionRow("Remove teammate", Icons.Outlined.PersonRemove, destructive = true, enabled = state.can("member:remove")) {
                removal = "Remove ${member.title} from this organization and remove their direct device access rules." to {
                    if (state.save({ state.model.backend.call("RemoveMember", state.account, state.organization, member.id) })) nav.pop()
                }
            }
        }
    }
    removal?.let { (message, work) ->
        ConfirmAction(if (message.startsWith("Remove the")) "Remove access rule?" else "Remove teammate?", message, "Confirm", onDismiss = { removal = null }) { removal = null; scope.launch { work() } }
    }
}

@Composable
private fun AccessRuleRow(rule: CloudAccessRule, enabled: Boolean, remove: () -> Unit) {
    Row(Modifier.fillMaxWidth().padding(vertical = 6.dp), verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(10.dp)) {
        Box(Modifier.size(7.dp).background(if (rule.effect == 1) MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.error, CircleShape))
        Column(Modifier.weight(1f)) { Text(rule.target, style = MaterialTheme.typography.bodyLarge); SecondaryText("${rule.effectName} · ${rule.scope}") }
        IconButton(onClick = remove, enabled = enabled) { Icon(Icons.Outlined.RemoveCircleOutline, "Remove rule") }
    }
}

@Composable
private fun FormPage(state: CloudManagementState, footer: String?, submitLabel: String, valid: Boolean, submit: suspend () -> Unit, fields: @Composable ColumnScope.() -> Unit) {
    val scope = rememberCoroutineScope()
    var submitting by remember { mutableStateOf(false) }
    PageColumn {
        state.actionError?.let { formSection { FailureNote(it) } }
        formSection(footer = footer ?: state.snapshot?.name) { fields() }
        item {
            Button(onClick = { if (!submitting) { submitting = true; scope.launch { try { submit() } finally { submitting = false } } } },
                Modifier.fillMaxWidth(), enabled = valid && !submitting && !state.saving) { Text(if (submitting || state.saving) "Saving…" else submitLabel) }
        }
    }
}

@Composable
private fun TeammateForm(state: CloudManagementState) {
    val nav = LocalNavigator.current
    var email by remember { mutableStateOf("") }
    var role by remember { mutableStateOf("") }
    LaunchedEffect(Unit) { state.actionError = null }
    FormPage(state, null, "Save", email.isNotBlank(), { state.save({ state.model.backend.call("AddMember", state.account, state.organization, email.trim(), role) }) { nav.pop() } }) {
        OutlinedTextField(email, { email = it }, Modifier.fillMaxWidth(), label = { Text("Email") }, singleLine = true, keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Email, autoCorrectEnabled = false))
        SecondaryText("Adds someone who already has a Wendy account. For a new teammate, create an invitation link.")
        ChoiceField("Role", listOf("" to "Organization default") + state.snapshot?.roles.orEmpty().map { it.id to it.name }, role) { role = it }
    }
}

@Composable
private fun RoleForm(state: CloudManagementState, member: CloudMember) {
    val nav = LocalNavigator.current
    var role by remember { mutableStateOf(member.roleID) }
    LaunchedEffect(Unit) { state.actionError = null }
    FormPage(state, null, "Save", role.isNotEmpty(), { state.save({ state.model.backend.call("SetMemberRole", state.account, state.organization, member.id, role) }) { nav.pop() } }) {
        (listOf(CloudRole("", "Organization default", "", emptyList())) + state.snapshot?.roles.orEmpty()).forEach { option ->
            Row(Modifier.fillMaxWidth().clickable { role = option.id }.padding(vertical = 8.dp), verticalAlignment = Alignment.CenterVertically) {
                Column(Modifier.weight(1f)) { Text(option.name, style = MaterialTheme.typography.bodyLarge); SecondaryText(option.description) }
                RadioButton(role == option.id, { role = option.id })
            }
        }
        state.snapshot?.rolesError?.takeIf { it.isNotEmpty() }?.let { FailureNote(it) }
    }
}

@Composable
private fun AccessForm(state: CloudManagementState, initialUser: String) {
    val nav = LocalNavigator.current
    var user by remember { mutableStateOf(initialUser) }
    var device by remember { mutableStateOf("") }
    var deny by remember { mutableStateOf(false) }
    LaunchedEffect(Unit) { state.actionError = null }
    FormPage(state, null, "Save", user.isNotEmpty(), { state.save({ state.model.backend.call("CreateAccess", state.account, state.organization, user, device, if (deny) 2 else 1) }) { nav.pop() } }) {
        ChoiceField("Teammate", state.snapshot?.members.orEmpty().map { it.id to it.title }, user) { user = it }
        ChoiceField("Devices", listOf("" to "All devices") + state.snapshot?.devices.orEmpty(), device) { device = it }
        ChoiceTabs(listOf("Allow", "Deny"), if (deny) "Deny" else "Allow", { deny = it == "Deny" })
        SecondaryText(if (deny) "Deny blocks access even if another rule allows it." else "An allow still requires role permissions and cannot override a deny.")
        state.snapshot?.devicesError?.takeIf { it.isNotEmpty() }?.let { FailureNote(it) }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun InvitationForm(state: CloudManagementState) {
    val context = LocalContext.current
    var role by remember { mutableStateOf("") }
    var expires by remember { mutableLongStateOf(System.currentTimeMillis() + 7 * 86_400_000L) }
    var link by remember { mutableStateOf("") }
    var picking by remember { mutableStateOf(false) }
    LaunchedEffect(Unit) { state.actionError = null }
    if (link.isNotEmpty()) {
        PageColumn {
            formSection(footer = "Copy or share this link before leaving. Anyone with the link can join with the selected role until it expires.") {
                SelectionContainer { Text(link, Modifier.padding(vertical = 8.dp), fontFamily = FontFamily.Monospace, style = MaterialTheme.typography.bodyMedium) }
                ActionRow("Copy link", Icons.Outlined.ContentCopy) { (context.getSystemService(Context.CLIPBOARD_SERVICE) as ClipboardManager).setPrimaryClip(ClipData.newPlainText("Wendy invitation", link)) }
                ActionRow("Share link", Icons.Outlined.Share) {
                    context.startActivity(Intent.createChooser(Intent(Intent.ACTION_SEND).setType("text/plain").putExtra(Intent.EXTRA_TEXT, link), "Share invitation"))
                }
            }
        }
        return
    }
    FormPage(state, null, "Create link", expires > System.currentTimeMillis(), {
        state.save({ link = state.model.backend.call("CreateInvite", state.account, state.organization, role, expires / 1000) as? String ?: "" })
    }) {
        ChoiceField("Role", listOf("" to "Organization default", "viewer" to "Viewer", "member" to "Member", "admin" to "Admin"), role) { role = it }
        NavRow("Expires", value = DateFormat.getDateTimeInstance(DateFormat.MEDIUM, DateFormat.SHORT).format(Date(expires))) { picking = true }
    }
    if (picking) {
        val picker = rememberDatePickerState(initialSelectedDateMillis = expires, selectableDates = object : SelectableDates {
            override fun isSelectableDate(utcTimeMillis: Long) = utcTimeMillis >= System.currentTimeMillis() - 86_400_000L
        })
        DatePickerDialog(onDismissRequest = { picking = false }, confirmButton = {
            TextButton(onClick = {
                picker.selectedDateMillis?.let { day ->
                    // Keep the current time of day on the chosen date.
                    val now = Calendar.getInstance()
                    val chosen = Calendar.getInstance().apply { timeInMillis = day; set(Calendar.HOUR_OF_DAY, now.get(Calendar.HOUR_OF_DAY)); set(Calendar.MINUTE, now.get(Calendar.MINUTE)) }
                    expires = chosen.timeInMillis
                }
                picking = false
            }) { Text("OK") }
        }, dismissButton = { TextButton(onClick = { picking = false }) { Text("Cancel") } }) { DatePicker(picker) }
    }
}

@Composable
private fun InvitationsPage(state: CloudManagementState) {
    val nav = LocalNavigator.current
    val scope = rememberCoroutineScope()
    var revoking by remember { mutableStateOf<CloudInvitation?>(null) }
    val snapshot = state.snapshot
    PageColumn {
        state.actionError?.let { formSection { FailureNote(it) } }
        item { SecondaryText("Create a link to share with a teammate. Links are shown only once.") }
        item { OutlinedButton(onClick = { nav.push("Create invitation") { InvitationForm(state) } }, enabled = state.can("invite:create")) { Text("Create invitation") } }
        when {
            snapshot == null -> item { LoadingRow("Loading invitations…") }
            snapshot.invitesError.isNotEmpty() -> item { FailureView("Could not load invitations", snapshot.invitesError) { scope.launch { state.refresh() } } }
            snapshot.invitations.isEmpty() -> item { EmptyMessage("No invitations", "Invitation links you create appear here.", Icons.Outlined.Mail) }
            else -> formSection {
                snapshot.invitations.forEach { invitation ->
                    val status = invitation.state()
                    Row(Modifier.fillMaxWidth().padding(vertical = 8.dp), verticalAlignment = Alignment.CenterVertically) {
                        Column(Modifier.weight(1f)) {
                            Text(invitation.role.ifEmpty { "Organization default" }.replaceFirstChar { it.uppercase() }, style = MaterialTheme.typography.bodyLarge)
                            Text("$status · Expires ${DateFormat.getDateInstance(DateFormat.MEDIUM).format(Date(invitation.expiresSeconds * 1000))}", style = MaterialTheme.typography.bodySmall,
                                color = if (status == "Active") MaterialTheme.colorScheme.primary else MaterialTheme.colorScheme.onSurfaceVariant)
                        }
                        TextButton(onClick = { revoking = invitation }, enabled = state.can("invite:revoke") && status == "Active") { Text("Revoke", color = MaterialTheme.colorScheme.error) }
                    }
                }
            }
        }
    }
    revoking?.let { invitation ->
        ConfirmAction("Revoke invitation?", "This invitation link will no longer let people join the organization.", "Revoke", onDismiss = { revoking = null }) {
            revoking = null; scope.launch { state.save({ state.model.backend.call("RevokeInvite", state.account, state.organization, invitation.id) }) }
        }
    }
}
