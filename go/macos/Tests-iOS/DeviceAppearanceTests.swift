import XCTest
import UIKit
@testable import WendyMobile

final class DeviceAppearanceTests: XCTestCase {
    private func device(_ id: String, transport: DeviceTransport = .cloud) -> Device {
        Device(id: id, name: "Same name", selector: id, transport: transport, address: "")
    }
    @MainActor func testIdentitySharesEnrolledRoutesButSeparatesAccountsAndOrganizations() {
        let cloud = device("cloud://api.wendy.sh/tenant/tenant-a/asset/device-a")
        let lan = device("spiffe://wendy.sh/tenant/tenant-a/device/device-a", transport: .lan)
        let key = DeviceAppearanceStore.key(account: "user-a", organization: "org-a", device: cloud)
        XCTAssertEqual(key, DeviceAppearanceStore.key(account: "user-a", organization: "org-a", device: lan))
        XCTAssertNotEqual(key, DeviceAppearanceStore.key(account: "user-b", organization: "org-a", device: cloud))
        XCTAssertNotEqual(key, DeviceAppearanceStore.key(account: "user-a", organization: "org-b", device: cloud))
        XCTAssertNotEqual(key, DeviceAppearanceStore.key(account: "user-a", organization: "org-a", device: device("cloud://api.wendy.sh/tenant/tenant-a/asset/device-b")))
        XCTAssertNotEqual(DeviceAppearanceStore.key(account: "a|b", organization: "c", device: cloud), DeviceAppearanceStore.key(account: "a", organization: "b|c", device: cloud))
    }
    @MainActor func testSaveReloadResetAndOversizePreserveExistingAppearance() throws {
        let directory = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
        defer { try? FileManager.default.removeItem(at: directory) }
        let key = DeviceAppearanceStore.key(account: "a", organization: "o", device: device("device-1"))
        let store = DeviceAppearanceStore(directory: directory)
        let value = DeviceAppearance(kind: .emoji, emoji: "🤖", color: "blue")
        try store.save(value, for: key)
        XCTAssertEqual(DeviceAppearanceStore(directory: directory).appearance(for: key), value)
        XCTAssertThrowsError(try store.save(DeviceAppearance(kind: .photo, photo: Data(repeating: 0, count: 600_001)), for: key))
        XCTAssertEqual(store.appearance(for: key), value)
        try store.save(nil, for: key)
        XCTAssertNil(store.appearance(for: key))
        XCTAssertNil(DeviceAppearanceStore(directory: directory).appearance(for: key))
        XCTAssertThrowsError(try store.save(value, for: "../invalid"))
    }
    @MainActor func testPhotoImportAndCropBoundPixelsAndClampBlankEdges() throws {
        let format = UIGraphicsImageRendererFormat(); format.scale = 1
        let image = UIGraphicsImageRenderer(size: CGSize(width: 2400, height: 1200), format: format).image { context in
            UIColor.red.setFill(); context.fill(CGRect(x: 0, y: 0, width: 2400, height: 1200))
        }
        let url = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString + ".jpg")
        defer { try? FileManager.default.removeItem(at: url) }
        try XCTUnwrap(image.jpegData(compressionQuality: 0.9)).write(to: url)
        let imported = try DeviceAppearancePhoto.downsample(url)
        let small = try XCTUnwrap(UIImage(data: imported))
        XCTAssertLessThanOrEqual(max(small.size.width, small.size.height), 1024)
        let offset = DeviceAppearanceCrop.clamped(CGSize(width: 10000, height: -10000), imageSize: small.size, side: 280, zoom: 1)
        XCTAssertEqual(offset.width, 140, accuracy: 1)
        XCTAssertEqual(offset.height, 0)
        let data = try XCTUnwrap(DeviceAppearanceCrop.render(image: small, zoom: 1, offset: offset, side: 280))
        let result = try XCTUnwrap(UIImage(data: data))
        XCTAssertEqual(result.size, CGSize(width: 512, height: 512))
        XCTAssertLessThan(data.count, 600_000)
        try Data("not an image".utf8).write(to: url)
        XCTAssertThrowsError(try DeviceAppearancePhoto.downsample(url))
    }
}
