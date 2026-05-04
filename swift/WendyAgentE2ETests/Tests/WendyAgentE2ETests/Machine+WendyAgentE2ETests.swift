import Foundation
import Testing
public import WendyE2ETesting

extension Machine {
    public static func cli(ssh: String? = nil, verbose: Bool = false) async throws -> Machine {
        let machine = Machine(
            name: "CLI",
            ssh: ssh,
            workingDirectory: Helper.repositoryRootDirectoryURL().appendingPathComponent("go").path,
            verbose: verbose
        )

        try await buildCLIOnce.perform {
            try await machine.run("make build-cli") { standardOutput, _ in
                #expect(standardOutput.contains(/go build .* bin\/wendy/))
            }
        }

        return machine
    }

    public static func agent(ssh: String? = nil, verbose: Bool = false) async throws -> Machine {
        let machine = Machine(
            name: "Agent",
            ssh: ssh,
            workingDirectory: Helper.repositoryRootDirectoryURL().appendingPathComponent("swift").path,
            verbose: verbose
        )

        try await buildAgentOnce.perform {
            try await machine.run("make build-dev") { standardOutput, _ in
                #expect(
                    standardOutput.contains(
                        /Created macOS app artifact: .*wendy-agent-macos-arm64-.*\.zip/
                    )
                )
            }
        }

        return machine
    }

    // MARK: - Private

    private static let buildCLIOnce = Once(name: "build CLI")
    private static let buildAgentOnce = Once(name: "build agent")
}
