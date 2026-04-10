import ArgumentParser
import Foundation

enum PermissionKind: String, CaseIterable, Sendable {
    case camera
    case microphone
    case bluetooth
}

enum PermissionStatus: String, Sendable {
    case granted
    case missing
    case unknown
}

struct PermissionSummary: Equatable, Sendable {
    let permission: PermissionKind
    let status: PermissionStatus
}

struct PermissionSetupOutcome: Sendable {
    let summaries: [PermissionSummary]

    var summaryLines: [String] {
        summaries.map { "\($0.permission.rawValue): \($0.status.rawValue)" }
    }

    var warningLines: [String] {
        summaries.compactMap { summary in
            switch summary.status {
            case .granted:
                return nil
            case .missing, .unknown:
                return "Warning: \(summary.permission.rawValue) permission is \(summary.status.rawValue). Run 'wendy-agent setup' to retry permission onboarding."
            }
        }
    }
}

protocol PermissionAuthorizing: Sendable {
    func status(for permission: PermissionKind) async -> PermissionStatus
    func requestAccess(for permission: PermissionKind) async -> PermissionStatus
}

struct PermissionSetupRunner: Sendable {
    let authorizer: PermissionAuthorizing

    func run() async -> PermissionSetupOutcome {
        var summaries: [PermissionSummary] = []

        for permission in PermissionKind.allCases {
            let current = await authorizer.status(for: permission)
            let finalStatus: PermissionStatus
            switch current {
            case .granted:
                finalStatus = .granted
            case .missing, .unknown:
                finalStatus = await authorizer.requestAccess(for: permission)
            }
            summaries.append(PermissionSummary(permission: permission, status: finalStatus))
        }

        return PermissionSetupOutcome(summaries: summaries)
    }
}

struct Setup: AsyncParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "setup",
        abstract: "Request macOS permissions needed by launched apps"
    )

    mutating func run() async throws {
        #if os(macOS)
            let runner = PermissionSetupRunner(authorizer: MacOSPermissionAuthorizer())
            let outcome = await runner.run()

            print("Wendy Agent macOS permission setup")
            for line in outcome.summaryLines {
                print(line)
            }
            for warning in outcome.warningLines {
                fputs(warning + "\n", stderr)
            }
        #else
            throw ValidationError("setup is currently macOS-only")
        #endif
    }
}
