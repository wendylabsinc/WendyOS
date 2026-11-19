import Foundation
import JPEG

struct YUYVJPEGError: Error, CustomStringConvertible {
    let description: String
}

/// Collects the bytes swift-jpeg writes.
private struct DataDestination: JPEG.Bytestream.Destination {
    var data = Data()

    mutating func write(_ bytes: [UInt8]) -> Void? {
        data.append(contentsOf: bytes)
        return ()
    }
}

/// BT.601 YUV to RGB, the same conversion HelloVideo uses.
private func rgb(y: UInt8, u: UInt8, v: UInt8) -> JPEG.RGB {
    let y1 = Int32(y)
    let u1 = Int32(u) - 128
    let v1 = Int32(v) - 128
    let r = y1 + (359 * v1) / 256
    let g = y1 - (88 * u1) / 256 - (183 * v1) / 256
    let b = y1 + (454 * u1) / 256
    func clamp(_ value: Int32) -> UInt8 { UInt8(max(0, min(255, value))) }
    return JPEG.RGB(clamp(r), clamp(g), clamp(b))
}

/// Encodes one YUYV (YUY2) frame as a baseline JPEG, using HelloVideo's
/// `captureRGBFrame` layout (4:2:0 subsampling, best-quality quanta).
///
/// A shared camera delivers YUYV even when the app asked for MJPG; the VLM
/// backend wants JPEG, so the app encodes the frames it samples.
func jpegFromYUYV(
    _ yuyv: UnsafeRawBufferPointer,
    width: Int,
    height: Int,
    bytesPerLine: Int
) throws -> Data {
    guard width > 0, height > 0, width % 2 == 0 else {
        throw YUYVJPEGError(description: "YUYV frame needs a positive, even width (got \(width)x\(height)).")
    }
    guard bytesPerLine >= width * 2 else {
        throw YUYVJPEGError(description: "YUYV bytes per line \(bytesPerLine) is less than \(width * 2).")
    }
    guard yuyv.count >= bytesPerLine * (height - 1) + width * 2 else {
        throw YUYVJPEGError(
            description: "YUYV frame is \(yuyv.count) bytes; \(width)x\(height) needs more."
        )
    }

    var pixels = [JPEG.RGB](repeating: JPEG.RGB(0, 0, 0), count: width * height)
    for row in 0..<height {
        let rowStart = row * bytesPerLine
        for x in stride(from: 0, to: width, by: 2) {
            let i = rowStart + x * 2
            let u = yuyv[i + 1]
            let v = yuyv[i + 3]
            pixels[row * width + x] = rgb(y: yuyv[i], u: u, v: v)
            pixels[row * width + x + 1] = rgb(y: yuyv[i + 2], u: u, v: v)
        }
    }

    let layout = JPEG.Layout<JPEG.Common>(
        format: .ycc8,
        process: .baseline,
        components: [
            1: (factor: (2, 2), qi: 0),
            2: (factor: (1, 1), qi: 1),
            3: (factor: (1, 1), qi: 1),
        ],
        scans: [
            .sequential((1, \.0, \.0), (2, \.1, \.1), (3, \.1, \.1))
        ]
    )
    let jfif = JPEG.JFIF(version: .v1_2, density: (x: 10, y: 10, unit: .centimeters))
    let image = JPEG.Data.Rectangular<JPEG.Common>.pack(
        size: (x: width, y: height),
        layout: layout,
        metadata: [.jfif(jfif)],
        pixels: pixels
    )

    var destination = DataDestination()
    try image.compress(
        stream: &destination,
        quanta: [
            0: JPEG.CompressionLevel.luminance(0.0).quanta,
            1: JPEG.CompressionLevel.chrominance(0.0).quanta,
        ]
    )
    return destination.data
}
