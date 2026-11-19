import Foundation
import JPEG
import Testing

@testable import HelloVLM

private struct DataSource: JPEG.Bytestream.Source {
    let bytes: [UInt8]
    var position = 0

    init(_ data: Data) { bytes = [UInt8](data) }

    mutating func read(count: Int) -> [UInt8]? {
        guard position + count <= bytes.count else { return nil }
        defer { position += count }
        return Array(bytes[position..<position + count])
    }
}

@Suite struct YUYVJPEGTests {
    @Test func yuyvEncodesToAJPEGOfTheSameSizeAndColours() throws {
        let width = 32
        let height = 16
        var yuyv = [UInt8]()
        for _ in 0..<height {
            for x in stride(from: 0, to: width, by: 2) {
                if x < width / 2 {
                    yuyv += [81, 90, 81, 240]  // red
                } else {
                    yuyv += [145, 54, 145, 34]  // green
                }
            }
        }

        let jpeg = try yuyv.withUnsafeBytes {
            try jpegFromYUYV($0, width: width, height: height, bytesPerLine: width * 2)
        }

        var source = DataSource(jpeg)
        let image = try JPEG.Data.Rectangular<JPEG.Common>.decompress(stream: &source)
        #expect(image.size.x == width)
        #expect(image.size.y == height)
        let pixels = image.unpack(as: JPEG.RGB.self)
        let red = pixels[8 * width + 4]
        let green = pixels[8 * width + 28]
        #expect(Int(red.r) > Int(red.g) + 60)
        #expect(Int(green.g) > Int(green.r) + 60)
    }

    @Test func nodesSortByNumber() {
        #expect(
            videoNodesInNumberOrder(["video254", "video0", "video10", "video2"])
                == ["video0", "video2", "video10", "video254"]
        )
    }

    @Test func shortFrameThrowsInsteadOfCrashing() {
        let short = [UInt8](repeating: 0, count: 10)
        #expect(throws: YUYVJPEGError.self) {
            try short.withUnsafeBytes {
                try jpegFromYUYV($0, width: 32, height: 16, bytesPerLine: 64)
            }
        }
    }

    @Test func rowPaddingIsSkipped() throws {
        let width = 8
        let height = 4
        let bytesPerLine = width * 2 + 8
        var yuyv = [UInt8](repeating: 0, count: bytesPerLine * height)
        for y in 0..<height {
            for x in stride(from: 0, to: width, by: 2) {
                let i = y * bytesPerLine + x * 2
                yuyv[i] = 81
                yuyv[i + 1] = 90
                yuyv[i + 2] = 81
                yuyv[i + 3] = 240
            }
        }
        let jpeg = try yuyv.withUnsafeBytes {
            try jpegFromYUYV($0, width: width, height: height, bytesPerLine: bytesPerLine)
        }
        var source = DataSource(jpeg)
        let image = try JPEG.Data.Rectangular<JPEG.Common>.decompress(stream: &source)
        let p = image.unpack(as: JPEG.RGB.self)[2 * width + 6]
        #expect(Int(p.r) > Int(p.g) + 60)
    }
}
