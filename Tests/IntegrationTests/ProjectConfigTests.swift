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

    func createProject() async throws -> URL {
        let projectDir = FileManager.default.temporaryDirectory.appending(path: UUID().uuidString)
        try FileManager.default.createDirectory(at: projectDir, withIntermediateDirectories: true)

        var initCommand = InitCommand()
        initCommand.projectPath = projectDir.path()

        try await initCommand.run()

        let config = try loadConfig(at: projectDir)

        #expect(config.entitlements.isEmpty)
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
        case .network:
            command.entitlementType = .network
        case .bluetooth:
            command.entitlementType = .bluetooth
        case .video:
            command.entitlementType = .video    case .audio:
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
        _ = try await createProject()
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
        let projectDir = try await createProject()
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
        let projectDir = try await createProject()
        try await addEntitlement(entitlement, to: projectDir)
    }
}
