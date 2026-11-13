import CoreMedia
import Testing

@testable import WendyAgentCore

@Suite("Camera capture format choice")
struct CaptureFormatChoiceTests {
    private func sizes(_ pairs: [(Int32, Int32)]) -> [CMVideoDimensions] {
        pairs.map { CMVideoDimensions(width: $0.0, height: $0.1) }
    }

    @Test
    func `pins the thermal picture instead of a vendor data channel`() {
        // As AVFoundation reports the TOPDON thermal camera (2bdf:0102).
        let formats = sizes([
            (4, 12305), (4, 12621), (8, 12578), (16, 15505), (60, 3299),
            (256, 192), (256, 196), (256, 392), (512, 384), (512, 484),
            (520, 192), (644, 384),
        ])
        #expect(captureFormatChoice(formats) == .pinned(8))
    }

    @Test
    func `leaves ordinary cameras to the session`() {
        // As AVFoundation reports an Arducam RGB camera (0c45:6366).
        let formats = sizes([
            (320, 240), (320, 240), (640, 480), (640, 480), (1280, 720),
            (1920, 1080), (640, 360), (640, 480), (1280, 720), (1920, 1080),
        ])
        #expect(captureFormatChoice(formats) == .automatic)
    }

    @Test
    func `falls back to the largest picture without a standard aspect ratio`() {
        let formats = sizes([(16, 15505), (352, 288), (520, 192)])
        #expect(captureFormatChoice(formats) == .pinned(1))
    }

    @Test
    func `refuses a camera that offers only data channels`() {
        #expect(captureFormatChoice(sizes([(4, 12305), (60, 3299)])) == .unusable)
        #expect(captureFormatChoice([]) == .automatic)
    }
}
