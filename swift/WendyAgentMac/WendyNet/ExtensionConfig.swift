import Foundation
import WendyAgentCore
import os

private let extensionConfigLog = Logger(
    subsystem: "sh.wendy.WendyAgentMac.WendyNet",
    category: "ExtensionConfig"
)

struct ExtensionConfig {
    let cloudGRPC: String
    let credentials: WendyCloudCredentials
    let directory: WendyMeshDirectory

    /// Persistent preferences contain only non-secret settings. Secrets and the live directory
    /// arrive in the one-shot options payload supplied by the host when it starts the tunnel.
    static func load(
        providerConfiguration: [String: Any]?,
        options: [String: Any]?
    ) -> ExtensionConfig? {
        guard let cfg = providerConfiguration else {
            extensionConfigLog.error("provider configuration is missing")
            return nil
        }
        guard let cloudGRPC = cfg["cloudGRPC"] as? String else {
            extensionConfigLog.error("provider configuration is incomplete")
            return nil
        }
        guard let blob = options?["credentialBlob"] as? Data,
            let directoryData = options?["deviceDirectory"] as? Data
        else {
            extensionConfigLog.error("provider start options are incomplete")
            return nil
        }
        guard let credentials = try? JSONDecoder().decode(WendyCloudCredentials.self, from: blob)
        else {
            extensionConfigLog.error("shared credential payload is invalid")
            return nil
        }
        guard let directory = try? WendyMeshDirectory.decode(directoryData) else {
            extensionConfigLog.error("device directory payload is invalid")
            return nil
        }
        return ExtensionConfig(
            cloudGRPC: cloudGRPC,
            credentials: credentials,
            directory: directory
        )
    }
}
