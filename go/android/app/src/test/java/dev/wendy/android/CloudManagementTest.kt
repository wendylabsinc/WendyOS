package dev.wendy.android

import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test

class CloudManagementTest {
    private val overview = JSONObject("""{
        "Organization":{"id":"o1","name":"Wendy Labs"},
        "Members":[{"Member":{"user_id":"u1","role_id":"r1"},"Name":"Ada","Email":"ada@example.com"},{"Member":{"user_id":"u2"},"Email":"bob@example.com","ProfileError":"profile unavailable"}],
        "Roles":[{"id":"r1","name":"Admin","permissions":["member:add","device-access:create"]}],
        "Devices":[{"id":"d1","name":"thor","is_compute_device":true},{"id":"s1","name":"site","is_compute_device":false}],
        "Access":[{"id":"a1","principal_type":1,"principal_id":"u1","target_type":4,"effect":1},{"id":"a2","principal_type":1,"principal_id":"u1","target_type":1,"target_ref":"d1","target_display_name":"thor","effect":2}],
        "Invites":[{"id":"i1","role":"viewer","expires_at":{"seconds":4102444800}},{"id":"i2","expires_at":{"seconds":1},"revoked":false},{"id":"i3","expires_at":{"seconds":4102444800},"revoked":true}],
        "Permissions":["member:add","invite:create"],
        "AccessError":""
    }""")

    @Test fun overviewReadsMembersRolesRulesAndInvitations() {
        val snapshot = CloudOrganizationSnapshot(overview)
        assertEquals("Wendy Labs", snapshot.name)
        assertEquals(listOf("Ada", "bob@example.com"), snapshot.members.map { it.title })
        assertEquals("Admin", snapshot.roleName("r1"))
        assertEquals("No assigned role", snapshot.roleName(""))
        assertEquals(listOf("d1" to "thor"), snapshot.devices)
        assertEquals("All devices", snapshot.rules[0].target)
        assertEquals("Organization", snapshot.rules[0].scope)
        assertEquals("Deny", snapshot.rules[1].effectName)
        assertEquals("thor", snapshot.rules[1].target)
        assertEquals(listOf("Active", "Expired", "Revoked"), snapshot.invitations.map { it.state(now = 1_000_000) })
        assertEquals(setOf("member:add", "invite:create"), snapshot.permissions)
    }
}
