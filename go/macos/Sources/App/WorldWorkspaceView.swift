#if os(macOS)
import SwiftUI
import WorldOperator
import WorldTwin

struct WorldWorkspaceView: View {
    @ObservedObject var session: WorldOperatorSession
    private let launch: WorldTwinLaunchRequest?
    private let launchError: String?
    private let render: WorldTimelineRenderRequest?
    @State private var selectedPage: String

    init(session: WorldOperatorSession, launch: WorldTwinLaunchRequest?, launchError: String?, render: WorldTimelineRenderRequest? = nil) {
        self.session = session; self.launch = launch; self.launchError = launchError
        self.render = render
        _selectedPage = State(initialValue: session.section == .sessions ? "sessions" : "operations")
    }

    var body: some View {
        VStack(spacing: 0) {
            HStack {
                Picker("World", selection: $selectedPage) {
                    Text("Operations").tag("operations")
                    Text("Digital twin sessions").tag("sessions")
                }.pickerStyle(.segmented).frame(width: 330).disabled(render != nil)
                Spacer()
            }.padding(12)
            Text("Chat uses the World profile selected at app startup. This workspace imports its own profile; navigation transfers no identity or authority.")
                .font(.caption).foregroundStyle(.secondary)
                .frame(maxWidth: .infinity, alignment: .leading).padding(.horizontal, 12).padding(.bottom, 8)
            if let launchError { Text(launchError).foregroundStyle(.orange).padding(12) }
            Divider()
            // An explicit export owns this destination for its full lifetime.
            // Restored picker state or operator routing must not remove its
            // task after the first frame. Leaving World still cancels export.
            if let render {
                WorldTimelineRenderView(request: render)
            } else if selectedPage == "operations" {
                WorldOperatorWorkspace(session: session)
            } else {
                WorldTwinWorkspace(launch: launch)
            }
        }.accessibilityIdentifier("world-workspace")
            .onChange(of: session.section) { _, section in
                selectedPage = section == .sessions ? "sessions" : "operations"
            }
    }
}
#endif
