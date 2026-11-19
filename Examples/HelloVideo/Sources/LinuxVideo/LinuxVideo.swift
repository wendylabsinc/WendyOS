import CLinuxVideo
import Foundation
import JPEG

// V4L2 capability constants
// These must match the values from linux/videodev2.h
public let V4L2_CAP_VIDEO_CAPTURE: UInt32 = 0x0000_0001
public let V4L2_CAP_STREAMING: UInt32 = 0x0400_0000

// V4L2 buffer types
public let V4L2_BUF_TYPE_VIDEO_CAPTURE: UInt32 = 1

// V4L2 memory types
public let V4L2_MEMORY_MMAP: UInt32 = 1

// V4L2 pixel formats
public let V4L2_PIX_FMT_YUYV: UInt32 = 0x5659_5559  // 'YUYV'

// V4L2 ioctl command values
// // These are the evaluated values of the macros in videodev2.h
// public let VIDIOC_QUERYCAP: UInt = 0x8068_5600  // _IOR('V', 0, struct v4l2_capability)
// public let VIDIOC_S_FMT: UInt = 0xC0D0_5605  // _IOWR('V', 5, struct v4l2_format)
// public let VIDIOC_REQBUFS: UInt = 0xC014_5608  // _IOWR('V', 8, struct v4l2_requestbuffers)
// public let VIDIOC_QUERYBUF: UInt = 0x8068_5609  // _IOWR('V', 9, struct v4l2_buffer)
// public let VIDIOC_QBUF: UInt = 0x8068_560A  // _IOWR('V', 10, struct v4l2_buffer)
// public let VIDIOC_DQBUF: UInt = 0x8068_560B  // _IOWR('V', 11, struct v4l2_buffer)
// public let VIDIOC_STREAMON: UInt = 0x4004_5614  // _IOW('V', 20, int)
// public let VIDIOC_STREAMOFF: UInt = 0x4004_5615  // _IOW('V', 21, int)

/// One captured YUYV frame and the size the camera actually gave.
///
/// A shared camera ignores the requested size and gives its own, so the
/// buffer must be read with these values, not the ones asked for.
public struct CapturedFrame {
    public let data: Data
    public let width: UInt32
    public let height: UInt32
    public let bytesPerLine: UInt32
}

/// Orders video node names (`video0`, `video2`, `video10`) by their number,
/// so the camera's own path comes before the extra nodes next to it.
public func videoNodesInNumberOrder(_ names: [String]) -> [String] {
    func number(_ name: String) -> Int {
        Int(name.drop(while: { !$0.isNumber })) ?? Int.max
    }
    return names.sorted { (number($0), $0) < (number($1), $1) }
}

/// A structure representing a V4L2 video device
public struct VideoDevice {
    /// The device path (e.g., "/dev/video0")
    public let path: String

    /// The device name
    public let name: String

    /// The device driver name
    public let driver: String

    /// The device bus information
    public let busInfo: String

    /// The device capabilities
    public let capabilities: UInt32

    /// Whether the device supports video capture
    public var supportsCapture: Bool {
        return (capabilities & V4L2_CAP_VIDEO_CAPTURE) != 0
    }

    /// Whether the device supports streaming I/O
    public var supportsStreaming: Bool {
        return (capabilities & V4L2_CAP_STREAMING) != 0
    }

    /// Capture a single frame from the video device
    /// - Parameters:
    ///   - width: The desired frame width
    ///   - height: The desired frame height
    /// - Returns: The frame and the width, height and bytes per line the
    ///   camera gave, which can differ from the ones asked for
    public func captureFrame(width: UInt32 = 640, height: UInt32 = 480) throws -> CapturedFrame {
        try captureFrame(width: width, height: height, calls: SystemV4L2Calls())
    }

    /// `captureFrame` through `calls`, which tests replace with a fake driver.
    func captureFrame(width: UInt32, height: UInt32, calls: some V4L2Calls) throws -> CapturedFrame {
        let fd = try calls.openDevice(path)
        defer { calls.closeDevice(fd) }

        // Set up format
        var format = v4l2_format()
        format.type = V4L2_BUF_TYPE_VIDEO_CAPTURE
        format.fmt.pix.width = width
        format.fmt.pix.height = height
        format.fmt.pix.pixelformat = V4L2_PIX_FMT_YUYV
        format.fmt.pix.field = V4L2_FIELD_NONE.rawValue
        format.fmt.pix.bytesperline = width * 2
        format.fmt.pix.sizeimage = width * height * 2
        // format.fmt.pix.colorspace = V4L2_COLORSPACE_SRGB.rawValue

        try calls.control(fd, WENDY_VIDIOC_S_FMT, "VIDIOC_S_FMT", &format)

        guard format.fmt.pix.pixelformat == V4L2_PIX_FMT_YUYV else {
            throw VideoError.unsupportedOperation(
                message: "Camera gave pixel format 0x\(String(format.fmt.pix.pixelformat, radix: 16)), not YUYV"
            )
        }
        // The driver may change the size; use what it gave.
        let width = format.fmt.pix.width
        let height = format.fmt.pix.height
        let bytesPerLine =
            format.fmt.pix.bytesperline >= width * 2 ? format.fmt.pix.bytesperline : width * 2
        let frameSize = max(format.fmt.pix.sizeimage, bytesPerLine * height)

        // Request buffers. The driver decides how many: v4l2loopback (a shared
        // camera's node) gives a reader as many as its writer uses, whatever is
        // asked for.
        var reqbuf = v4l2_requestbuffers()
        reqbuf.count = 2
        reqbuf.memory = V4L2_MEMORY_MMAP
        reqbuf.type = V4L2_BUF_TYPE_VIDEO_CAPTURE
        try calls.control(fd, WENDY_VIDIOC_REQBUFS, "VIDIOC_REQBUFS", &reqbuf)
        guard reqbuf.count > 0 else {
            throw VideoError.unsupportedOperation(message: "VIDIOC_REQBUFS on \(path) gave no buffers")
        }

        // Map every buffer at the length and offset VIDIOC_QUERYBUF gives for
        // it. They must not be read from a struct passed to VIDIOC_QBUF: a driver
        // need not fill those fields in. uvcvideo does, but v4l2loopback leaves
        // them 0, and mmap of 0 bytes fails with EINVAL.
        var mapped: [(address: UnsafeMutableRawPointer, length: Int)] = []
        defer {
            for buffer in mapped {
                calls.unmap(buffer.address, length: buffer.length)
            }
        }
        for index in 0..<reqbuf.count {
            var query = v4l2_buffer()
            query.type = V4L2_BUF_TYPE_VIDEO_CAPTURE
            query.memory = V4L2_MEMORY_MMAP
            query.index = index
            try calls.control(fd, WENDY_VIDIOC_QUERYBUF, "VIDIOC_QUERYBUF", &query)
            let length = Int(query.length)
            let address = try calls.map(fd, index: index, length: length, offset: Int(query.m.offset))
            mapped.append((address, length))
        }

        // Queue every buffer: the driver may fill and hand back any of them.
        for index in 0..<reqbuf.count {
            var queue = v4l2_buffer()
            queue.type = V4L2_BUF_TYPE_VIDEO_CAPTURE
            queue.memory = V4L2_MEMORY_MMAP
            queue.index = index
            try calls.control(fd, WENDY_VIDIOC_QBUF, "VIDIOC_QBUF", &queue)
        }

        var type = V4L2_BUF_TYPE_VIDEO_CAPTURE
        let streamStart = calls.monotonicNanoseconds()
        try calls.control(fd, WENDY_VIDIOC_STREAMON, "VIDIOC_STREAMON", &type)
        // The frame is copied out before this runs, so a failure here costs
        // nothing worth reporting.
        defer { _ = try? calls.control(fd, WENDY_VIDIOC_STREAMOFF, "VIDIOC_STREAMOFF", &type) }

        // Dequeue a filled buffer and read the one it names, not buffer 0:
        // v4l2loopback hands its buffers out in turn. A shared camera's node
        // hands a new reader first the newest frame it holds, which was written
        // before this capture began: the black frame the agent writes when the
        // last reading session ended. Such a frame is queued again and the next
        // one taken, at most once per buffer, so a driver whose timestamps say
        // something else still gives a frame.
        var dqbuf = v4l2_buffer()
        var skipped = 0
        while true {
            guard try calls.waitForFrame(fd, milliseconds: frameTimeoutMilliseconds) else {
                throw VideoError.noFrame(path: path, milliseconds: frameTimeoutMilliseconds)
            }
            dqbuf = v4l2_buffer()
            dqbuf.type = V4L2_BUF_TYPE_VIDEO_CAPTURE
            dqbuf.memory = V4L2_MEMORY_MMAP
            try calls.control(fd, WENDY_VIDIOC_DQBUF, "VIDIOC_DQBUF", &dqbuf)
            guard Int(dqbuf.index) < mapped.count else {
                throw VideoError.invalidData(
                    message: "VIDIOC_DQBUF on \(path) returned buffer \(dqbuf.index); \(mapped.count) were mapped"
                )
            }
            guard skipped < mapped.count, capturedBefore(dqbuf, streamStart) else {
                break
            }
            skipped += 1
            var again = v4l2_buffer()
            again.type = V4L2_BUF_TYPE_VIDEO_CAPTURE
            again.memory = V4L2_MEMORY_MMAP
            again.index = dqbuf.index
            try calls.control(fd, WENDY_VIDIOC_QBUF, "VIDIOC_QBUF", &again)
        }
        let buffer = mapped[Int(dqbuf.index)]

        let usedBytes = min(Int(dqbuf.bytesused), buffer.length, Int(frameSize))
        let neededBytes = Int(bytesPerLine) * Int(height - 1) + Int(width) * 2
        guard usedBytes >= neededBytes else {
            throw VideoError.invalidData(
                message: "Short frame: \(usedBytes) bytes, \(width)x\(height) needs \(neededBytes)"
            )
        }
        let imageData = Data(bytes: buffer.address, count: usedBytes)

        return CapturedFrame(
            data: imageData,
            width: width,
            height: height,
            bytesPerLine: bytesPerLine
        )
    }

    /// Capture a frame and convert it to RGB format
    /// - Parameters:
    ///   - width: The desired frame width
    ///   - height: The desired frame height
    /// - Returns: RGB frame data (3 bytes per pixel)
    public func captureRGBFrame(
        width: UInt32 = 640,
        height: UInt32 = 480,
        compression: Double = 0.0,
        writer: inout some JPEG.Bytestream.Destination
    ) async throws {
        let frame = try captureFrame(width: width, height: height)
        let width = frame.width
        let height = frame.height
        let rgbData = try yuyvSRGBToRGB(
            yuyvSRGBData: frame.data,
            width: Int(width),
            height: Int(height),
            bytesPerLine: Int(frame.bytesPerLine)
        )

        let layout = JPEG.Layout<JPEG.Common>(
            format: .ycc8,
            process: .baseline,
            components: [
                1: (factor: (2, 2), qi: 0),  // Y
                2: (factor: (1, 1), qi: 1),  // Cb
                3: (factor: (1, 1), qi: 1),  // Cr
            ],
            scans: [
                .sequential((1, \.0, \.0), (2, \.1, \.1), (3, \.1, \.1))
            ]
        )

        print(width, height, rgbData.count)

        let jfif = JPEG.JFIF(version: .v1_2, density: (x: 10, y: 10, unit: .centimeters))
        let image: JPEG.Data.Rectangular<JPEG.Common> = JPEG.Data.Rectangular<JPEG.Common>.pack(
            size: (x: Int(width), y: Int(height)),
            layout: layout,
            metadata: [.jfif(jfif)],
            pixels: rgbData
        )

        try image.compress(
            stream: &writer,
            quanta: [
                0: JPEG.CompressionLevel.luminance(compression).quanta,
                1: JPEG.CompressionLevel.chrominance(compression).quanta,
            ]
        )
    }
}

/// How long a capture waits for a frame. A shared camera starts when an app
/// begins reading, which takes about a second.
let frameTimeoutMilliseconds: Int32 = 10_000

/// The V4L2_BUF_FLAG_TIMESTAMP_* type bits of a buffer's flags, and the two
/// types whose timestamps are CLOCK_MONOTONIC times: the driver's own
/// (uvcvideo), and the writer's, copied (v4l2loopback; the agent writes
/// CLOCK_MONOTONIC times).
let timestampTypeMask: UInt32 = 0xE000
let timestampMonotonic: UInt32 = 0x2000
let timestampCopy: UInt32 = 0x4000

/// Whether a dequeued buffer holds a frame from before `start` (CLOCK_MONOTONIC
/// nanoseconds). A buffer whose timestamps are of another kind never is.
func capturedBefore(_ buffer: v4l2_buffer, _ start: Int64) -> Bool {
    let kind = buffer.flags & timestampTypeMask
    guard kind == timestampMonotonic || kind == timestampCopy else {
        return false
    }
    let nanoseconds = Int64(buffer.timestamp.tv_sec) * 1_000_000_000 + Int64(buffer.timestamp.tv_usec) * 1_000
    return nanoseconds < start
}

/// Errors that can occur when working with V4L2 devices
public enum VideoError: Error {
    case deviceOpenFailed(path: String, errno: Int32)
    case ioctlFailed(command: String, errno: Int32)
    case mapFailed(index: UInt32, length: Int, offset: Int, errno: Int32)
    case noFrame(path: String, milliseconds: Int32)
    case unsupportedOperation(message: String)
    case invalidData(message: String)
}

extension VideoError: CustomStringConvertible {
    public var description: String {
        switch self {
        case .deviceOpenFailed(let path, let errno):
            return "opening \(path) failed: \(errnoDescription(errno))"
        case .ioctlFailed(let command, let errno):
            return "\(command) failed: \(errnoDescription(errno))"
        case .mapFailed(let index, let length, let offset, let errno):
            return "mapping buffer \(index) (\(length) bytes at offset \(offset)) failed: \(errnoDescription(errno))"
        case .noFrame(let path, let milliseconds):
            return "no frame from \(path) within \(Double(milliseconds) / 1000) s"
        case .unsupportedOperation(let message), .invalidData(let message):
            return message
        }
    }
}

/// "Invalid argument (errno 22)".
func errnoDescription(_ errno: Int32) -> String {
    "\(String(cString: strerror(errno))) (errno \(errno))"
}

/// The system calls a capture makes. `SystemV4L2Calls` makes the real ones;
/// tests stand in a driver.
protocol V4L2Calls {
    /// Opens the device read-write.
    func openDevice(_ path: String) throws -> Int32
    /// Runs the ioctl `request` (named `name` in errors) on `argument`.
    func control(_ fd: Int32, _ request: UInt32, _ name: String, _ argument: UnsafeMutableRawPointer) throws
    /// Maps buffer `index`, `length` bytes at `offset`, shared and read-write.
    func map(_ fd: Int32, index: UInt32, length: Int, offset: Int) throws -> UnsafeMutableRawPointer
    func unmap(_ address: UnsafeMutableRawPointer, length: Int)
    /// Waits up to `milliseconds` for a filled buffer; false when none came.
    func waitForFrame(_ fd: Int32, milliseconds: Int32) throws -> Bool
    /// CLOCK_MONOTONIC, in nanoseconds.
    func monotonicNanoseconds() -> Int64
    func closeDevice(_ fd: Int32)
}

struct SystemV4L2Calls: V4L2Calls {
    func openDevice(_ path: String) throws -> Int32 {
        let fd = open(path, O_RDWR)
        if fd < 0 {
            throw VideoError.deviceOpenFailed(path: path, errno: errno)
        }
        return fd
    }

    func control(_ fd: Int32, _ request: UInt32, _ name: String, _ argument: UnsafeMutableRawPointer) throws {
        if ioctl(fd, UInt(request), argument) < 0 {
            throw VideoError.ioctlFailed(command: name, errno: errno)
        }
    }

    func map(_ fd: Int32, index: UInt32, length: Int, offset: Int) throws -> UnsafeMutableRawPointer {
        guard let address = mmap(nil, length, PROT_READ | PROT_WRITE, MAP_SHARED, fd, offset),
            address != MAP_FAILED
        else {
            throw VideoError.mapFailed(index: index, length: length, offset: offset, errno: errno)
        }
        return address
    }

    func unmap(_ address: UnsafeMutableRawPointer, length: Int) {
        munmap(address, length)
    }

    func waitForFrame(_ fd: Int32, milliseconds: Int32) throws -> Bool {
        var request = pollfd(fd: fd, events: Int16(POLLIN), revents: 0)
        while true {
            let ready = poll(&request, 1, milliseconds)
            if ready >= 0 {
                return ready > 0
            }
            if errno != EINTR {
                throw VideoError.ioctlFailed(command: "poll", errno: errno)
            }
        }
    }

    func monotonicNanoseconds() -> Int64 {
        var now = timespec()
        clock_gettime(CLOCK_MONOTONIC, &now)
        return Int64(now.tv_sec) * 1_000_000_000 + Int64(now.tv_nsec)
    }

    func closeDevice(_ fd: Int32) {
        close(fd)
    }
}

/// A class providing access to V4L2 video devices
public class VideoDeviceManager {

    /// List all available video devices in the system
    /// - Returns: An array of VideoDevice objects
    public static func listDevices() throws -> [VideoDevice] {
        var devices: [VideoDevice] = []

        // Look for devices in /dev
        let fileManager = FileManager.default
        let devDir = "/dev"

        do {
            let contents = try fileManager.contentsOfDirectory(atPath: devDir)

            // Filter for video device nodes (video0, video1, etc.)
            let videoDevices = videoNodesInNumberOrder(
                contents.filter {
                    $0.hasPrefix("video")
                        && CharacterSet.decimalDigits.contains(
                            $0.last?.unicodeScalars.first ?? Unicode.Scalar(0)
                        )
                })

            // Try to open each device and query its capabilities
            for deviceName in videoDevices {
                let devicePath = "\(devDir)/\(deviceName)"

                do {
                    let device = try queryDeviceInfo(path: devicePath)
                    devices.append(device)
                } catch {
                    print("Warning: Could not query device \(devicePath): \(error)")
                    // Continue to the next device
                }
            }

        } catch {
            throw VideoError.unsupportedOperation(
                message: "Could not list contents of /dev: \(error)"
            )
        }

        return devices
    }

    /// Print information about all available video devices to the console
    public static func printDeviceList() {
        do {
            let devices = try listDevices()

            if devices.isEmpty {
                print("No video devices found.")
                return
            }

            print("Available video devices:")
            print("------------------------")

            for (index, device) in devices.enumerated() {
                print("[\(index)] \(device.path):")
                print("    Name: \(device.name)")
                print("    Driver: \(device.driver)")
                print("    Bus info: \(device.busInfo)")
                print("    Capabilities: \(String(format: "0x%08X", device.capabilities))")
                print("    Supports capture: \(device.supportsCapture ? "Yes" : "No")")
                print("    Supports streaming: \(device.supportsStreaming ? "Yes" : "No")")
                print("")
            }
        } catch {
            print("Error listing video devices: \(error)")
        }
    }

    // MARK: - Private Methods

    /// Query a device's information
    /// - Parameter path: The path to the video device
    /// - Returns: A VideoDevice object with the device's information
    private static func queryDeviceInfo(path: String) throws -> VideoDevice {
        // Open the device
        let fd = open(path, O_RDWR)
        if fd < 0 {
            throw VideoError.deviceOpenFailed(path: path, errno: errno)
        }
        defer { close(fd) }

        // Query device capabilities using a manual approach since the v4l2_capability struct may not be exposed correctly
        // Create a buffer large enough to hold v4l2_capability (typically 104 bytes)
        var capabilityBuffer = [UInt8](repeating: 0, count: 104)

        let result = ioctl(fd, UInt(WENDY_VIDIOC_QUERYCAP), &capabilityBuffer)

        if result < 0 {
            throw VideoError.ioctlFailed(command: "VIDIOC_QUERYCAP", errno: errno)
        }

        // Extract information from the buffer
        // The layout of v4l2_capability is:
        // - driver: char[16]
        // - card: char[32]
        // - bus_info: char[32]
        // - version: __u32
        // - capabilities: __u32
        // - device_caps: __u32
        // - reserved: __u32[3]

        // Extract strings - this assumes null-terminated C strings
        let driverData = capabilityBuffer[0..<16]
        let nameData = capabilityBuffer[16..<48]
        let busInfoData = capabilityBuffer[48..<80]

        // Extract capabilities (4 bytes at offset 84)
        let capabilities = capabilityBuffer[84..<88].withUnsafeBytes { $0.load(as: UInt32.self) }

        // Convert data to strings
        let driver = String(bytes: driverData.filter { $0 != 0 }, encoding: .utf8) ?? "Unknown"
        let name = String(bytes: nameData.filter { $0 != 0 }, encoding: .utf8) ?? "Unknown"
        let busInfo = String(bytes: busInfoData.filter { $0 != 0 }, encoding: .utf8) ?? "Unknown"

        return VideoDevice(
            path: path,
            name: name,
            driver: driver,
            busInfo: busInfo,
            capabilities: capabilities
        )
    }
}

/// Convert YUV values to RGB
/// - Parameters:
///   - y: Luminance value
///   - u: U chrominance value
///   - v: V chrominance value
/// - Returns: Tuple containing (red, green, blue) values
private func yuvToRGB(y: UInt8, u: UInt8, v: UInt8) -> (r: UInt8, g: UInt8, b: UInt8) {
    // Convert YUV to RGB using BT.601 conversion
    // First, adjust ranges
    let y1 = Int32(y)
    let u1 = Int32(u) - 128
    let v1 = Int32(v) - 128

    // Calculate RGB values
    var r = (y1 + (359 * v1) / 256)
    var g = (y1 - (88 * u1) / 256 - (183 * v1) / 256)
    var b = (y1 + (454 * u1) / 256)

    // Clamp values to 0-255
    r = max(0, min(255, r))
    g = max(0, min(255, g))
    b = max(0, min(255, b))

    return (UInt8(r), UInt8(g), UInt8(b))
}

/// Convert YUYV frame data to RGB
/// - Parameters:
///   - yuyvSRGBData: Raw YUYV SRGB frame data
///   - width: Frame width
///   - height: Frame height
///   - bytesPerLine: Bytes per row in the buffer; defaults to `width * 2`
/// - Returns: RGB data (3 bytes per pixel)
public func yuyvSRGBToRGB(
    yuyvSRGBData: Data,
    width: Int,
    height: Int,
    bytesPerLine: Int? = nil
) throws -> [JPEG.RGB] {
    let bytesPerLine = bytesPerLine ?? width * 2
    guard yuyvSRGBData.count % 4 == 0 else {
        throw VideoError.invalidData(message: "Width must be even")
    }

    var rgbData = [JPEG.RGB](
        repeating: JPEG.RGB(0, 0, 0),
        count: width * height
    )

    // Process two pixels at a time (4 bytes YUYV -> 6 bytes RGB)
    for y in 0..<height {
        for x in stride(from: 0, to: width, by: 2) {
            let index = y * bytesPerLine + x * 2
            guard index + 3 < yuyvSRGBData.count else { break }

            // Extract YUYV values for two pixels
            let y1 = yuyvSRGBData[index]
            let u = yuyvSRGBData[index + 1]
            let y2 = yuyvSRGBData[index + 2]
            let v = yuyvSRGBData[index + 3]

            // Convert first pixel
            let rgb1 = yuvToRGB(y: y1, u: u, v: v)
            let rgbIndex1 = y * width + x
            rgbData[rgbIndex1] = JPEG.RGB(rgb1.r, rgb1.g, rgb1.b)

            // Convert second pixel if within bounds
            if x + 1 < width {
                let rgb2 = yuvToRGB(y: y2, u: u, v: v)
                let rgbIndex2 = y * width + x + 1
                rgbData[rgbIndex2] = JPEG.RGB(rgb2.r, rgb2.g, rgb2.b)
            }
        }
    }

    return rgbData
}
