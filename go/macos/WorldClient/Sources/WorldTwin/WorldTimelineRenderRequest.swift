import Foundation
import SwiftUI

public struct WorldTimelineRenderRequest {
    let timeline: URL
    let output: URL
    let duration: Double
    let fps: Int

    public static func recognizes(_ arguments: [String]) -> Bool {
        arguments.contains { $0.hasPrefix("--world-render-") || $0 == "--world-timeline" }
    }

    public static func parse(_ arguments: [String]) throws -> Self? {
        guard recognizes(arguments) else { return nil }
        guard !WorldTwinLaunchRequest.recognizes(arguments) else {
            throw WorldTwinLaunchRequest.InvalidArguments.conflictingModes
        }
        func value(_ flag: String) throws -> String {
            guard let index = arguments.firstIndex(of: flag), index + 1 < arguments.count,
                  arguments.filter({ $0 == flag }).count == 1 else {
                throw PromoFailure(message: "Missing or repeated render argument: \(flag)")
            }
            return arguments[index + 1]
        }
        let input = try value("--world-timeline"), output = try value("--world-render-output")
        guard input.hasPrefix("/"), output.hasPrefix("/"),
              let duration = Double(try value("--world-render-duration")), duration.isFinite,
              duration > 0, duration <= 60,
              let fps = Int(try value("--world-render-fps")), (1...24).contains(fps) else {
            throw PromoFailure(message: "Render requires absolute paths, a duration in (0, 60] and 1–24 fps.")
        }
        let source = URL(fileURLWithPath: input).resolvingSymlinksInPath()
        let destination = URL(fileURLWithPath: output).resolvingSymlinksInPath()
        guard source != destination, !destination.path.hasPrefix(source.path + "/") else {
            throw PromoFailure(message: "Render output must be outside the immutable timeline.")
        }
        return Self(timeline: source, output: destination, duration: duration, fps: fps)
    }
}

public struct WorldTimelineRenderView: View {
    let request: WorldTimelineRenderRequest
    @State private var status = "Rendering recorded simulation…"
    public init(request: WorldTimelineRenderRequest) { self.request = request }
    public var body: some View {
        VStack(alignment: .leading, spacing: 16) {
            Label("Simulation replay export", systemImage: "film").font(.title)
            Text(status)
            Text("Recorded states render through the native World scene and interface. Robot controls are disabled.")
                .foregroundStyle(.secondary)
            Text(request.output.path).font(.caption).textSelection(.enabled)
        }.padding(32).frame(maxWidth: .infinity, maxHeight: .infinity, alignment: .topLeading)
            .task {
                do {
                    try await WorldTimelineRenderer.render(request)
                    status = "Export complete. Frames and render.json are saved."
                } catch is CancellationError {
                    status = "Export cancelled. No completion receipt was written."
                } catch {
                    status = "Export failed: \(error)"
                }
            }
    }
}
