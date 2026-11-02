import SwiftUI

struct SiteFogControls: View {
    @Binding var configuration: SiteFogConfiguration
    let summary: SiteFogSummary

    var body: some View {
        VStack(alignment: .leading, spacing: 6) {
            HStack {
                Toggle("Fog of war", isOn: $configuration.enabled)
                    .toggleStyle(.switch)
                    .fixedSize()
                if configuration.enabled {
                    Button("Reset", systemImage: "arrow.counterclockwise") { configuration.reset = UUID() }
                        .help("Clear exploration history for this view. This does not move or stop Go2.")
                }
            }
            if configuration.enabled {
                if summary.available {
                    Text(String(format: "%.1f m² explored · %@", summary.exploredArea,
                                summary.receiving ? "Live position" : "Waiting for a confirmed live position"))
                        .font(.caption.monospacedDigit())
                    Text("Dark: hidden · Dim: explored · Bright: visible")
                        .font(.caption)
                    Text("Approximate 2.5 m visibility from the saved scan. Movement controls are unchanged.")
                        .font(.caption2).foregroundStyle(.secondary)
                } else {
                    Text("Fog of war needs a model with valid floor bounds.")
                        .font(.caption).foregroundStyle(.secondary)
                }
            }
        }
    }
}
