import Darwin
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
    private let keychainStore: KeychainStore
    private let logger = Logger(label: "sh.wendy.agent.provisioning.store")

    init(configPath: URL, keychainStore: KeychainStore = KeychainStore()) {
        self.configPath = configPath
        self.keychainStore = keychainStore
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
    // Removed outputs from older agents are retained only for unprovision cleanup.
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
            backing = .secureEnclave
        } else {
            let storedKey = (try? String(contentsOf: self.keyPath, encoding: .utf8)) ?? ""
            let keyPEM = storedKey.isEmpty ? (state.keyPem ?? "") : storedKey
            guard !keyPEM.isEmpty else { return nil }
            do {
                try self.writeFile(self.keyPath, contents: keyPEM, permissions: 0o600)
            } catch let error where storedKey.isEmpty {
                // A failed migration must leave the legacy JSON key intact.
                self.logger.error("Failed to migrate device key: \(error)")
                return nil
            } catch {
                // The key is already on disk, so like the Go agent, a failed
                // repair keeps the enrollment instead of discarding it.
                self.logger.warning("Failed to repair device key file: \(error)")
            }
            backing = .softwarePEM(keyPEM)
        }

        do {
            if state.keyPem != nil {
                // Retain fields written by a newer agent while removing the
                // migrated secret, matching the Go agent's state migration.
                guard var stripped = try JSONSerialization.jsonObject(with: data) as? [String: Any]
                else {
                    return nil
                }
                stripped.removeValue(forKey: "keyPem")
                try self.writeFile(
                    self.statePath,
                    data: JSONSerialization.data(
                        withJSONObject: stripped,
                        options: [.prettyPrinted, .sortedKeys]
                    ),
                    permissions: 0o600
                )
            } else {
                // Preserve original formatting and unknown fields during normal startup.
                try self.writeFile(self.statePath, data: data, permissions: 0o600)
            }
        } catch {
            self.logger.warning("Failed to repair provisioning state file: \(error)")
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

        // Write the software key FIRST. provisioning.json (with
        // enrolled: true) is the commit marker `load()` treats as the source
        // of truth, so it must be written LAST: if anything above fails, the
        // device is left honestly unprovisioned (no json => load() -> nil)
        // rather than "enrolled" on disk with no private key.
        let keyBackingValue: String?
        switch keyBacking {
        case .softwarePEM(let pem):
            try self.writeFile(self.keyPath, contents: pem, permissions: 0o600)
            keyBackingValue = nil
        case .secureEnclave:
            // No key file: the key never leaves the Secure Enclave.
            try? FileManager.default.removeItem(at: self.keyPath)
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
        try? self.keychainStore.remove(account: "device-key-se")
    }

    /// Avoid replacing correct state. Symlinks and incorrect permissions must
    /// go through the writer even when their readable contents match.
    private func fileMatches(_ url: URL, data: Data, permissions: Int) -> Bool {
        let manager = FileManager.default
        guard
            let before = try? manager.attributesOfItem(atPath: url.path),
            before[.type] as? FileAttributeType == .typeRegular,
            before[.posixPermissions] as? Int == permissions,
            before[.size] as? Int == data.count,
            let contents = try? Data(contentsOf: url), contents == data,
            let after = try? manager.attributesOfItem(atPath: url.path),
            after[.type] as? FileAttributeType == .typeRegular,
            after[.posixPermissions] as? Int == permissions,
            let inode = before[.systemFileNumber] as? NSNumber,
            let device = before[.systemNumber] as? NSNumber,
            after[.systemFileNumber] as? NSNumber == inode,
            after[.systemNumber] as? NSNumber == device
        else { return false }
        return true
    }

    private func writeFile(_ url: URL, contents: String, permissions: Int) throws {
        try self.writeFile(url, data: Data(contents.utf8), permissions: permissions)
    }

    private func writeFile(_ url: URL, data: Data, permissions: Int) throws {
        if self.fileMatches(url, data: data, permissions: permissions) { return }
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
            // umask may have masked bits from createFile's attributes; re-assert.
            try FileManager.default.setAttributes(
                [.posixPermissions: permissions],
                ofItemAtPath: tmpURL.path
            )

            let handle = try FileHandle(forWritingTo: tmpURL)
            do {
                try handle.write(contentsOf: data)
                try handle.synchronize()
                try handle.close()
            } catch {
                try? handle.close()
                throw error
            }

            // Rename over the destination without unlinking it first. Readers
            // see a complete old or new file, and an unexpected directory is
            // rejected without recursively removing its contents.
            let result = tmpURL.path.withCString { source in
                url.path.withCString { destination in
                    unsafe Darwin.rename(source, destination)
                }
            }
            guard result == 0 else {
                throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
            }
            // Make the directory entry durable after the replacement.
            let parent = unsafe Darwin.open(directory.path, O_RDONLY | O_DIRECTORY | O_CLOEXEC)
            guard parent >= 0 else {
                throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
            }
            let synced = Darwin.fsync(parent)
            let syncError = errno
            let closed = Darwin.close(parent)
            guard synced == 0 else {
                throw POSIXError(POSIXErrorCode(rawValue: syncError) ?? .EIO)
            }
            guard closed == 0 else {
                throw POSIXError(POSIXErrorCode(rawValue: errno) ?? .EIO)
            }
        } catch {
            try? FileManager.default.removeItem(at: tmpURL)
            throw error
        }
    }
}
