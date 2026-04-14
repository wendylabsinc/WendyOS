import SwiftUI

@main
struct WendyAgentApp: App {
    @StateObject private var appState: WendyAgentAppState

    init() {
        let appState = WendyAgentAppState()
        self._appState = StateObject(wrappedValue: appState)
        appState.startIfNeeded()
    }

    var body: some Scene {
        MenuBarExtra {
            switch self.appState.status {
            case .failed(let message):
                Text(message)
                    .font(.footnote)
                    .foregroundStyle(.secondary)
                    .fixedSize(horizontal: false, vertical: true)
                Divider()
            case .idle, .starting, .running, .stopping, .stopped:
                EmptyView()
            }

            Button("Quit WendyAgent") {
                self.appState.quit()
            }
            .keyboardShortcut("q")
        } label: {
            ZStack(alignment: .topTrailing) {
                Image("StatusIcon")

                if case .failed = self.appState.status {
                    Image(systemName: "exclamationmark.circle.fill")
                        .symbolRenderingMode(.palette)
                        .foregroundStyle(.white, .red)
                        .font(.system(size: 8, weight: .bold))
                        .offset(x: 4, y: -4)
                }
            }
            .help("WendyAgent")
        }
    }
}
