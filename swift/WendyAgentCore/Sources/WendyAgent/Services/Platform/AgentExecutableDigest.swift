import CryptoKit
import Foundation

/// Hashes the executable mapped by this agent process. The CLI extracts and
/// hashes the same file from a macOS update ZIP, so a match proves that the
/// replacement bundle launched even when its version string did not change.
enum AgentExecutableDigest {
    static func sha256(at url: URL?) -> String {
        guard let url, let data = try? Data(contentsOf: url, options: .mappedIfSafe) else {
            return ""
        }
        return SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
    }
}
