import Foundation

/// Shared rolling buffer of captured frames, written by the camera thread
/// and read by the inference loop.
final class FrameBuffer: @unchecked Sendable {
    private let lock = NSLock()
    private var frames: [FrameCapture] = []

    func append(_ frame: FrameCapture) {
        lock.lock()
        frames.append(frame)
        let cutoff = Date().addingTimeInterval(-600)
        frames.removeAll { $0.capturedAt < cutoff }
        lock.unlock()
    }

    func window(within seconds: TimeInterval) -> [FrameCapture] {
        let cutoff = Date().addingTimeInterval(-seconds)
        lock.lock()
        defer { lock.unlock() }
        return frames.filter { $0.capturedAt >= cutoff }
    }
}

/// Motion-JPEG frames commonly omit the Huffman tables (DHT) and rely on the
/// JPEG Annex K defaults. Lenient decoders (llama.cpp, ffmpeg) fill them in;
/// Safari and macOS ImageIO refuse to decode such frames. Insert the standard
/// tables before the scan when they are missing.
func withStandardHuffmanTables(_ jpeg: Data) -> Data {
    var i = 2
    while i + 3 < jpeg.count, jpeg[i] == 0xFF {
        let marker = jpeg[i + 1]
        if marker == 0xC4 {
            return jpeg
        }
        if marker == 0xDA {
            break
        }
        if (0xD0...0xD9).contains(marker) {
            i += 2
            continue
        }
        let length = Int(jpeg[i + 2]) << 8 | Int(jpeg[i + 3])
        i += 2 + length
    }

    guard i + 1 < jpeg.count, jpeg[i] == 0xFF, jpeg[i + 1] == 0xDA else {
        return jpeg
    }

    var patched = Data(capacity: jpeg.count + standardJPEGHuffmanTables.count)
    patched.append(jpeg.prefix(i))
    patched.append(standardJPEGHuffmanTables)
    patched.append(jpeg.suffix(from: i))
    return patched
}

/// The four DHT segments (DC/AC, luminance/chrominance) from JPEG Annex K.
private let standardJPEGHuffmanTables = Data(base64Encoded: [
    "/8QAHwAAAQUBAQEBAQEAAAAAAAAAAAECAwQFBgcICQoL/8QAtRAAAgEDAwIEAwUFBAQAAAF9AQID",
    "AAQRBRIhMUEGE1FhByJxFDKBkaEII0KxwRVS0fAkM2JyggkKFhcYGRolJicoKSo0NTY3ODk6Q0RF",
    "RkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqDhIWGh4iJipKTlJWWl5iZmqKjpKWmp6ipqrKz",
    "tLW2t7i5usLDxMXGx8jJytLT1NXW19jZ2uHi4+Tl5ufo6erx8vP09fb3+Pn6/8QAHwEAAwEBAQEB",
    "AQEBAQAAAAAAAAECAwQFBgcICQoL/8QAtREAAgECBAQDBAcFBAQAAQJ3AAECAxEEBSExBhJBUQdh",
    "cRMiMoEIFEKRobHBCSMzUvAVYnLRChYkNOEl8RcYGRomJygpKjU2Nzg5OkNERUZHSElKU1RVVldY",
    "WVpjZGVmZ2hpanN0dXZ3eHl6goOEhYaHiImKkpOUlZaXmJmaoqOkpaanqKmqsrO0tba3uLm6wsPE",
    "xcbHyMnK0tPU1dbX2Nna4uPk5ebn6Onq8vP09fb3+Pn6",
].joined())!

#if os(Linux)

import CLinuxVideo

// V4L2 constants from linux/videodev2.h.
private let V4L2_CAP_VIDEO_CAPTURE: UInt32 = 0x0000_0001
private let V4L2_BUF_TYPE_VIDEO_CAPTURE: UInt32 = 1
private let V4L2_MEMORY_MMAP: UInt32 = 1
private let V4L2_PIX_FMT_MJPEG: UInt32 = 0x4750_4A4D  // 'MJPG'
private let V4L2_PIX_FMT_YUYV: UInt32 = 0x5659_5559  // 'YUYV'

/// Captures frames from a V4L2 device on a dedicated thread.
///
/// The app asks for MJPG. A shared camera (the agent's virtual camera) gives
/// YUYV at its own size instead, which `S_FMT` reports back; the app then
/// reads the size from the result and encodes only the frames it samples.
///
/// The device is kept streaming continuously so exposure/white balance can
/// settle (one-shot captures return green/garbage frames on many UVC
/// cameras). Frames are sampled into the rolling buffer at the configured
/// rate; the camera itself runs at its native rate.
final class LinuxCamera: @unchecked Sendable {
    private let config: AppConfig
    private let state: AppState
    private let buffer: FrameBuffer

    init(config: AppConfig, state: AppState, buffer: FrameBuffer) {
        self.config = config
        self.state = state
        self.buffer = buffer
    }

    func start() async {
        await state.setCameraStarting()

        let devices = listCaptureDevices()
        print("Available cameras:")
        for device in devices {
            print("  - \(device.name) [\(device.path)]")
        }

        let device: (path: String, name: String)?
        if let name = config.camera, !name.isEmpty {
            device = devices.first { $0.name.localizedCaseInsensitiveContains(name) }
        } else {
            device = devices.first
        }

        guard let device else {
            await state.setCameraFailed(message: "No camera found.")
            return
        }

        print("Using: \(device.name) [\(device.path)]")
        await state.setCameraReady(name: device.name)

        let thread = Thread { [self] in
            captureLoop(path: device.path, name: device.name)
        }
        thread.name = "HelloVLM.Camera"
        thread.start()
    }

    private struct CameraError: Error, CustomStringConvertible {
        let description: String
    }

    private func captureLoop(path: String, name: String) {
        while true {
            do {
                try streamFrames(path: path)
            } catch {
                Task { [state] in
                    await state.setCameraFailed(message: "Camera error: \(error)")
                }
                print("Camera error: \(error); retrying in 3s")
                Thread.sleep(forTimeInterval: 3)
            }
        }
    }

    private func streamFrames(path: String) throws {
        let fd = open(path, O_RDWR)
        guard fd >= 0 else {
            throw CameraError(description: "open(\(path)) failed: errno \(errno)")
        }
        defer { close(fd) }

        var format = v4l2_format()
        format.type = V4L2_BUF_TYPE_VIDEO_CAPTURE
        format.fmt.pix.width = UInt32(config.width)
        format.fmt.pix.height = UInt32(config.height)
        format.fmt.pix.pixelformat = V4L2_PIX_FMT_MJPEG
        format.fmt.pix.field = V4L2_FIELD_NONE.rawValue
        guard ioctl(fd, UInt(WENDY_VIDIOC_S_FMT), &format) >= 0 else {
            throw CameraError(description: "VIDIOC_S_FMT failed: errno \(errno)")
        }
        let pixelFormat = format.fmt.pix.pixelformat
        guard pixelFormat == V4L2_PIX_FMT_MJPEG || pixelFormat == V4L2_PIX_FMT_YUYV else {
            throw CameraError(
                description:
                    "Camera gave pixel format \(fourCC(pixelFormat)); only MJPG and YUYV are supported."
            )
        }
        // The camera may not give the size we asked for; use what it gave.
        let frameWidth = Int(format.fmt.pix.width)
        let frameHeight = Int(format.fmt.pix.height)
        let bytesPerLine = Int(format.fmt.pix.bytesperline)
        if pixelFormat == V4L2_PIX_FMT_YUYV {
            print("Camera delivers YUYV \(frameWidth)x\(frameHeight); encoding sampled frames to JPEG")
        }

        var reqbuf = v4l2_requestbuffers()
        reqbuf.count = 4
        reqbuf.memory = V4L2_MEMORY_MMAP
        reqbuf.type = V4L2_BUF_TYPE_VIDEO_CAPTURE
        guard ioctl(fd, UInt(WENDY_VIDIOC_REQBUFS), &reqbuf) >= 0, reqbuf.count > 0 else {
            throw CameraError(description: "VIDIOC_REQBUFS failed: errno \(errno)")
        }

        var mapped: [(pointer: UnsafeMutableRawPointer, length: Int)] = []
        defer {
            for buffer in mapped {
                munmap(buffer.pointer, buffer.length)
            }
        }

        for index in 0..<reqbuf.count {
            var buffer = v4l2_buffer()
            buffer.type = V4L2_BUF_TYPE_VIDEO_CAPTURE
            buffer.memory = V4L2_MEMORY_MMAP
            buffer.index = index
            guard ioctl(fd, UInt(WENDY_VIDIOC_QUERYBUF), &buffer) >= 0 else {
                throw CameraError(description: "VIDIOC_QUERYBUF failed: errno \(errno)")
            }
            guard
                let pointer = mmap(nil, Int(buffer.length), PROT_READ | PROT_WRITE, MAP_SHARED, fd, Int(buffer.m.offset)),
                pointer != MAP_FAILED
            else {
                throw CameraError(description: "mmap failed: errno \(errno)")
            }
            mapped.append((pointer, Int(buffer.length)))

            guard ioctl(fd, UInt(WENDY_VIDIOC_QBUF), &buffer) >= 0 else {
                throw CameraError(description: "VIDIOC_QBUF failed: errno \(errno)")
            }
        }

        var type = V4L2_BUF_TYPE_VIDEO_CAPTURE
        guard ioctl(fd, UInt(WENDY_VIDIOC_STREAMON), &type) >= 0 else {
            throw CameraError(description: "VIDIOC_STREAMON failed: errno \(errno)")
        }
        defer {
            _ = ioctl(fd, UInt(WENDY_VIDIOC_STREAMOFF), &type)
        }

        // Let exposure/white balance settle before sampling.
        let warmupFrames = 10
        var framesSeen = 0
        var lastSampledAt: Date?
        let minGap = 1.0 / config.fps

        while true {
            var buffer = v4l2_buffer()
            buffer.type = V4L2_BUF_TYPE_VIDEO_CAPTURE
            buffer.memory = V4L2_MEMORY_MMAP
            guard ioctl(fd, UInt(WENDY_VIDIOC_DQBUF), &buffer) >= 0 else {
                throw CameraError(description: "VIDIOC_DQBUF failed: errno \(errno)")
            }

            framesSeen += 1
            let now = Date()
            let shouldSample =
                framesSeen > warmupFrames
                && buffer.bytesused > 0
                && (lastSampledAt == nil || now.timeIntervalSince(lastSampledAt!) >= minGap)

            if shouldSample {
                lastSampledAt = now
                let source = mapped[Int(buffer.index)]
                let jpeg: Data
                if pixelFormat == V4L2_PIX_FMT_YUYV {
                    jpeg = try jpegFromYUYV(
                        UnsafeRawBufferPointer(start: source.pointer, count: Int(buffer.bytesused)),
                        width: frameWidth,
                        height: frameHeight,
                        bytesPerLine: bytesPerLine
                    )
                } else {
                    jpeg = withStandardHuffmanTables(
                        Data(bytes: source.pointer, count: Int(buffer.bytesused))
                    )
                }
                let frame = FrameCapture(capturedAt: now, jpeg: jpeg)
                buffer.bytesused = 0
                self.buffer.append(frame)
                Task { [state] in
                    await state.setLiveFrame(jpeg: frame.jpeg, at: frame.capturedAt)
                }
            }

            guard ioctl(fd, UInt(WENDY_VIDIOC_QBUF), &buffer) >= 0 else {
                throw CameraError(description: "VIDIOC_QBUF failed: errno \(errno)")
            }
        }
    }

    private func fourCC(_ value: UInt32) -> String {
        let bytes = (0..<4).map { UInt8((value >> (8 * UInt32($0))) & 0xFF) }
        let text = String(bytes: bytes, encoding: .ascii) ?? ""
        return text.isEmpty ? String(format: "0x%08X", value) : "'\(text)'"
    }

    private func listCaptureDevices() -> [(path: String, name: String)] {
        let fileManager = FileManager.default
        guard let entries = try? fileManager.contentsOfDirectory(atPath: "/dev") else {
            return []
        }

        return entries
            .filter { $0.hasPrefix("video") && $0.dropFirst(5).allSatisfy(\.isNumber) }
            .sorted()
            .compactMap { entry in
                let path = "/dev/\(entry)"
                guard let info = queryDevice(path: path), info.supportsCapture else { return nil }
                return (path, info.name)
            }
    }

    private func queryDevice(path: String) -> (name: String, supportsCapture: Bool)? {
        let fd = open(path, O_RDWR)
        guard fd >= 0 else { return nil }
        defer { close(fd) }

        // v4l2_capability layout: driver[16], card[32], bus_info[32],
        // version u32, capabilities u32 (offset 84).
        var capability = [UInt8](repeating: 0, count: 104)
        guard ioctl(fd, UInt(WENDY_VIDIOC_QUERYCAP), &capability) >= 0 else { return nil }

        let name = String(bytes: capability[16..<48].filter { $0 != 0 }, encoding: .utf8) ?? path
        let capabilities = capability[84..<88].withUnsafeBytes { $0.load(as: UInt32.self) }
        return (name, (capabilities & V4L2_CAP_VIDEO_CAPTURE) != 0)
    }
}

#else

/// The camera requires Linux/V4L2; on other platforms the app still builds
/// so the web server and VLM client can be exercised locally.
final class LinuxCamera: @unchecked Sendable {
    private let state: AppState

    init(config: AppConfig, state: AppState, buffer: FrameBuffer) {
        self.state = state
    }

    func start() async {
        await state.setCameraFailed(message: "Camera capture requires Linux (V4L2). Deploy to a WendyOS device.")
    }
}

#endif
