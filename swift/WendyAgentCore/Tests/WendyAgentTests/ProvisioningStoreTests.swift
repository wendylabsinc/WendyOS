import Foundation
import Testing

@testable import WendyAgentCore

@Suite("ProvisioningStore")
struct ProvisioningStoreTests {
    private func tempDir() -> URL {
        let dir = FileManager.default.temporaryDirectory
            .appendingPathComponent("wendy-prov-\(UUID().uuidString)", isDirectory: true)
        return dir
    }

    private func store(at dir: URL) -> ProvisioningStore {
        // Clearing a fixture must never remove the real agent's Keychain item.
        ProvisioningStore(
            configPath: dir,
            keychainStore: KeychainStore(service: "sh.wendy.agent.tests.\(UUID().uuidString)")
        )
    }

    @Test("save then load round-trips and never writes the key into provisioning.json")
    func roundTrip() throws {
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let store = self.store(at: dir)

        try store.save(
            cloudHost: "cloud.example:50051",
            orgID: 7,
            assetID: 42,
            keyBacking: .softwarePEM("KEYPEM"),
            certPEM: "CERTPEM",
            chainPEM: "CHAINPEM"
        )

        let loaded = try #require(store.load())
        #expect(loaded.enrolled)
        #expect(loaded.cloudHost == "cloud.example:50051")
        #expect(loaded.orgID == 7)
        #expect(loaded.assetID == 42)
        #expect(loaded.keyBacking == .softwarePEM("KEYPEM"))
        #expect(loaded.certPEM == "CERTPEM")
        #expect(loaded.chainPEM == "CHAINPEM")

        // Key must NOT be in provisioning.json.
        let json = try String(
            contentsOf: dir.appendingPathComponent("provisioning.json"),
            encoding: .utf8
        )
        #expect(!json.contains("KEYPEM"))
        #expect(
            try FileManager.default.contentsOfDirectory(atPath: dir.path).sorted()
                == ["device-key.pem", "provisioning.json"]
        )
    }

    @Test("legacy keyPem in provisioning.json migrates into device-key.pem")
    func legacyMigration() throws {
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)
        let legacy = """
            {"enrolled":true,"cloudHost":"c:50051","orgId":1,"assetId":2,\
            "keyPem":"LEGACYKEY","certPem":"C","chainPem":"CH","futureState":{"generation":7}}
            """
        try legacy.write(
            to: dir.appendingPathComponent("provisioning.json"),
            atomically: true,
            encoding: .utf8
        )

        let store = self.store(at: dir)
        let loaded = try #require(store.load())
        #expect(loaded.keyBacking == .softwarePEM("LEGACYKEY"))
        // Migrated to device-key.pem and stripped from json.
        let migratedKey = try String(
            contentsOf: dir.appendingPathComponent("device-key.pem"),
            encoding: .utf8
        )
        #expect(migratedKey == "LEGACYKEY")
        let json = try String(
            contentsOf: dir.appendingPathComponent("provisioning.json"),
            encoding: .utf8
        )
        #expect(!json.contains("LEGACYKEY"))
        let migrated = try #require(
            JSONSerialization.jsonObject(with: Data(json.utf8)) as? [String: Any]
        )
        #expect((migrated["futureState"] as? [String: Int])?["generation"] == 7)

    }

    @Test("save leaves the device unprovisioned if writing the key fails")
    func saveFailureDoesNotMarkEnrolled() throws {
        let dir = tempDir()
        let keyPath = dir.appendingPathComponent("device-key.pem")
        defer {
            // Restore write permissions so cleanup can actually remove everything.
            try? FileManager.default.setAttributes(
                [.posixPermissions: 0o700],
                ofItemAtPath: keyPath.path
            )
            try? FileManager.default.setAttributes(
                [.posixPermissions: 0o700],
                ofItemAtPath: dir.path
            )
            try? FileManager.default.removeItem(at: dir)
        }
        // The config directory itself stays writable: a provisioning.json
        // write would succeed here if `save` attempted it.
        try FileManager.default.createDirectory(at: dir, withIntermediateDirectories: true)

        // Only the key destination is blocked; the config directory stays
        // writable, so a JSON-first save would incorrectly mark it enrolled.
        try FileManager.default.createDirectory(at: keyPath, withIntermediateDirectories: true)
        try "blocker".write(
            to: keyPath.appendingPathComponent("blocker"),
            atomically: true,
            encoding: .utf8
        )
        try FileManager.default.setAttributes(
            [.posixPermissions: 0o500],
            ofItemAtPath: keyPath.path
        )

        let store = self.store(at: dir)

        #expect(throws: (any Error).self) {
            try store.save(
                cloudHost: "c:50051",
                orgID: 1,
                assetID: 2,
                keyBacking: .softwarePEM("KEYPEM"),
                certPEM: "CERTPEM",
                chainPEM: "CHAINPEM"
            )
        }

        // The device must NOT be left looking enrolled: provisioning.json
        // (the commit marker) must not exist, and load() must return nil.
        #expect(
            !FileManager.default.fileExists(
                atPath: dir.appendingPathComponent("provisioning.json").path
            )
        )
        #expect(store.load() == nil)
    }

    @Test("clear removes every artifact and tolerates missing files")
    func clearRemovesAll() throws {
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let store = self.store(at: dir)
        try store.save(
            cloudHost: "c:50051",
            orgID: 1,
            assetID: 2,
            keyBacking: .softwarePEM("K"),
            certPEM: "C",
            chainPEM: "CH"
        )

        // Unprovision still removes copies emitted by older agents.
        for name in ["device.pem", "ca.pem", ".provisioned"] {
            try "legacy".write(
                to: dir.appendingPathComponent(name),
                atomically: true,
                encoding: .utf8
            )
        }

        try store.clear()

        #expect(store.load() == nil)
        for name in ["provisioning.json", "device-key.pem", "device.pem", "ca.pem", ".provisioned"]
        {
            #expect(!FileManager.default.fileExists(atPath: dir.appendingPathComponent(name).path))
        }
        // Second clear on an empty dir does not throw.
        try store.clear()
    }

    @Test("Secure-Enclave-backed save persists no device-key.pem, and round-trips")
    func secureEnclaveBackingPersistsNoKeyFile() throws {
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let store = self.store(at: dir)

        try store.save(
            cloudHost: "h",
            orgID: 1,
            assetID: 2,
            keyBacking: .secureEnclave,
            certPEM: "CERT",
            chainPEM: "CHAIN"
        )

        #expect(
            !FileManager.default.fileExists(
                atPath: dir.appendingPathComponent("device-key.pem").path
            )
        )
        let loaded = try #require(store.load())
        #expect(loaded.keyBacking == .secureEnclave)
        #expect(loaded.certPEM == "CERT")
        #expect(loaded.chainPEM == "CHAIN")
        #expect(
            try FileManager.default.contentsOfDirectory(atPath: dir.path) == ["provisioning.json"]
        )
    }
    @Test("unchanged saves and loads preserve key and JSON metadata")
    func unchangedStatePreservesFiles() throws {
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let store = self.store(at: dir)
        func save() throws {
            try store.save(
                cloudHost: "h",
                orgID: 1,
                assetID: 2,
                keyBacking: .softwarePEM("KEY"),
                certPEM: "CERT",
                chainPEM: "CHAIN"
            )
        }
        try save()
        var before: [String: NSDictionary] = [:]
        for name in ["device-key.pem", "provisioning.json"] {
            let path = dir.appendingPathComponent(name).path
            try FileManager.default.setAttributes(
                [.modificationDate: Date(timeIntervalSince1970: 1_700_000_000)],
                ofItemAtPath: path
            )
            before[name] = try FileManager.default.attributesOfItem(atPath: path) as NSDictionary
        }
        try save()
        #expect(store.load() != nil)
        #expect(store.load() != nil)
        for (name, attrs) in before {
            let after = try FileManager.default.attributesOfItem(
                atPath: dir.appendingPathComponent(name).path
            )
            for key: FileAttributeKey in [
                .systemFileNumber, .posixPermissions, .modificationDate, .ownerAccountID,
                .groupOwnerAccountID,
            ] {
                #expect((after[key] as? NSObject) == (attrs[key] as? NSObject))
            }
        }
    }

    @Test("changed contents, modes and symlinks are repaired for both state files")
    func repairsStateFiles() throws {
        for name in ["device-key.pem", "provisioning.json"] {
            for damage in ["contents", "mode", "missing", "symlink"] {
                let dir = tempDir()
                defer { try? FileManager.default.removeItem(at: dir) }
                let store = self.store(at: dir)
                func save() throws {
                    try store.save(
                        cloudHost: "h",
                        orgID: 1,
                        assetID: 2,
                        keyBacking: .softwarePEM("KEY"),
                        certPEM: "CERT",
                        chainPEM: "CHAIN"
                    )
                }
                try save()
                let url = dir.appendingPathComponent(name)
                let original = try Data(contentsOf: url)
                let target = dir.appendingPathComponent("symlink-target")
                switch damage {
                case "contents":
                    var changed = original
                    changed[0] ^= 1
                    try changed.write(to: url)
                case "mode":
                    try FileManager.default.setAttributes(
                        [.posixPermissions: 0o644],
                        ofItemAtPath: url.path
                    )
                case "missing":
                    try FileManager.default.removeItem(at: url)
                default:
                    try original.write(to: target)
                    try FileManager.default.removeItem(at: url)
                    try FileManager.default.createSymbolicLink(at: url, withDestinationURL: target)
                }
                try save()
                #expect(try Data(contentsOf: url) == original)
                let attributes = try FileManager.default.attributesOfItem(atPath: url.path)
                #expect(attributes[.type] as? FileAttributeType == .typeRegular)
                #expect(attributes[.posixPermissions] as? Int == 0o600)
                if damage == "symlink" { #expect(try Data(contentsOf: target) == original) }
            }
        }
    }

    @Test("a failed legacy key migration preserves its only saved copy")
    func failedLegacyMigrationRetainsKey() throws {
        let dir = tempDir()
        defer { try? FileManager.default.removeItem(at: dir) }
        let key = dir.appendingPathComponent("device-key.pem")
        try FileManager.default.createDirectory(at: key, withIntermediateDirectories: true)
        try "blocker".write(
            to: key.appendingPathComponent("blocker"),
            atomically: true,
            encoding: .utf8
        )
        try FileManager.default.setAttributes([.posixPermissions: 0o500], ofItemAtPath: key.path)
        defer {
            try? FileManager.default.setAttributes(
                [.posixPermissions: 0o700],
                ofItemAtPath: key.path
            )
        }
        let state = dir.appendingPathComponent("provisioning.json")
        let legacy = #"{"enrolled":true,"keyPem":"LEGACY","certPem":"CERT"}"#
        try legacy.write(to: state, atomically: true, encoding: .utf8)
        #expect(self.store(at: dir).load() == nil)
        #expect(try String(contentsOf: state, encoding: .utf8) == legacy)
    }

    @Test("a failed state file repair keeps the device enrolled")
    func failedRepairKeepsEnrollment() throws {
        let dir = tempDir()
        defer {
            try? FileManager.default.setAttributes(
                [.posixPermissions: 0o700],
                ofItemAtPath: dir.path
            )
            try? FileManager.default.removeItem(at: dir)
        }
        let store = self.store(at: dir)
        try store.save(
            cloudHost: "h",
            orgID: 1,
            assetID: 2,
            keyBacking: .softwarePEM("KEY"),
            certPEM: "CERT",
            chainPEM: "CHAIN"
        )
        // Both files need a mode repair, and a read-only directory makes it fail.
        for name in ["device-key.pem", "provisioning.json"] {
            try FileManager.default.setAttributes(
                [.posixPermissions: 0o644],
                ofItemAtPath: dir.appendingPathComponent(name).path
            )
        }
        try FileManager.default.setAttributes([.posixPermissions: 0o500], ofItemAtPath: dir.path)

        let loaded = try #require(store.load())
        #expect(loaded.enrolled)
        #expect(loaded.keyBacking == .softwarePEM("KEY"))
        #expect(loaded.certPEM == "CERT")
    }

    @Test("saving never removes a directory at a state file path")
    func savePreservesUnexpectedDirectory() throws {
        for name in ["device-key.pem", "provisioning.json"] {
            let dir = tempDir()
            defer { try? FileManager.default.removeItem(at: dir) }
            let store = self.store(at: dir)
            let path = dir.appendingPathComponent(name)
            try FileManager.default.createDirectory(at: path, withIntermediateDirectories: true)
            let sentinel = path.appendingPathComponent("keep")
            try Data("preserved".utf8).write(to: sentinel)
            #expect(throws: (any Error).self) {
                try store.save(
                    cloudHost: "c",
                    orgID: 1,
                    assetID: 2,
                    keyBacking: .softwarePEM("KEY"),
                    certPEM: "CERT",
                    chainPEM: "CHAIN"
                )
            }
            #expect(try Data(contentsOf: sentinel) == Data("preserved".utf8))
        }
    }

}
