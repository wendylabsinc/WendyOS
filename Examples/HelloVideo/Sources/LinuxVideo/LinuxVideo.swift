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
        // Open the device
        let fd = open(path, O_RDWR)
        if fd < 0 {
            throw VideoError.deviceOpenFailed(path: path, errno: errno)
        }
        defer { close(fd) }

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

        print("Set format")

        // Set format
        var result = ioctl(fd, UInt(WENDY_VIDIOC_S_FMT), &format)
        if result < 0 {
            throw VideoError.ioctlFailed(command: "VIDIOC_S_FMT", errno: errno)
        }

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
        print("Set format \(width)x\(height), \(bytesPerLine) bytes per line")

        // Request buffers
        var reqbuf = v4l2_requestbuffers()
        reqbuf.count = 1
        reqbuf.memory = V4L2_MEMORY_MMAP
        reqbuf.type = V4L2_BUF_TYPE_VIDEO_CAPTURE

        result = ioctl(fd, UInt(WENDY_VIDIOC_REQBUFS), &reqbuf)
        if result < 0 {
            throw VideoError.ioctlFailed(command: "VIDIOC_REQBUFS", errno: errno)
        }

        print("Requested buffers")

        // Init buffers
        var buffer = v4l2_buffer()
        buffer.type = V4L2_BUF_TYPE_VIDEO_CAPTURE
        buffer.memory = V4L2_MEMORY_MMAP
        buffer.index = 0
        buffer.length = frameSize
        result = ioctl(fd, UInt(WENDY_VIDIOC_QUERYBUF), &buffer)
        if result < 0 {
            throw VideoError.ioctlFailed(command: "VIDIOC_QUERYBUF", errno: errno)
        }

        buffer = v4l2_buffer()
        buffer.memory = V4L2_MEMORY_MMAP
        buffer.type = V4L2_BUF_TYPE_VIDEO_CAPTURE
        buffer.index = 0

        result = ioctl(fd, UInt(WENDY_VIDIOC_QBUF), &buffer)
        if result < 0 {
            throw VideoError.ioctlFailed(command: "VIDIOC_QBUF", errno: errno)
        }

        print("Query buffer")

        // Start streaming
        var type = V4L2_BUF_TYPE_VIDEO_CAPTURE
        result = ioctl(fd, UInt(WENDY_VIDIOC_STREAMON), &type)
        if result < 0 {
            throw VideoError.ioctlFailed(command: "VIDIOC_STREAMON", errno: errno)
        }

        print("Started streaming")

        // Map the buffer
        guard
            let data = mmap(
                nil,
                Int(buffer.length),
                PROT_READ | PROT_WRITE,
                MAP_SHARED,
                fd,
                Int(buffer.m.offset)
            ),
            data != MAP_FAILED
        else {
            throw VideoError.unsupportedOperation(message: "Failed to map buffer")
        }

        defer { munmap(data, Int(buffer.length)) }

        print("Mapped buffer")

        // Dequeue buffer
        var dqbuf = v4l2_buffer()
        dqbuf.type = V4L2_BUF_TYPE_VIDEO_CAPTURE
        dqbuf.memory = V4L2_MEMORY_MMAP
        result = ioctl(fd, UInt(WENDY_VIDIOC_DQBUF), &dqbuf)
        if result < 0 {
            print(errno)
            throw VideoError.ioctlFailed(command: "VIDIOC_DQBUF", errno: errno)
        }

        print("Grabbing data")
        let imageData = Data(bytes: data, count: min(Int(buffer.length), Int(frameSize)))

        print("Stop streaming")

        // Stop streaming
        result = ioctl(fd, UInt(WENDY_VIDIOC_STREAMOFF), &type)
        if result < 0 {
            print(errno)
            throw VideoError.ioctlFailed(command: "VIDIOC_STREAMOFF", errno: errno)
        }

        // Copy the frame data
        print("Returning frame data")
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

/// Errors that can occur when working with V4L2 devices
public enum VideoError: Error {
    case deviceOpenFailed(path: String, errno: Int32)
    case ioctlFailed(command: String, errno: Int32)
    case unsupportedOperation(message: String)
    case invalidData(message: String)
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
