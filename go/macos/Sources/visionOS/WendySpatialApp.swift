import SwiftUI

@main
struct WendySpatialApp: App {
    @State private var model = SpatialModel()
    init() {
        SpatialFloorTarget.registerComponent()
        SpatialPinTarget.registerComponent()
    }
    var body: some Scene {
        WindowGroup(id: "workspace") {
            SpatialWorkspace(model: model)
        }
        // Device and AR sessions are recreated on launch. Present a fresh
        // control window instead of restoring a hidden session's layout.
        .restorationBehavior(.disabled)
        .defaultLaunchBehavior(.presented)
        .defaultSize(width: 1120, height: 820)
        WindowGroup(id: "assistant") {
            SpatialAssistantView(model: model, assistant: model.assistant)
        }
        .restorationBehavior(.disabled)
        .defaultLaunchBehavior(.suppressed)
        .defaultSize(width: 520, height: 720)
        .defaultWindowPlacement { _, context in
            if let main = context.windows.first(where: { $0.id == "workspace" }) { return WindowPlacement(.trailing(main)) }
            return WindowPlacement()
        }
        WindowGroup(id: "tabletop") {
            SpatialTabletop(model: model)
        }
        .restorationBehavior(.disabled)
        .defaultLaunchBehavior(.suppressed)
        .windowStyle(.volumetric)
        .defaultSize(width: 1.2, height: 0.8, depth: 1.2, in: .meters)
        ImmersiveSpace(id: "overlays") {
            SpatialOverlayView(model: model)
        }
        .immersionStyle(selection: .constant(.mixed), in: .mixed)
    }
}
