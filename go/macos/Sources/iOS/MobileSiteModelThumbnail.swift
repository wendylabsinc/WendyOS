import SwiftUI
import QuickLookThumbnailing
@preconcurrency import SceneKit

/// Quick Look renders the actual USDZ; renaming does not invalidate geometry previews.
struct MobileSiteModelThumbnail: View {
    let store: MobileSitesStore
    let modelID: UUID
    @State private var image: UIImage?
    @State private var loading = true
    @Environment(\.displayScale) private var scale
    private static let cache: NSCache<NSString, UIImage> = {
        let cache = NSCache<NSString, UIImage>()
        cache.totalCostLimit = 24 * 1024 * 1024
        return cache
    }()
    var body: some View {
        ZStack {
            RoundedRectangle(cornerRadius: 12).fill(.quaternary)
            if let image {
                Image(uiImage: image).resizable().scaledToFit().padding(4)
            } else if loading {
                ProgressView()
            } else {
                Image(systemName: "cube.transparent").font(.title2).foregroundStyle(.secondary)
            }
        }.frame(width: 88, height: 72).clipShape(RoundedRectangle(cornerRadius: 12))
            .accessibilityLabel(image == nil ? "3D model preview unavailable" : "3D model preview")
            .task(id: modelID) {
                loading = true
                defer { loading = false }
                do {
                    let saved = try await store.models.load(id: modelID)
                    let key = (saved.modelURL.path + saved.site.modelRevision.uuidString) as NSString
                    if let cached = Self.cache.object(forKey: key) { image = cached; return }
                    let rendered = try await Self.render(url: saved.modelURL, scale: scale)
                    try Task.checkCancellation()
                    image = rendered
                    Self.cache.setObject(rendered, forKey: key, cost: (rendered.cgImage?.bytesPerRow ?? 0) * (rendered.cgImage?.height ?? 0))
                } catch { image = nil }
            }
    }
    static func render(url: URL, scale: CGFloat) async throws -> UIImage {
        let request = QLThumbnailGenerator.Request(fileAt: url, size: CGSize(width: 176, height: 144), scale: scale, representationTypes: .thumbnail)
        do { return try await QLThumbnailGenerator.shared.generateBestRepresentation(for: request).uiImage }
        catch {
            try Task.checkCancellation()
            // Quick Look's USDZ extension is not available on every runtime.
            // Render the same geometry as the model viewer off the main thread.
            let bytes: Data = try await BlockingWork.run(on: .global(qos: .utility)) {
                try autoreleasepool {
                    let scene = try SCNScene(url: url, options: nil)
                    let (minimum, maximum) = scene.rootNode.boundingBox
                    let center = SCNVector3((minimum.x + maximum.x)/2, (minimum.y + maximum.y)/2, (minimum.z + maximum.z)/2)
                    let radius = max(0.1, sqrt(pow(maximum.x-minimum.x, 2) + pow(maximum.y-minimum.y, 2) + pow(maximum.z-minimum.z, 2))/2)
                    let camera = SCNNode()
                    camera.camera = SCNCamera()
                    camera.camera?.zNear = Double(radius)/1000
                    camera.camera?.zFar = Double(radius)*100
                    camera.position = SCNVector3(center.x + radius*2, center.y + radius*1.8, center.z + radius*2)
                    camera.look(at: center)
                    scene.rootNode.addChildNode(camera)
                    let renderer = SCNRenderer(device: nil, options: nil)
                    renderer.scene = scene; renderer.pointOfView = camera; renderer.autoenablesDefaultLighting = true
                    let snapshot = renderer.snapshot(atTime: 0, with: CGSize(width: 176*scale, height: 144*scale), antialiasingMode: .multisampling4X)
                    guard let bytes = snapshot.pngData() else { throw SiteError.invalidSite }
                    return bytes
                }
            }
            guard let image = UIImage(data: bytes) else { throw SiteError.invalidSite }
            return image
        }
    }

}

struct MobileRenameSiteModel: View {
    @ObservedObject var store: MobileSitesStore
    let siteID: UUID
    let link: MobileSiteModelLink
    @Environment(\.dismiss) private var dismiss
    @State private var name: String
    @State private var busy = false
    @State private var error: String?
    @State private var actions = AsyncActions()
    @FocusState private var focused: Bool
    init(store: MobileSitesStore, siteID: UUID, link: MobileSiteModelLink) {
        self.store = store; self.siteID = siteID; self.link = link
        _name = State(initialValue: link.name)
    }
    var body: some View {
        NavigationStack {
            Form {
                Section("Model name") {
                    TextField("Name", text: $name).focused($focused).accessibilityIdentifier("sites.modelName")
                }
                if let error { Text(error).foregroundStyle(.red) }
                if busy { ProgressView("Saving…") }
            }.scrollContentBackground(.hidden).background(MobileSitesStyle.background)
                .navigationTitle("Rename model").navigationBarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() }.disabled(busy) }
                    ToolbarItem(placement: .confirmationAction) {
                        Button("Save") {
                            busy = true; error = nil
                            actions.submit {
                                defer { busy = false }
                                do { try await store.renameModel(link.id, siteID: siteID, name: name); dismiss() }
                                catch { self.error = error.localizedDescription }
                            }
                        }.disabled(busy || name.trimmingCharacters(in: .whitespacesAndNewlines).isEmpty || name.trimmingCharacters(in: .whitespacesAndNewlines).count > 200)
                    }
                }
        }.interactiveDismissDisabled(busy).tint(MobileLensStyle.green)
            .task { await actions.run() }
            .onAppear { focused = true }
    }
}
