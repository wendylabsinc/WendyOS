import Foundation
import CoreGraphics

/// Shared decoded image storage, independent of the scene renderer.
public struct SiteCameraColorFrame: Sendable {
    let image: CGImage
    let received: Date
    let sequence: UInt64
}
