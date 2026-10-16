import Foundation
import Logging

/// On-disk persistence of provisioning state and certificate material. Mirrors
/// the Go agent's layout so behavior (file names, permissions, legacy key
/// migration) matches. The private key is never written into
/// `provisioning.json`; for a software-backed key it lives only in
/// `device-key.pem`. A Secure-Enclave-backed key never touches disk at all —
/// only the SE-wrapped blob lives in the Keychain (`KeychainStore`).
struct ProvisioningStore {
    let configPath: URL
    private let logger = Logger(label: "sh.wendy.agent.provisioning.store")

    init(configPath: URL) {
        self.configPath = configPath
    }

    /// How the device's private key is stored: in the Secure Enclave (no key
    /// material on disk) or as a software PEM file (`device-key.pem`).
    enum KeyBacking: Equatable, Sendable {
        case secureEnclave
        case softwarePEM(String)
    }

    struct LoadedState {
        var enrolled: Bool
        var cloudHost: String
        var orgID: Int32
        var assetID: Int32
        var keyBacking: KeyBacking
        var certPEM: String
        var chainPEM: String
        /// Set for direct PKI (ACME) enrollment; empty for legacy Cloud identities.
        var principalURI: String
    }

    private struct PersistedState: Codable {
        var enrolled: Bool
        var cloudHost: String?
        var orgId: Int32?
        var assetId: Int32?
        var keyPem: String?
        var keyBacking: String?
        var certPem: String?
        var chainPem: String?
        var principalURI: String?
    }

    private var statePath: URL { self.configPath.appendingPathComponent("provisioning.json") }
    private var keyPath: URL { self.configPath.appendingPathComponent("device-key.pem") }
    private var certPath: URL { self.configPath.appendingPathComponent("device.pem") }
    private var caPath: URL { self.configPath.appendingPathComponent("ca.pem") }
    private var markerPath: URL { self.configPath.appendingPathComponent(".provisioned") }

    func load() -> LoadedState? {
        guard let data = try? Data(contentsOf: self.statePath) else { return nil }
        guard let state = try? JSONDecoder().decode(PersistedState.self, from: data), state.enrolled
        else {
            return nil
        }

        let certPEM = state.certPem ?? ""
        let chainPEM = state.chainPem ?? ""

        let backing: KeyBacking
        if state.keyBacking == "secureEnclave" {
            // No key file for SE-backed devices; still restore cert/ca/marker
            // if lost across an update (mirrors the software-key path below).
            if !certPEM.isEmpty {
                try? self.writeFile(self.certPath, contents: certPEM, permissions: 0o644)
                try? self.writeFile(self.caPath, contents: chainPEM, permissions: 0o644)
                try? self.writeFile(self.markerPath, contents: "", permissions: 0o644)
            }
            backing = .secureEnclave
        } else {
            var keyPEM = (try? String(contentsOf: self.keyPath, encoding: .utf8)) ?? ""
            if keyPEM.isEmpty, let legacy = state.keyPem, !legacy.isEmpty {
                // Migrate a legacy in-JSON key into device-key.pem, then rewrite the
                // JSON without it.
                keyPEM = legacy
                try? self.writeFile(self.keyPath, contents: legacy, permissions: 0o600)
                var stripped = state
                stripped.keyPem = nil
                if let rewritten = try? JSONEncoder().encode(stripped) {
                    try? self.writeFile(self.statePath, data: rewritten, permissions: 0o600)
                }
                self.logger.info("Migrated device key from provisioning.json to device-key.pem")
            }

            // Restore individual PEM files if missing (e.g., lost across an update).
            if !keyPEM.isEmpty, !certPEM.isEmpty {
                try? self.writePEMFiles(keyPEM: keyPEM, certPEM: certPEM, chainPEM: chainPEM)
            }

            // Fail closed: an enrolled-but-keyless software device is not usable.
            guard !keyPEM.isEmpty else { return nil }
            backing = .softwarePEM(keyPEM)
        }

        return LoadedState(
            enrolled: true,
            cloudHost: state.cloudHost ?? "",
            orgID: state.orgId ?? 0,
            assetID: state.assetId ?? 0,
            keyBacking: backing,
            certPEM: certPEM,
            chainPEM: chainPEM,
            principalURI: state.principalURI ?? ""
        )
    }

    func save(
        cloudHost: String,
        orgID: Int32,
        assetID: Int32,
        keyBacking: KeyBacking,
        certPEM: String,
        chainPEM: String,
        principalURI: String = ""
    ) throws {
        try FileManager.default.createDirectory(
            at: self.configPath,
            withIntermediateDirectories: true,
            attributes: [.posixPermissions: 0o700]
        )

        // Write the key/cert/ca PEM files FIRST. provisioning.json (with
        // enrolled: true) is the commit marker `load()` treats as the source
        // of truth, so it must be written LAST: if anything above fails, the
        // device is left honestly unprovisioned (no json => load() -> nil)
        // rather than "enrolled" on disk with no private key.
        let keyBackingValue: String?
        switch keyBacking {
        case .softwarePEM(let pem):
            try self.writePEMFiles(keyPEM: pem, certPEM: certPEM, chainPEM: chainPEM)
            keyBackingValue = nil
        case .secureEnclave:
            // No key file: the key never leaves the Secure Enclave.
            try? FileManager.default.removeItem(at: self.keyPath)
            try self.writeFile(self.certPath, contents: certPEM, permissions: 0o644)
            try self.writeFile(self.caPath, contents: chainPEM, permissions: 0o644)
            try self.writeFile(self.markerPath, contents: "", permissions: 0o644)
            keyBackingValue = "secureEnclave"
        }

        // provisioning.json WITHOUT the key, mode 0600.
        let state = PersistedState(
            enrolled: true,
            cloudHost: cloudHost,
            orgId: orgID,
            assetId: assetID,
            keyPem: nil,
            keyBacking: keyBackingValue,
            certPem: certPEM,
            chainPem: chainPEM,
            principalURI: principalURI.isEmpty ? nil : principalURI
        )
        let encoder = JSONEncoder()
        encoder.outputFormatting = [.prettyPrinted, .sortedKeys]
        try self.writeFile(self.statePath, data: encoder.encode(state), permissions: 0o600)
    }

    func clear() throws {
        for url in [self.statePath, self.keyPath, self.certPath, self.caPath, self.markerPath] {
            do {
                try FileManager.default.removeItem(at: url)
            } catch let error as CocoaError where error.code == .fileNoSuchFile {
                continue
            } catch let error as NSError
                where error.domain == NSCocoaErrorDomain && error.code == NSFileNoSuchFileError
            {
                continue
            }
        }
        // Best-effort: also purge any Secure-Enclave-backed key blob so a
        // re-provision on this device starts clean. A missing Keychain item
        // is not an error here.
        try? KeychainStore().remove(account: "device-key-se")
    }

    private func writePEMFiles(keyPEM: String, certPEM: String, chainPEM: String) throws {
        try self.writeFile(self.keyPath, contents: keyPEM, permissions: 0o600)
        try self.writeFile(self.certPath, contents: certPEM, permissions: 0o644)
        try self.writeFile(self.caPath, contents: chainPEM, permissions: 0o644)
        try self.writeFile(self.markerPath, contents: "", permissions: 0o644)
    }

    private func writeFile(_ url: URL, contents: String, permissions: Int) throws {
        try self.writeFile(url, data: Data(contents.utf8), permissions: permissions)
    }

    private func writeFile(_ url: URL, data: Data, permissions: Int) throws {
        let directory = url.deletingLastPathComponent()
        let tmpURL = directory.appendingPathComponent(
            ".\(url.lastPathComponent).\(UUID().uuidString).tmp"
        )

        // Create the temp file with the target permissions applied up front so
        // it never appears on disk at a broader-than-target mode. `createFile`
        // is subject to umask, so we re-assert the mode below as well.
        guard
            FileManager.default.createFile(
                atPath: tmpURL.path,
                contents: nil,
                attributes: [.posixPermissions: permissions]
            )
        else {
            throw CocoaError(.fileWriteUnknown)
        }

        do {
            let handle = try FileHandle(forWritingTo: tmpURL)
            do {
                try handle.write(contentsOf: data)
                try handle.close()
            } catch {
                try? handle.close()
                throw error
            }

            // umask may have masked bits from createFile's attributes; re-assert.
            try FileManager.default.setAttributes(
                [.posixPermissions: permissions],
                ofItemAtPath: tmpURL.path
            )

            // Remove any existing destination first, then rename atomically
            // within the directory so the final path never observes the old
            // (or a partially-written) content.
            if FileManager.default.fileExists(atPath: url.path) {
                try FileManager.default.removeItem(at: url)
            }
            try FileManager.default.moveItem(at: tmpURL, to: url)
        } catch {
            try? FileManager.default.removeItem(at: tmpURL)
            throw error
        }
    }
}
