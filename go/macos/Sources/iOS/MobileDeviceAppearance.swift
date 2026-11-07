import SwiftUI
import PhotosUI
import CryptoKit
import ImageIO
import UniformTypeIdentifiers

struct DeviceAppearance: Codable, Equatable {
    enum Kind: String, Codable { case symbol, emoji, photo }
    var kind: Kind = .symbol
    var symbol = "cpu"
    var emoji = "🤖"
    var color = "green"
    var photo: Data?
    static let symbols = ["cpu", "desktopcomputer", "server.rack", "sensor", "camera.fill", "video.fill", "car.fill", "airplane", "bolt.fill", "leaf.fill", "house.fill", "star.fill"]
    static let emojis = ["🤖", "🦾", "🚀", "🛰️", "🛸", "🐶", "🐱", "🦊", "🐙", "🐝", "🌱", "⚡️", "🔬", "🎥", "🏠", "⭐️"]
    static let colors = ["green", "blue", "purple", "pink", "orange", "slate"]
    var tint: Color {
        switch color {
        case "blue": .blue
        case "purple": .purple
        case "pink": .pink
        case "orange": .orange
        case "slate": Color(red: 0.32, green: 0.38, blue: 0.43)
        default: MobileLensStyle.green
        }
    }
}

@MainActor final class DeviceAppearanceStore: ObservableObject {
    @Published private(set) var revision = 0
    private let directory: URL
    private var values: [String: DeviceAppearance] = [:]
    private var loaded: Set<String> = []
    init(directory: URL? = nil) {
        self.directory = directory ?? FileManager.default.urls(for: .applicationSupportDirectory, in: .userDomainMask)[0].appendingPathComponent("DeviceAppearance", isDirectory: true)
    }
    static func key(account: String, organization: String, device: Device) -> String {
        // Length-prefix identity components; titles and IP addresses are not identities.
        let components = [account, organization, recordingDeviceID(device)]
        let value = components.map { "\($0.utf8.count):\($0)" }.joined()
        return SHA256.hash(data: Data(value.utf8)).map { String(format: "%02x", $0) }.joined()
    }
    func appearance(for key: String) -> DeviceAppearance? {
        if loaded.insert(key).inserted {
            let url = directory.appendingPathComponent(key + ".json")
            if let size = try? url.resourceValues(forKeys: [.fileSizeKey]).fileSize, size <= 1_000_000,
               let data = try? Data(contentsOf: url), let value = try? JSONDecoder().decode(DeviceAppearance.self, from: data) {
                values[key] = value
            }
        }
        return values[key]
    }
    func save(_ value: DeviceAppearance?, for key: String) throws {
        guard key.count == 64, key.allSatisfy({ $0.isHexDigit }) else { throw WendyError(message: "Invalid device appearance identity.") }
        let url = directory.appendingPathComponent(key + ".json")
        if let value {
            guard (value.photo?.count ?? 0) <= 600_000 else { throw WendyError(message: "This image is too large. Choose another photo.") }
            let data = try JSONEncoder().encode(value)
            try FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
            try data.write(to: url, options: [.atomic, .completeFileProtectionUntilFirstUserAuthentication])
        } else if FileManager.default.fileExists(atPath: url.path) { try FileManager.default.removeItem(at: url) }
        values[key] = value; loaded.insert(key); revision += 1
    }
}

struct MobileDeviceAvatar: View {
    @EnvironmentObject private var model: AppModel
    @EnvironmentObject private var appearances: DeviceAppearanceStore
    let device: Device
    var size: CGFloat = 46
    var body: some View {
        DeviceAppearancePreview(appearance: appearances.appearance(for: DeviceAppearanceStore.key(account: model.mobileCloudAccount, organization: model.mobileCloudOrganization, device: device)), lite: device.lite, size: size)
    }
}

private struct DeviceAppearancePreview: View {
    let appearance: DeviceAppearance?
    var lite = false
    var size: CGFloat = 96
    var body: some View {
        Group {
            if let value = appearance {
                ZStack {
                    Rectangle().fill(value.tint.gradient)
                    if value.kind == .photo, let data = value.photo, let image = UIImage(data: data) {
                        Image(uiImage: image).resizable().scaledToFill()
                    } else if value.kind == .emoji {
                        Text(value.emoji).font(.system(size: size * 0.53))
                    } else {
                        Image(systemName: value.symbol).font(.system(size: size * 0.44, weight: .light)).foregroundStyle(.white)
                    }
                }.frame(width: size, height: size).clipShape(.rect(cornerRadius: size * 0.28))
                    .overlay { RoundedRectangle(cornerRadius: size * 0.22).strokeBorder(.white.opacity(0.22), lineWidth: 1).padding(size * 0.08) }
            } else { DeviceEmblem(lite: lite, size: size).accentColor(MobileLensStyle.green) }
        }.accessibilityHidden(true)
    }
}

struct DeviceAppearancePhoto: Transferable, Sendable {
    let data: Data
    static var transferRepresentation: some TransferRepresentation {
        FileRepresentation(importedContentType: .image) { received in
            DeviceAppearancePhoto(data: try downsample(received.file))
        }
    }
    static func downsample(_ url: URL) throws -> Data {
        guard let size = try url.resourceValues(forKeys: [.fileSizeKey]).fileSize, size <= 50_000_000 else {
            throw WendyError(message: "Choose an image smaller than 50 MB.")
        }
        guard let source = CGImageSourceCreateWithURL(url as CFURL, [kCGImageSourceShouldCache: false] as CFDictionary),
              let image = CGImageSourceCreateThumbnailAtIndex(source, 0, [
                kCGImageSourceCreateThumbnailFromImageAlways: true,
                kCGImageSourceCreateThumbnailWithTransform: true,
                kCGImageSourceThumbnailMaxPixelSize: 1024
              ] as CFDictionary) else { throw WendyError(message: "This photo could not be opened. Choose another image.") }
        let result = NSMutableData()
        guard let destination = CGImageDestinationCreateWithData(result, UTType.jpeg.identifier as CFString, 1, nil) else {
            throw WendyError(message: "This photo could not be prepared.")
        }
        CGImageDestinationAddImage(destination, image, [kCGImageDestinationLossyCompressionQuality: 0.85] as CFDictionary)
        guard CGImageDestinationFinalize(destination) else { throw WendyError(message: "This photo could not be prepared.") }
        return result as Data
    }
}

struct MobileDeviceAppearanceEditor: View {
    @Environment(\.dismiss) private var dismiss
    @EnvironmentObject private var store: DeviceAppearanceStore
    let device: Device
    let key: String
    @State private var draft = DeviceAppearance()
    @State private var isDefault = true
    @State private var selectedPhoto: PhotosPickerItem?
    @State private var crop: AppearanceCropSource?
    @State private var loading = false
    @State private var error: String?
    private let columns = Array(repeating: GridItem(.flexible()), count: 4)
    var body: some View {
        NavigationStack {
            Form {
                Section {
                    HStack { Spacer(); DeviceAppearancePreview(appearance: isDefault ? nil : draft, lite: device.lite); Spacer() }.padding(.vertical, 12)
                    Text(device.title).font(.headline).frame(maxWidth: .infinity).multilineTextAlignment(.center)
                }
                Section {
                    PhotosPicker(selection: $selectedPhoto, matching: .images) { Label("Choose photo", systemImage: "photo") }
                        .disabled(loading).accessibilityIdentifier("appearance.photo")
                    if loading { ProgressView("Preparing photo…") }
                } footer: { Text("Choose a photo, then move and zoom to crop it.") }
                Section("Emoji") {
                    LazyVGrid(columns: columns, spacing: 12) {
                        ForEach(DeviceAppearance.emojis, id: \.self) { emoji in
                            Button { draft.emoji = emoji; draft.kind = .emoji; draft.photo = nil; isDefault = false } label: {
                                Text(emoji).font(.largeTitle).frame(maxWidth: .infinity, minHeight: 48)
                                    .background(!isDefault && draft.kind == .emoji && draft.emoji == emoji ? draft.tint.opacity(0.2) : .clear, in: .rect(cornerRadius: 12))
                            }.buttonStyle(.plain).accessibilityLabel("Use " + emoji)
                                .accessibilityAddTraits(!isDefault && draft.kind == .emoji && draft.emoji == emoji ? .isSelected : [])
                        }
                    }
                }
                Section("Icon") {
                    LazyVGrid(columns: columns, spacing: 12) {
                        ForEach(DeviceAppearance.symbols, id: \.self) { symbol in
                            Button { draft.symbol = symbol; draft.kind = .symbol; draft.photo = nil; isDefault = false } label: {
                                Image(systemName: symbol).font(.title2).frame(maxWidth: .infinity, minHeight: 48)
                                    .background(!isDefault && draft.kind == .symbol && draft.symbol == symbol ? draft.tint.opacity(0.2) : .clear, in: .rect(cornerRadius: 12))
                            }.buttonStyle(.plain).accessibilityLabel("Use " + symbol)
                                .accessibilityAddTraits(!isDefault && draft.kind == .symbol && draft.symbol == symbol ? .isSelected : []).accessibilityIdentifier("appearance.symbol." + symbol)
                        }
                    }
                }
                if draft.kind != .photo || isDefault {
                    Section("Background color") {
                        LazyVGrid(columns: Array(repeating: GridItem(.flexible()), count: 6)) {
                            ForEach(DeviceAppearance.colors, id: \.self) { color in
                                Button { draft.color = color; isDefault = false } label: {
                                    Circle().fill(DeviceAppearance(color: color).tint).frame(width: 36, height: 36)
                                        .overlay { if draft.color == color { Image(systemName: "checkmark").foregroundStyle(.white) } }
                                        .frame(minHeight: 44)
                                }.buttonStyle(.plain).accessibilityLabel(color.capitalized)
                                    .accessibilityAddTraits(draft.color == color ? .isSelected : []).accessibilityIdentifier("appearance.color." + color)
                            }
                        }
                    }
                }
                Section {
                    Button("Reset to default", role: .destructive) { isDefault = true; draft = DeviceAppearance() }
                        .accessibilityIdentifier("appearance.reset")
                } footer: { Text("Personal to you on this iPhone. Other users’ device icons stay unchanged. Appearance does not sync between phones yet.") }
                if let error { Section { Text(error).foregroundStyle(.red) } }
            }.navigationTitle("Device appearance").navigationBarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                    ToolbarItem(placement: .confirmationAction) { Button("Save") { save() }.disabled(loading).accessibilityIdentifier("appearance.save") }
                }
                .onAppear { if let value = store.appearance(for: key) { draft = value; isDefault = false } }
                .task(id: selectedPhoto) {
                    guard let item = selectedPhoto else { return }
                    loading = true; error = nil
                    do {
                        guard let imported = try await item.loadTransferable(type: DeviceAppearancePhoto.self), let image = UIImage(data: imported.data) else {
                            throw WendyError(message: "This photo could not be opened.")
                        }
                        guard !Task.isCancelled else { return }
                        crop = AppearanceCropSource(image: image)
                    } catch { if !Task.isCancelled { self.error = (error as? WendyError)?.message ?? "Could not load photo. Try another image or download it from iCloud first." } }
                    if !Task.isCancelled { loading = false }
                }
                .sheet(item: $crop, onDismiss: { selectedPhoto = nil }) { source in
                    DeviceAppearanceCrop(image: source.image) { data in
                        draft.kind = .photo; draft.photo = data; isDefault = false; crop = nil
                    }
                }
        }.tint(MobileLensStyle.green)
    }
    private func save() {
        do { try store.save(isDefault ? nil : draft, for: key); dismiss() }
        catch { self.error = "Could not save this appearance. Please try again." }
    }
}

private struct AppearanceCropSource: Identifiable {
    let id = UUID()
    let image: UIImage
}

struct DeviceAppearanceCrop: View {
    @Environment(\.dismiss) private var dismiss
    let image: UIImage
    let save: (Data) -> Void
    @State private var zoom: CGFloat = 1
    @State private var offset = CGSize.zero
    @State private var dragStart = CGSize.zero
    @State private var error: String?
    private let side: CGFloat = 280
    private var scale: CGFloat { max(side / image.size.width, side / image.size.height) * zoom }
    static func clamped(_ offset: CGSize, imageSize: CGSize, side: CGFloat, zoom: CGFloat) -> CGSize {
        let scale = max(side / imageSize.width, side / imageSize.height) * zoom
        let x = max(0, (imageSize.width * scale - side) / 2)
        let y = max(0, (imageSize.height * scale - side) / 2)
        return CGSize(width: min(x, max(-x, offset.width)), height: min(y, max(-y, offset.height)))
    }
    static func render(image: UIImage, zoom: CGFloat, offset: CGSize, side: CGFloat) -> Data? {
        let output: CGFloat = 512
        let scale = max(side / image.size.width, side / image.size.height) * zoom
        let safe = clamped(offset, imageSize: image.size, side: side, zoom: zoom)
        let width = image.size.width * scale, height = image.size.height * scale
        let format = UIGraphicsImageRendererFormat(); format.scale = 1; format.opaque = true
        return UIGraphicsImageRenderer(size: CGSize(width: output, height: output), format: format).image { context in
            UIColor.white.setFill(); context.fill(CGRect(x: 0, y: 0, width: output, height: output))
            image.draw(in: CGRect(x: ((side - width) / 2 + safe.width) * output / side,
                                  y: ((side - height) / 2 + safe.height) * output / side,
                                  width: width * output / side, height: height * output / side))
        }.jpegData(compressionQuality: 0.85)
    }
    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(spacing: 24) {
                Image(uiImage: image).resizable().frame(width: image.size.width * scale, height: image.size.height * scale)
                    .offset(offset).frame(width: side, height: side).clipped()
                    .clipShape(.rect(cornerRadius: side * 0.28))
                    .contentShape(.rect)
                    .gesture(DragGesture().onChanged { gesture in
                        offset = Self.clamped(CGSize(width: dragStart.width + gesture.translation.width, height: dragStart.height + gesture.translation.height), imageSize: image.size, side: side, zoom: zoom)
                    }.onEnded { _ in dragStart = offset })
                Text("Drag to position your photo").foregroundStyle(.secondary)
                HStack { Image(systemName: "minus.magnifyingglass"); Slider(value: $zoom, in: 1...3).accessibilityLabel("Photo zoom"); Image(systemName: "plus.magnifyingglass") }.padding(.horizontal, 32)
                    .onChange(of: zoom) { _, _ in offset = Self.clamped(offset, imageSize: image.size, side: side, zoom: zoom); dragStart = offset }
                if let error { Text(error).foregroundStyle(.red) }
                }.padding(.vertical, 24)
            }.navigationTitle("Crop photo").navigationBarTitleDisplayMode(.inline)
                .toolbar {
                    ToolbarItem(placement: .cancellationAction) { Button("Cancel") { dismiss() } }
                    ToolbarItem(placement: .confirmationAction) { Button("Use photo") {
                        if let data = Self.render(image: image, zoom: zoom, offset: offset, side: side) { save(data) }
                        else { error = "Could not crop this photo." }
                    } }
                }
        }
    }
}
