package dev.wendy.android

import org.json.JSONObject

internal data class VpnNetworkSettings(
    val address4: String,
    val address6: String?,
    val mtu: Int,
    val routes: List<Pair<String, Int>>,
    val internetBackup: Boolean,
) {
    companion object {
        fun parse(config: JSONObject): VpnNetworkSettings {
            val internet = config.optJSONObject("internet")
            val routes = config.list("routes").map { it.text("address") to 32 } + ("100.64.0.53" to 32)
            return VpnNetworkSettings(
                address4 = internet?.getString("address4") ?: "100.64.0.54",
                // Match iOS: capture IPv6 even if a donor only supplies IPv4.
                address6 = internet?.let { it.text("address6").ifBlank { "fd2b:677a:9d11::ffff" } },
                mtu = internet?.getInt("mtu") ?: 1400,
                routes = if (internet == null) routes else routes + listOf("0.0.0.0" to 0, "::" to 0),
                internetBackup = internet != null,
            )
        }
    }
}
