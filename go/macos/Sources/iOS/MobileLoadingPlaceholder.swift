import SwiftUI

/// Shapes describe the incoming layout without presenting invented content or progress.
/// Keep one accessibility element for the entire group; individual bars are decorative.
struct MobileLoadingPlaceholder: View {
    enum Style: Equatable { case rows, recordings, camera }
    let label: String
    var style: Style = .rows
    var rows = 3
    @Environment(\.accessibilityReduceMotion) private var reduceMotion
    @Environment(\.scenePhase) private var phase
    @ScaledMetric(relativeTo: .body) private var rowHeight = 56.0

    var body: some View {
        VStack(alignment: .leading, spacing: 12) {
            Group {
                if reduceMotion || phase != .active { shapes }
                else {
                    shapes.phaseAnimator([false, true]) { content, dimmed in
                        content.opacity(dimmed ? 0.55 : 1)
                    } animation: { _ in .easeInOut(duration: 1.3) }
                }
            }.accessibilityHidden(true)
            Text(label).font(.footnote).foregroundStyle(.secondary)
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .accessibilityElement(children: .ignore)
        .accessibilityLabel(label)
        .accessibilityIdentifier("loading.placeholder")
        .allowsHitTesting(false)
    }

    private var shapes: some View {
        VStack(alignment: .leading, spacing: 14) {
            if style == .camera {
                RoundedRectangle(cornerRadius: 18).fill(.secondary.opacity(0.10))
                    .aspectRatio(4.0 / 3.0, contentMode: .fit)
            } else {
                ForEach(0..<min(max(rows, 1), 4), id: \.self) { index in
                    HStack(spacing: 12) {
                        RoundedRectangle(cornerRadius: 12).fill(.secondary.opacity(0.12))
                            .frame(width: style == .recordings ? 70 : 42, height: style == .recordings ? 48 : 42)
                        VStack(alignment: .leading, spacing: 9) {
                            bar(width: index.isMultiple(of: 2) ? 0.72 : 0.56, height: 13)
                            bar(width: 0.42, height: 9)
                        }
                    }.frame(minHeight: rowHeight)
                }
            }
        }
    }
    private func bar(width: CGFloat, height: CGFloat) -> some View {
        GeometryReader { geometry in
            Capsule().fill(.secondary.opacity(0.12)).frame(width: geometry.size.width * width)
        }.frame(height: height)
    }
}
