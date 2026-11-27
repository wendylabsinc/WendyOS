import Foundation

/// Explicit launch inputs for the integrated native workspace. These never
/// commission hardware or start a simulator; the selected run already exists.
public struct WorldTwinLaunchRequest {
    let mode: TwinWorkspaceMode
    let input: URL
    let output: URL?
    let mission: URL?
    let viewer: URL?
    let capture: URL?
    public let readOnly: Bool

    public enum InvalidArguments: Error, LocalizedError {
        case missingPath(String), conflictingModes, missingOutput, replayRequiresSession
        public var errorDescription: String? {
            switch self {
            case .missingPath(let flag): "\(flag) requires an absolute file path."
            case .conflictingModes: "Choose one World workspace per launch."
            case .missingOutput: "This review needs an explicit fresh output file."
            case .replayRequiresSession: "Read-only replay requires a 3D session."
            }
        }
    }

    private static let modes: [(String, TwinWorkspaceMode)] = [
        ("--world-session", .session), ("--session", .session),
        ("--camera-profile", .camera), ("--gas-source", .gas),
        ("--gas-change", .gasChange), ("--incident", .incident),
        ("--incident-change", .incidentChange), ("--patrol-debrief", .debrief),
        ("--patrol-schedule-review", .schedule), ("--room-inspection-review", .tour),
        ("--offline-patrol-kits", .offline)
    ]

    public static func recognizes(_ arguments: [String]) -> Bool {
        arguments.contains("--world-replay") || modes.contains { arguments.contains($0.0) }
    }

    public static func parse(_ arguments: [String]) throws -> Self? {
        let selected = modes.filter { arguments.contains($0.0) }
        guard selected.count <= 1 else { throw InvalidArguments.conflictingModes }
        guard let selected = selected.first else {
            if arguments.contains("--world-replay") { throw InvalidArguments.replayRequiresSession }
            return nil
        }
        func path(_ flag: String) throws -> URL? {
            guard let index = arguments.firstIndex(of: flag) else { return nil }
            guard index + 1 < arguments.count, arguments[index + 1].hasPrefix("/"),
                  arguments.filter({ $0 == flag }).count == 1 else { throw InvalidArguments.missingPath(flag) }
            return URL(fileURLWithPath: arguments[index + 1])
        }
        guard let input = try path(selected.0) else { throw InvalidArguments.missingPath(selected.0) }
        let output = try path(selected.1 == .offline ? "--offline-selection" : "--schedule-output")
        if selected.1.reviews && output == nil { throw InvalidArguments.missingOutput }
        let replay = arguments.contains("--world-replay")
        if replay && selected.1 != .session { throw InvalidArguments.replayRequiresSession }
        return Self(mode: selected.1, input: input, output: output,
                    mission: replay ? nil : try path("--mission-config"),
                    viewer: replay ? nil : try path("--viewer-profile"),
                    capture: try path("--capture"), readOnly: replay)
    }
}
