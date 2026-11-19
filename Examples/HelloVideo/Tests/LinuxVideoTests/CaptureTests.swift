import CLinuxVideo
import Foundation
import Testing

@testable import LinuxVideo

/// A capture driver in memory, with the two behaviours that differ between
/// the drivers HelloVideo meets:
///
/// - uvcvideo (videobuf2) fills in a struct passed to VIDIOC_QBUF, length and
///   offset included, and hands back the buffer that was queued first.
/// - v4l2loopback 0.15.4, the node a shared camera gives an app, does not:
///   its vidioc_qbuf for CAPTURE sets only the struct's flags, so its length
///   and m.offset stay as the caller passed them. A reader gets as many
///   buffers as the writer uses (2 for the agent's share writer), and VIDIOC_DQBUF
///   returns them in turn, starting with buffer 1.
///
/// mmap fails as the kernel does: EINVAL for 0 bytes, or for an offset or
/// length that is not a buffer's.
final class FakeDriver: V4L2Calls {
    let width: UInt32 = 1280
    let height: UInt32 = 720
    var frameBytes: Int { Int(width * height * 2) }
    let fillsQueuedBuffer: Bool
    let buffersGiven: UInt32
    let dequeueOrder: [UInt32]

    private var storage: [UnsafeMutableRawPointer] = []
    private var dequeues = 0
    private(set) var mapped: [(length: Int, offset: Int)] = []
    private(set) var unmapped = 0
    private(set) var closed = false
    private(set) var streaming = false

    init(fillsQueuedBuffer: Bool, buffersGiven: UInt32, dequeueOrder: [UInt32]) {
        self.fillsQueuedBuffer = fillsQueuedBuffer
        self.buffersGiven = buffersGiven
        self.dequeueOrder = dequeueOrder
    }

    deinit {
        storage.forEach { $0.deallocate() }
    }

    /// Every byte of buffer i is i + 1, so the test can tell them apart.
    func contents(of index: Int) -> UInt8 { UInt8(index + 1) }

    func openDevice(_ path: String) throws -> Int32 { 7 }

    func closeDevice(_ fd: Int32) { closed = true }

    func control(_ fd: Int32, _ request: UInt32, _ name: String, _ argument: UnsafeMutableRawPointer) throws {
        switch request {
        case WENDY_VIDIOC_S_FMT:
            // A shared camera keeps its own mode, whatever is asked for.
            let format = argument.assumingMemoryBound(to: v4l2_format.self)
            format.pointee.fmt.pix.width = width
            format.pointee.fmt.pix.height = height
            format.pointee.fmt.pix.pixelformat = LinuxVideo.V4L2_PIX_FMT_YUYV
            format.pointee.fmt.pix.bytesperline = width * 2
            format.pointee.fmt.pix.sizeimage = UInt32(frameBytes)
        case WENDY_VIDIOC_REQBUFS:
            let request = argument.assumingMemoryBound(to: v4l2_requestbuffers.self)
            request.pointee.count = buffersGiven
            while storage.count < Int(buffersGiven) {
                let buffer = UnsafeMutableRawPointer.allocate(byteCount: frameBytes, alignment: 16)
                buffer.initializeMemory(as: UInt8.self, repeating: contents(of: storage.count), count: frameBytes)
                storage.append(buffer)
            }
        case WENDY_VIDIOC_QUERYBUF:
            let buffer = argument.assumingMemoryBound(to: v4l2_buffer.self)
            guard buffer.pointee.index < buffersGiven else { throw VideoError.ioctlFailed(command: name, errno: EINVAL) }
            describe(buffer, index: buffer.pointee.index)
        case WENDY_VIDIOC_QBUF:
            let buffer = argument.assumingMemoryBound(to: v4l2_buffer.self)
            guard buffer.pointee.index < buffersGiven else { throw VideoError.ioctlFailed(command: name, errno: EINVAL) }
            if fillsQueuedBuffer {
                describe(buffer, index: buffer.pointee.index)
            }
            buffer.pointee.flags |= 0x2  // V4L2_BUF_FLAG_QUEUED
        case WENDY_VIDIOC_STREAMON:
            streaming = true
        case WENDY_VIDIOC_STREAMOFF:
            streaming = false
        case WENDY_VIDIOC_DQBUF:
            guard streaming else { throw VideoError.ioctlFailed(command: name, errno: EINVAL) }
            let buffer = argument.assumingMemoryBound(to: v4l2_buffer.self)
            let index = dequeueOrder[dequeues % dequeueOrder.count]
            dequeues += 1
            describe(buffer, index: index)
            buffer.pointee.bytesused = UInt32(frameBytes)
        default:
            throw VideoError.ioctlFailed(command: name, errno: ENOTTY)
        }
    }

    private func describe(_ buffer: UnsafeMutablePointer<v4l2_buffer>, index: UInt32) {
        buffer.pointee.index = index
        buffer.pointee.length = UInt32(frameBytes)
        buffer.pointee.m.offset = index * UInt32(frameBytes)
    }

    func map(_ fd: Int32, index: UInt32, length: Int, offset: Int) throws -> UnsafeMutableRawPointer {
        guard length > 0, length <= frameBytes, offset % frameBytes == 0, offset / frameBytes < storage.count else {
            throw VideoError.mapFailed(index: index, length: length, offset: offset, errno: EINVAL)
        }
        mapped.append((length, offset))
        return storage[offset / frameBytes]
    }

    func unmap(_ address: UnsafeMutableRawPointer, length: Int) { unmapped += 1 }
}

@Suite struct CaptureTests {
    let device = VideoDevice(
        path: "/dev/video0", name: "Brio 101", driver: "v4l2 loopback", busInfo: "", capabilities: LinuxVideo.V4L2_CAP_VIDEO_CAPTURE)

    @Test func capturesFromAShareNode() throws {
        // The share node leaves the QBUF struct's length and offset at 0 and
        // returns buffer 1 first.
        let driver = FakeDriver(fillsQueuedBuffer: false, buffersGiven: 2, dequeueOrder: [1, 0])
        let frame = try device.captureFrame(width: 640, height: 480, calls: driver)

        #expect(frame.width == 1280 && frame.height == 720 && frame.bytesPerLine == 2560)
        #expect(frame.data.count == driver.frameBytes)
        // The bytes are buffer 1's, the one VIDIOC_DQBUF named.
        #expect(frame.data.allSatisfy { $0 == driver.contents(of: 1) })
        // Every buffer was mapped at the length and offset QUERYBUF gave.
        #expect(driver.mapped.map(\.length) == [driver.frameBytes, driver.frameBytes])
        #expect(driver.mapped.map(\.offset) == [0, driver.frameBytes])
        #expect(driver.unmapped == 2 && driver.closed && !driver.streaming)
    }

    @Test func capturesFromACamera() throws {
        let driver = FakeDriver(fillsQueuedBuffer: true, buffersGiven: 2, dequeueOrder: [0])
        let frame = try device.captureFrame(width: 1280, height: 720, calls: driver)

        #expect(frame.data.count == driver.frameBytes)
        #expect(frame.data.allSatisfy { $0 == driver.contents(of: 0) })
        #expect(driver.unmapped == 2 && driver.closed && !driver.streaming)
    }

    @Test func aMapFailureSaysWhatAndWhy() {
        #expect(
            VideoError.mapFailed(index: 0, length: 0, offset: 0, errno: EINVAL).description
                == "mapping buffer 0 (0 bytes at offset 0) failed: Invalid argument (errno 22)"
        )
        #expect(
            VideoError.ioctlFailed(command: "VIDIOC_DQBUF", errno: EIO).description
                == "VIDIOC_DQBUF failed: Input/output error (errno 5)"
        )
    }
}
