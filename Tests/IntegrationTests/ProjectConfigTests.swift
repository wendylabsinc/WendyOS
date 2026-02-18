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

    func initializeProject(language: ProjectLanguage) async throws -> URL {
        let projectDir = FileManager.default.temporaryDirectory.appending(path: UUID().uuidString)
        try FileManager.default.createDirectory(at: projectDir, withIntermediateDirectories: true)

        try await initializeProject(language: language, at: projectDir)
        return projectDir
    }

    func initializeProject(language: ProjectLanguage, at projectDir: URL) async throws {
        var initCommand = InitCommand()
        initCommand.projectPath = projectDir.path()
        initCommand.language = language

        try await initCommand.run()
    }

    func createEmptyProject() async throws -> URL {
        let projectDir = try await initializeProject(language: .swift)

        var config = try loadConfig(at: projectDir)
        config.entitlements = []
        try saveConfig(config, at: projectDir)
        return projectDir
    }

    func createProject() async throws -> URL {
        let projectDir = try await initializeProject(language: .swift)
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
        // Initialize optional wrappers explicitly for direct command invocation in tests.
        command.mode = nil
        command.name = nil
        command.path = nil

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

    @Test
    func rustProjectInitCreatesLocalProfile() async throws {
        let projectDir = try await initializeProject(language: .rust)
        let config = try loadConfig(at: projectDir)
        let localProfile = try #require(config.profile(withID: "local-dev"))

        #expect(config.language == "rust")
        #expect(config.defaultProfile == "local-dev")
        #expect(localProfile.when.target == .local)
        #expect(localProfile.run?.command == "cargo run")
        #expect(
            FileManager.default.fileExists(atPath: projectDir.appending(path: "Cargo.toml").path())
        )
        #expect(
            FileManager.default.fileExists(atPath: projectDir.appending(path: "src/main.rs").path())
        )
    }

    @Test
    func cppProjectInitCreatesLocalProfile() async throws {
        let projectDir = try await initializeProject(language: .cpp)
        let config = try loadConfig(at: projectDir)
        let localProfile = try #require(config.profile(withID: "local-dev"))

        #expect(config.language == "cpp")
        #expect(config.defaultProfile == "local-dev")
        #expect(localProfile.when.target == .local)
        #expect(
            localProfile.run?.command
                == "cmake -S . -B build && cmake --build build && ./build/wendy_app"
        )
        #expect(
            FileManager.default.fileExists(
                atPath: projectDir.appending(path: "CMakeLists.txt").path()
            )
        )
        #expect(
            FileManager.default.fileExists(atPath: projectDir.appending(path: "main.cpp").path())
        )
    }

    @Test
    func cppProjectInitSanitizesReservedProjectName() async throws {
        let rootDir = FileManager.default.temporaryDirectory.appending(path: UUID().uuidString)
        let projectDir = rootDir.appending(path: "class")
        try FileManager.default.createDirectory(at: projectDir, withIntermediateDirectories: true)
        try await initializeProject(language: .cpp, at: projectDir)

        let cmakeContents = try String(
            contentsOf: projectDir.appending(path: "CMakeLists.txt"),
            encoding: .utf8
        )
        #expect(cmakeContents.contains("project(app_class VERSION 0.1.0 LANGUAGES CXX)"))
    }

    @Test
    func addAndRemoveEntitlementPreserveTopLevelConfigFields() async throws {
        let projectDir = try await initializeProject(language: .python)
        let baseline = try loadConfig(at: projectDir)

        try await addEntitlement(.gpu(GPUEntitlements()), to: projectDir)
        try await removeEntitlement(.gpu(GPUEntitlements()), from: projectDir)

        let updated = try loadConfig(at: projectDir)

        #expect(updated.appId == baseline.appId)
        #expect(updated.version == baseline.version)
        #expect(updated.language == baseline.language)
        #expect(updated.defaultProfile == baseline.defaultProfile)
        #expect(updated.profiles == baseline.profiles)
        #expect(updated.python == baseline.python)
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
