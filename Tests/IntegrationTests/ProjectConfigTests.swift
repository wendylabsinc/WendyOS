import AppConfig
import ArgumentParser
import Foundation
import Testing

@testable import wendy
@testable import wendy_agent

@Suite
struct ProjectConfigTests {
    func loadConfig(at url: URL) throws -> AppConfig {
        let json = try Data(contentsOf: url.appending(path: "wendy.json"))
        return try JSONDecoder().decode(AppConfig.self, from: json)
    }

    func saveConfig(_ config: AppConfig, at url: URL) throws {
        let json = try JSONEncoder().encode(config)
        try json.write(to: url.appending(path: "wendy.json"))
    }

    func createEmptyProject() async throws -> URL {
        let projectDir = FileManager.default.temporaryDirectory.appending(path: UUID().uuidString)
        try FileManager.default.createDirectory(at: projectDir, withIntermediateDirectories: true)

        var initCommand = InitCommand()
        initCommand.projectPath = projectDir.path()
        initCommand.language = .swift

        try await initCommand.run()

        var config = try loadConfig(at: projectDir)
        config.entitlements = []
        try saveConfig(config, at: projectDir)
        return projectDir
    }

    func createProject() async throws -> URL {
        let projectDir = FileManager.default.temporaryDirectory.appending(path: UUID().uuidString)
        try FileManager.default.createDirectory(at: projectDir, withIntermediateDirectories: true)

        var initCommand = InitCommand()
        initCommand.projectPath = projectDir.path()
        initCommand.language = .swift

        try await initCommand.run()

        let config = try loadConfig(at: projectDir)

        #expect(config.entitlements.contains(.audio))
        #expect(config.entitlements.contains(.bluetooth(.init(mode: .bluez))))
        #expect(config.entitlements.contains(.network(.init(mode: .host))))
        #expect(config.entitlements.contains(.video(.init(mode: .all))))
        #expect(
            config.entitlements.contains(
                .persist(.init(name: "app-\(config.appId)", path: "/mnt/app"))
            )
        )
        #expect(
            config.entitlements.contains(.persist(.init(name: "wendy-shared", path: "/mnt/shared")))
        )
        #expect(config.version == "0.0.1")
        return projectDir
    }

    func removeEntitlement(
        _ entitlement: Entitlement,
        from projectDir: URL
    ) async throws {
        var command = RemoveCommand()
        command.project = projectDir.path()

        switch entitlement {
        case .persist:
            command.entitlementType = .persist
        case .network:
            command.entitlementType = .network
        case .bluetooth:
            command.entitlementType = .bluetooth
        case .video:
            command.entitlementType = .video
        case .audio:
            command.entitlementType = .audio
        case .gpu:
            command.entitlementType = .gpu
        }

        try await command.run()
        let config = try loadConfig(at: projectDir)
        #expect(
            !config.entitlements.contains(entitlement),
            "Entitlement was not successfully added"
        )
    }

    func addEntitlement(
        _ entitlement: Entitlement,
        to projectDir: URL
    ) async throws {
        var command = AddCommand()
        command.project = projectDir.path()

        switch entitlement {
        case .persist(let persistenceEntitlements):
            command.entitlementType = .persist
            command.name = persistenceEntitlements.name
            command.path = persistenceEntitlements.path
        case .gpu:
            command.entitlementType = .gpu
        case .network(let networkEntitlements):
            command.entitlementType = .network
            command.mode = networkEntitlements.mode.rawValue
        case .bluetooth(let bluetoothEntitlements):
            command.entitlementType = .bluetooth
            command.mode = bluetoothEntitlements.mode.rawValue
        case .video:
            command.entitlementType = .video
            command.mode = nil
        case .audio:
            command.entitlementType = .audio
        }

        try await command.run()
        let config = try loadConfig(at: projectDir)

        #expect(config.entitlements.contains(entitlement), "Entitlement was not successfully added")
    }

    @Test func canCreateProject() async throws {
        _ = try await createEmptyProject()
    }

    @Test(
        arguments: [
            Entitlement.bluetooth(BluetoothEntitlements(mode: .kernel)),
            Entitlement.bluetooth(BluetoothEntitlements(mode: .bluez)),
            Entitlement.network(NetworkEntitlements(mode: .host)),
            Entitlement.network(NetworkEntitlements(mode: .none)),
            Entitlement.video(VideoEntitlements()),
        ]
    )
    func canAddEntitlement(
        _ entitlement: Entitlement
    ) async throws {
        let projectDir = try await createEmptyProject()
        try await addEntitlement(entitlement, to: projectDir)
    }

    @Test(
        arguments: [
            Entitlement.bluetooth(BluetoothEntitlements(mode: .kernel)),
            Entitlement.bluetooth(BluetoothEntitlements(mode: .bluez)),
            Entitlement.network(NetworkEntitlements(mode: .host)),
            Entitlement.network(NetworkEntitlements(mode: .none)),
            Entitlement.video(VideoEntitlements()),
        ]
    )
    func canRemoveEntitlement(
        _ entitlement: Entitlement
    ) async throws {
        let projectDir = try await createEmptyProject()
        try await addEntitlement(entitlement, to: projectDir)
        try await removeEntitlement(entitlement, from: projectDir)
    }

    // MARK: - Invalid Key Detection Tests

    @Test
    func invalidEntitlementKeyGeneratesWarning() throws {
        // This JSON has "network" instead of "mode" - a common typo that was silently ignored
        // See: https://github.com/wendylabsinc/samples/commit/14232eca2afc9258ff456ae084ad3d487e31992b
        let invalidJSON = """
            {
                "appId": "com.example.test",
                "version": "1.0.0",
                "entitlements": [
                    {
                        "type": "network",
                        "network": "host"
                    }
                ]
            }
            """

        let data = invalidJSON.data(using: .utf8)!

        // Validation should return a warning about the unknown "network" key
        let warnings = AppConfig.validateJSON(data)
        #expect(warnings.count == 1)
        #expect(warnings[0].contains("network"))
        #expect(warnings[0].contains("Unknown key"))

        // Decoding will still fail because required "mode" key is missing
        #expect(throws: DecodingError.self) {
            _ = try JSONDecoder().decode(AppConfig.self, from: data)
        }
    }

    @Test
    func unknownEntitlementKeyGeneratesWarning() throws {
        // This JSON has an extra unknown key alongside valid ones
        // Unknown keys should generate a warning (not silently ignored)
        let jsonWithUnknownKey = """
            {
                "appId": "com.example.test",
                "version": "1.0.0",
                "entitlements": [
                    {
                        "type": "network",
                        "mode": "host",
                        "unknownKey": "value"
                    }
                ]
            }
            """

        let data = jsonWithUnknownKey.data(using: .utf8)!

        // Validation should return a warning about the unknown key
        let warnings = AppConfig.validateJSON(data)
        #expect(warnings.count == 1)
        #expect(warnings[0].contains("unknownKey"))
        #expect(warnings[0].contains("Unknown key"))

        // Decoding should still succeed (the config is valid, just has extra keys)
        let config = try JSONDecoder().decode(AppConfig.self, from: data)
        #expect(config.entitlements.first == .network(.init(mode: .host)))
    }

    @Test
    func validEntitlementDecodesWithNoWarnings() throws {
        // Ensure valid JSON decodes successfully with no warnings
        let validJSON = """
            {
                "appId": "com.example.test",
                "version": "1.0.0",
                "entitlements": [
                    {
                        "type": "network",
                        "mode": "host"
                    }
                ]
            }
            """

        let data = validJSON.data(using: .utf8)!

        // No warnings should be generated
        let warnings = AppConfig.validateJSON(data)
        #expect(warnings.isEmpty)

        // Decoding should succeed
        let config = try JSONDecoder().decode(AppConfig.self, from: data)
        #expect(config.appId == "com.example.test")
        #expect(config.entitlements.count == 1)
        #expect(config.entitlements.first == .network(.init(mode: .host)))
    }
}
