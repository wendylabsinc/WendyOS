import AppKit
import SwiftUI
import UniformTypeIdentifiers

/// These routes preserve the original run and review contracts. Opening a run
/// does not launch a simulator, bind physical hardware or invent observations.
enum TwinWorkspaceMode: String, CaseIterable, Identifiable {
    case session = "3D session"
    case camera = "Camera"
    case gas = "Gas sources"
    case gasChange = "Gas changes"
    case incident = "Incidents"
    case incidentChange = "Rising incidents"
    case debrief = "Patrol debrief"
    case schedule = "Patrol schedule"
    case tour = "Room inspection"
    case offline = "Offline patrol kits"

    var id: String { rawValue }
    var reviews: Bool { self == .schedule || self == .tour || self == .offline }
    var selectsFile: Bool { self == .camera || reviews }
    var purpose: String {
        switch self {
        case .session: "Open the viewer directory of an existing simulation or recorded session. Original layers, evidence and available controls remain attached to that run."
        case .camera: "Open a camera profile. The profile names the existing capture service and calibration; it does not install firmware or start a robot."
        case .gas, .gasChange, .incident, .incidentChange: "Open the reader directory of an existing simulated sensor-fusion session. Original evidence, freshness and available simulation controls remain visible."
        case .debrief: "Open a retained patrol report directory to inspect findings and retrieve original evidence."
        case .schedule, .tour: "Open a prepared review document and choose a fresh output file. Only an explicit review saves approval for the waiting simulation launcher."
        case .offline: "Open a prepared offline kit catalog and choose a fresh selection file. Activation requires a separate explicit choice."
        }
    }
}

private final class ScopedTwinFile: Identifiable {
    let url: URL
    let id = UUID()
    private let accessed: Bool
    init(_ url: URL) { self.url = url; accessed = url.startAccessingSecurityScopedResource() }
    deinit { if accessed { url.stopAccessingSecurityScopedResource() } }
}

private struct TwinWorkspaceSelection: Identifiable {
    let id = UUID()
    let mode: TwinWorkspaceMode
    let input: ScopedTwinFile
    let output: ScopedTwinFile?
}

public struct WorldTwinWorkspace: View {
    @State private var mode: TwinWorkspaceMode = .session
    @State private var selection: TwinWorkspaceSelection?
    @State private var importing = false
    @State private var importingMission = false
    @State private var importingViewer = false
    @State private var missionProfile: ScopedTwinFile?
    @State private var viewerProfile: ScopedTwinFile?
    @State private var message: String?

    private let readOnly: Bool
    private let capture: URL?

    public init(launch: WorldTwinLaunchRequest? = nil) {
        readOnly = launch?.readOnly ?? false
        capture = launch?.capture
        if let launch {
            _mode = State(initialValue: launch.mode)
            _selection = State(initialValue: TwinWorkspaceSelection(mode: launch.mode, input: ScopedTwinFile(launch.input), output: launch.output.map(ScopedTwinFile.init)))
            _missionProfile = State(initialValue: launch.mission.map(ScopedTwinFile.init))
            _viewerProfile = State(initialValue: launch.viewer.map(ScopedTwinFile.init))
        }
    }

    public var body: some View {
        VStack(spacing: 0) {
            HStack(spacing: 12) {
                Picker("Workspace", selection: $mode) {
                    ForEach(TwinWorkspaceMode.allCases) { Text($0.rawValue).tag($0) }
                }.frame(maxWidth: 220).disabled(selection != nil || readOnly)
                if let selection {
                    Text(selection.input.url.lastPathComponent).lineLimit(1).foregroundStyle(.secondary)
                    Spacer()
                    Button("Close session") { closeSelection() }
                } else {
                    Button(mode.selectsFile ? "Open document…" : "Open session folder…") { importing = true }
                    Spacer()
                    if !readOnly {
                        InvestigationLauncher()
                        MissionHistoryLauncher()
                    }
                }
            }.padding(14)
            Divider()
            if let selection {
                GeometryReader { geometry in
                    ScrollView([.horizontal, .vertical]) {
                        content(selection).id(selection.id)
                            .frame(width: max(1100, geometry.size.width), height: max(720, geometry.size.height))
                    }
                }.frame(maxWidth: .infinity, maxHeight: .infinity)
            } else {
                VStack(alignment: .leading, spacing: 20) {
                    Label(mode.rawValue, systemImage: "cube.transparent").font(.largeTitle)
                    Text(mode.purpose).foregroundStyle(.secondary).frame(maxWidth: 680, alignment: .leading)
                    if mode == .session && !readOnly {
                        GroupBox("Optional profiles for this session") {
                            VStack(alignment: .leading, spacing: 10) {
                                HStack {
                                    Button("Choose mission profile…") { importingMission = true }
                                    Text(missionProfile?.url.lastPathComponent ?? "No mission authority imported").foregroundStyle(.secondary)
                                    if missionProfile != nil { Button("Remove") { missionProfile = nil } }
                                }
                                HStack {
                                    Button("Choose evidence profile…") { importingViewer = true }
                                    Text(viewerProfile?.url.lastPathComponent ?? "Use retained session evidence").foregroundStyle(.secondary)
                                    if viewerProfile != nil { Button("Remove") { viewerProfile = nil } }
                                }
                            }.padding(8)
                        }
                    }
                    if let message { Text(message).foregroundStyle(.orange) }
                }.padding(32).frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
            }
        }
        .fileImporter(isPresented: $importing, allowedContentTypes: mode.selectsFile ? [.json] : [.folder]) { result in
            switch result {
            case .success(let url): open(url)
            case .failure(let error): message = error.localizedDescription
            }
        }
        .fileImporter(isPresented: $importingMission, allowedContentTypes: [.json]) { result in
            if case .success(let url) = result { missionProfile = ScopedTwinFile(url) }
        }
        .fileImporter(isPresented: $importingViewer, allowedContentTypes: [.json]) { result in
            if case .success(let url) = result { viewerProfile = ScopedTwinFile(url) }
        }
        .onDisappear { cancelReview() }
    }

    private func cancelReview() {
        guard let selection, selection.mode.reviews, let output = selection.output else { return }
        WorldReviewCancellation.write(to: output.url)
    }

    private func closeSelection() {
        cancelReview()
        selection = nil
    }

    private func open(_ url: URL) {
        let input = ScopedTwinFile(url)
        message = nil
        guard mode.reviews else {
            selection = TwinWorkspaceSelection(mode: mode, input: input, output: nil)
            return
        }
        let panel = NSSavePanel()
        panel.title = "Choose the review output"
        panel.message = "Choose a new file for the waiting simulation launcher. Existing files are never replaced."
        panel.allowedContentTypes = [.json]
        panel.nameFieldStringValue = "wendy-review-\(UUID().uuidString).json"
        let reviewedMode = mode
        panel.begin { response in
            guard response == .OK, let output = panel.url else { return }
            guard !FileManager.default.fileExists(atPath: output.path) else {
                message = "Choose a new output file; an existing approval cannot be replaced."
                return
            }
            selection = TwinWorkspaceSelection(mode: reviewedMode, input: input, output: ScopedTwinFile(output))
        }
    }

    @ViewBuilder private func content(_ selection: TwinWorkspaceSelection) -> some View {
        let url = selection.input.url
        switch selection.mode {
        case .session: EmbeddedTwinSession(directory: url, mission: missionProfile?.url, viewer: viewerProfile?.url, readOnly: readOnly, capture: capture)
        case .camera: StationaryCameraContent(profileURL: url)
        case .gas: GasSourceContent(session: url)
        case .gasChange: GasChangeContent(session: url)
        case .incident: IncidentContent(session: url)
        case .incidentChange: IncidentChangeContent(session: url)
        case .debrief: PatrolDebriefView(directory: url)
        case .schedule, .tour:
            if let output = selection.output {
                PatrolReviewRouter(onClose: { closeSelection() }, tour: selection.mode == .tour, inputURL: url, outputURL: output.url)
            }
        case .offline:
            if let output = selection.output {
                PatrolOfflineChooser(inputURL: url, outputURL: output.url, onClose: { closeSelection() })
            }
        }
    }
}

private struct EmbeddedTwinSession: View {
    @StateObject private var model: DemoModel
    private let mission: URL?

    init(directory: URL, mission: URL?, viewer: URL?, readOnly: Bool, capture: URL?) {
        _model = StateObject(wrappedValue: DemoModel(session: directory, capture: capture?.path, viewerProfile: viewer, readOnly: readOnly))
        self.mission = mission
    }

    var body: some View {
        DemoContent(model: model, missionConfig: mission)
            .frame(minWidth: 1100, minHeight: 720)
            .preferredColorScheme(.light)
            .onAppear { model.start() }
            .onDisappear { model.close() }
            .task { await model.actions.run() }
    }
}
