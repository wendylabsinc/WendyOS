/// Convert a 32-char hex string from /proc/net/if_inet6 to canonical IPv6 notation.
/// Produces compressed form (e.g., `fe80::1`) matching `ip neigh` output.
///
/// The input is the raw hex representation without colons or separators,
/// e.g. `fe800000000000000000000000000001` for `fe80::1`.
package func formatIPv6FromHex(_ hex: String) -> String? {
    guard hex.count == 32 else { return nil }
    var groups: [UInt16] = []
    var index = hex.startIndex
    for _ in 0..<8 {
        let end = hex.index(index, offsetBy: 4)
        guard let value = UInt16(hex[index..<end], radix: 16) else { return nil }
        groups.append(value)
        index = end
    }

    // Find the longest run of consecutive zero groups for :: compression
    var bestStart = -1
    var bestLen = 0
    var curStart = -1
    var curLen = 0
    for i in 0..<8 {
        if groups[i] == 0 {
            if curStart < 0 { curStart = i }
            curLen += 1
            if curLen > bestLen {
                bestStart = curStart
                bestLen = curLen
            }
        } else {
            curStart = -1
            curLen = 0
        }
    }

    // Build the string with :: for the longest zero run (must be >= 2 groups)
    var parts: [String] = []
    var i = 0
    while i < 8 {
        if i == bestStart && bestLen >= 2 {
            parts.append(i == 0 ? ":" : "")
            i += bestLen
            if i == 8 { parts.append("") }
        } else {
            parts.append(String(groups[i], radix: 16))
            i += 1
        }
    }
    return parts.joined(separator: ":")
}
