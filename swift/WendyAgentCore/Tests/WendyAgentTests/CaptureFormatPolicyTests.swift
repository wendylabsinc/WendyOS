import Testing

@testable import WendyAgentCore

@Suite("Capture format policy")
struct CaptureFormatPolicyTests {
    /// The TOPDON TC001's formats exactly as AVFoundation advertised them on
    /// macOS 26.2 (all `'yuvs'`), in advertised order. AVFoundation's own pick
    /// for this list was 16x15505, which aborted the agent in Portrait/Metal.
    static let topdonTC001: [CaptureFormatCandidate] = [
        (4, 12305), (4, 12621), (8, 12578), (16, 15505), (60, 3299),
        (256, 192), (256, 196), (256, 392), (512, 384), (512, 484), (520, 192), (644, 384),
    ].map { CaptureFormatCandidate(width: $0.0, height: $0.1, maxFrameRate: 25) }

    @Test
    func `pins the TC001's 512x384 picture instead of a control mode`() {
        let choice = CaptureFormatPolicy.choose(from: Self.topdonTC001)
        guard case .pin(let index) = choice else {
            Issue.record("Expected a pinned format, got \(choice)")
            return
        }
        #expect(Self.topdonTC001[index].width == 512)
        #expect(Self.topdonTC001[index].height == 384)
    }

    @Test
    func `leaves an ordinary webcam to the session preset`() {
        let webcam = [(640, 480), (1280, 720), (1920, 1080)].map {
            CaptureFormatCandidate(width: $0.0, height: $0.1, maxFrameRate: 30)
        }
        #expect(CaptureFormatPolicy.choose(from: webcam) == .sessionDefault)
    }

    @Test
    func `refuses a camera that offers only vendor modes`() {
        let vendorOnly = Array(Self.topdonTC001.prefix(5))
        #expect(CaptureFormatPolicy.choose(from: vendorOnly) == .unusable)
        #expect(CaptureFormatPolicy.choose(from: []) == .unusable)
    }

    @Test(arguments: [
        (64, 64, true), (63, 480, false), (8192, 4320, true), (8193, 4320, false),
        (1024, 256, true), (1025, 256, false), (16, 15505, false), (60, 3299, false),
        (520, 192, true), (384, 512, true),
    ])
    func `recognises picture shapes`(width: Int32, height: Int32, isPicture: Bool) {
        #expect(CaptureFormatPolicy.isPicture(width: width, height: height) == isPicture)
    }

    @Test
    func `prefers the largest odd shape when no standard one exists`() {
        let odd = [(4, 12305), (520, 192), (644, 384), (512, 484)].map {
            CaptureFormatCandidate(width: $0.0, height: $0.1, maxFrameRate: 25)
        }
        #expect(CaptureFormatPolicy.choose(from: odd) == .pin(index: 3))
    }

    @Test
    func `breaks a size tie on frame rate, then advertised order`() {
        let formats = [
            CaptureFormatCandidate(width: 16, height: 15505, maxFrameRate: 25),
            CaptureFormatCandidate(width: 640, height: 480, maxFrameRate: 15),
            CaptureFormatCandidate(width: 640, height: 480, maxFrameRate: 30),
            CaptureFormatCandidate(width: 640, height: 480, maxFrameRate: 30),
        ]
        #expect(CaptureFormatPolicy.choose(from: formats) == .pin(index: 2))
    }

    @Test
    func `summarises distinct shapes for the refusal message`() {
        let formats = [(4, 12305), (16, 15505), (4, 12305)].map {
            CaptureFormatCandidate(width: $0.0, height: $0.1, maxFrameRate: 25)
        }
        #expect(CaptureFormatPolicy.summary(of: formats) == "4x12305, 16x15505")
        #expect(
            CameraError.noStreamableFormat("4x12305").description.contains("4x12305")
        )
    }
}
