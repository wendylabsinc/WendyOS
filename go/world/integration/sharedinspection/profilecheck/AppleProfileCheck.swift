// Headless interoperability check. Reads private fixtures; never prints credentials.
import CryptoKit
import Foundation
import WorldClient

@main
struct AppleProfileCheck {
    static func main() throws {
        guard CommandLine.arguments.count == 4,
              let issued = Int64(CommandLine.arguments[3]) else {
            throw CocoaError(.fileReadInvalidFileName)
        }
        let input = URL(fileURLWithPath: CommandLine.arguments[1])
        let output = URL(fileURLWithPath: CommandLine.arguments[2])
        let raw = try Data(contentsOf: input)
        let wrapper = try JSONValue.decode(raw, limit: 64 << 10)
        guard try WorldReadMode(profile: wrapper) == .nativeInference,
              let config = wrapper["credentials"] else { throw WorldClientError.invalidCredentials }
        let credentials = try WorldCredentials(data: config.encoded())
        guard credentials.scopes == ["read", "evidence-read"],
              credentials.audience == "sharedinspection-native-viewer" else {
            throw WorldClientError.invalidCredentials
        }
        let signed = try credentials.signedReadAuthorization(operation: "read", nonce: "apple-viewer-profile-check",
                                                              issuedAtUnixMS: issued)
        try signed.write(to: output, options: .withoutOverwriting)
        try FileManager.default.setAttributes([.posixPermissions: 0o600], ofItemAtPath: output.path)
        let summary: [String: Any] = ["client": "apple", "read_mode": "native_inference", "scopes": ["read", "evidence-read"],
            "profile_bytes": raw.count, "profile_sha256": digest(raw),
            "request_bytes": signed.count, "request_sha256": digest(signed)]
        FileHandle.standardOutput.write(try JSONSerialization.data(withJSONObject: summary, options: [.sortedKeys]))
        FileHandle.standardOutput.write(Data([10]))
    }
    static func digest(_ raw: Data) -> String { SHA256.hash(data: raw).map { String(format: "%02x", $0) }.joined() }
}
