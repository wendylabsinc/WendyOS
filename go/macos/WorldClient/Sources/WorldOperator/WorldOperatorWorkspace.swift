import SwiftUI

public enum WorldWorkspaceSection: String, CaseIterable, Sendable {
    case operations, missions, annotations, areas, sessions
}

/// Retains the selected profile and local journals while switching Wendy pages.
@MainActor
public final class WorldOperatorSession: ObservableObject {
    let model: OperatorModel
    @Published var page: ConsolePage? = .world
    @Published public private(set) var section: WorldWorkspaceSection = .operations
    private var pendingProfile: URL?

    public func open(_ section: WorldWorkspaceSection) {
        self.section = section
        switch section {
        case .operations: page = .world
        case .missions: page = .missions
        case .annotations: page = .notes
        case .areas: page = .areas
        case .sessions: break
        }
    }

    public init(storageDirectory: URL? = nil, profile: URL? = nil) {
        model = OperatorModel(storageDirectory: storageDirectory)
        pendingProfile = profile
    }

    func run() async {
        if let profile = pendingProfile {
            pendingProfile = nil
            if await model.runDebugStage(profile: profile, arguments: ProcessInfo.processInfo.arguments) { return }
            await model.importProfile(profile)
        }
        guard !Task.isCancelled else { return }
        await model.run()
    }
}

/// Profiles are imported explicitly. Opening this workspace never chooses a
/// gateway or grants mission authority. Hidden workspaces stop participating.
public struct WorldOperatorWorkspace: View {
    @ObservedObject private var session: WorldOperatorSession
    @ObservedObject private var model: OperatorModel
    @Environment(\.scenePhase) private var scenePhase

    public init(session: WorldOperatorSession) {
        self.session = session
        model = session.model
    }

    public var body: some View {
        OperatorView(model: model, page: $session.page)
            .preferredColorScheme(.dark)
            .task { await model.actions.run() }
            .task(id: scenePhase) {
                if scenePhase == .active { await session.run() }
                else { model.setSceneActive(false) }
            }
    }
}
