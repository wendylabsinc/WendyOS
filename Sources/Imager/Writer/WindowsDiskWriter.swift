#if os(Windows)
    import Foundation
    import Subprocess
    import WinSDK
    import ucrt

    // Windows IOCTL codes for volume operations
    // CTL_CODE(FILE_DEVICE_FILE_SYSTEM, function, METHOD_BUFFERED, FILE_ANY_ACCESS)
    // where FILE_DEVICE_FILE_SYSTEM = 0x00000009, METHOD_BUFFERED = 0, FILE_ANY_ACCESS = 0
    private let FSCTL_LOCK_VOLUME: UInt32 = 0x0009_0018  // (0x9 << 16) | (6 << 2)
    private let FSCTL_UNLOCK_VOLUME: UInt32 = 0x0009_001C  // (0x9 << 16) | (7 << 2)
    private let FSCTL_DISMOUNT_VOLUME: UInt32 = 0x0009_0020  // (0x9 << 16) | (8 << 2)

    /// Windows implementation of the DiskWriter protocol using PowerShell and direct file I/O.
    public final class WindowsDiskWriter: DiskWriter {
        public init() {}

        public func write(
            imagePath: String,
            drive: Drive,
            progressHandler: @escaping (DiskWriteProgress) -> Void
        ) async throws {
            // Check if image exists
            guard FileManager.default.fileExists(atPath: imagePath) else {
                throw DiskWriterError.imageNotFoundInPath(path: imagePath)
            }

            // Check if image is a .img file
            guard imagePath.hasSuffix(".img") else {
                throw DiskWriterError.imageFileIncorrectType
            }

            // Get total image file size
            let totalBytes: Int64?
            do {
                let attributes = try FileManager.default.attributesOfItem(atPath: imagePath)
                if let fileSizeNumber = attributes[.size] as? NSNumber {
                    totalBytes = fileSizeNumber.int64Value
                } else if let fileSize = attributes[.size] as? Int {
                    totalBytes = Int64(fileSize)
                } else {
                    totalBytes = nil
                }
            } catch {
                totalBytes = nil
            }

            // Send initial progress update
            let initialTotalBytes: Int64 = totalBytes ?? 100
            progressHandler(
                DiskWriteProgress(
                    bytesWritten: 0,
                    totalBytes: initialTotalBytes
                )
            )

            // Extract disk number from drive ID (e.g., "Disk0" -> "0")
            let diskNumber = drive.id.replacingOccurrences(of: "Disk", with: "")

            do {
                let diskPath = "\\\\.\\PhysicalDrive\(diskNumber)"
                let diskPathW = Array(diskPath.utf16) + [0]
                let disk = CreateFileW(
                    diskPathW,
                    GENERIC_READ | UInt32(bitPattern: GENERIC_WRITE),
                    UInt32(bitPattern: FILE_SHARE_READ | FILE_SHARE_WRITE),
                    nil,
                    UInt32(bitPattern: OPEN_EXISTING),
                    UInt32(bitPattern: FILE_FLAG_NO_BUFFERING) | FILE_FLAG_WRITE_THROUGH,
                    nil
                )

                guard let disk, disk != INVALID_HANDLE_VALUE else {
                    let errorCode = GetLastError()
                    let reason: String
                    switch errorCode {
                    case 5:
                        reason = "Access denied. Administrator privileges required."
                    case 2:
                        reason = "Physical drive not found. Invalid disk number."
                    case 32:
                        reason = "Disk is in use. Try ejecting the volume first."
                    default:
                        reason = "Failed to open disk device (error \(errorCode))."
                    }
                    throw DiskWriterError.writeFailed(reason: reason)
                }

                defer { CloseHandle(disk) }

                let imagePath = Array(imagePath.utf16) + [0]
                let image = CreateFileW(
                    imagePath,
                    GENERIC_READ,
                    UInt32(bitPattern: FILE_SHARE_READ),
                    nil,
                    UInt32(bitPattern: OPEN_EXISTING),
                    UInt32(bitPattern: FILE_FLAG_SEQUENTIAL_SCAN),
                    nil
                )
                guard let image, image != INVALID_HANDLE_VALUE else {
                    throw DiskWriterError.writeFailed(reason: "Failed to open image file.")
                }

                defer { CloseHandle(image) }

                // Use diskpart to offline and clean the disk - this removes all partitions including EFI
                do {
                    let diskpartScript = """
                        select disk \(diskNumber)
                        offline disk
                        online disk
                        """

                    let result = try await Subprocess.run(
                        Subprocess.Executable.name("diskpart.exe"),
                        arguments: [],
                        input: .string(diskpartScript),
                        output: .discarded,
                        error: .discarded
                    )

                    if !result.terminationStatus.isSuccess {
                        print("Warning: Failed to offline/online disk via diskpart")
                    }
                } catch {
                    print("Warning: Failed to run diskpart: \(error)")
                }

                // Try to lock the physical disk itself
                var bytesReturned: UInt32 = 0
                _ = DeviceIoControl(disk, FSCTL_LOCK_VOLUME, nil, 0, nil, 0, &bytesReturned, nil)
                _ = DeviceIoControl(
                    disk,
                    FSCTL_DISMOUNT_VOLUME,
                    nil,
                    0,
                    nil,
                    0,
                    &bytesReturned,
                    nil
                )
                defer {
                    _ = DeviceIoControl(
                        disk,
                        FSCTL_UNLOCK_VOLUME,
                        nil,
                        0,
                        nil,
                        0,
                        &bytesReturned,
                        nil
                    )
                }

                var size = LARGE_INTEGER()
                GetFileSizeEx(image, &size)
                guard SetFilePointerEx(disk, LARGE_INTEGER(), nil, UInt32(bitPattern: FILE_BEGIN))
                else {
                    throw DiskWriterError.writeFailed(reason: "Failed to set file pointer.")
                }

                let buffer = UnsafeMutableRawBufferPointer.allocate(
                    byteCount: 8 * 1024 * 1024,
                    alignment: 4096
                )
                defer { buffer.deallocate() }
                var totalWritten: Int64 = 0
                while true {
                    var bytesRead: UInt32 = 0
                    guard ReadFile(image, buffer.baseAddress, UInt32(buffer.count), &bytesRead, nil)
                    else {
                        let errorCode = GetLastError()
                        throw DiskWriterError.writeFailed(
                            reason: "Failed to read from image file (error \(errorCode))."
                        )
                    }
                    if bytesRead == 0 {
                        break
                    }

                    // For FILE_FLAG_NO_BUFFERING, writes must be sector-aligned
                    // Round up to next 512-byte boundary if not EOF
                    var writeSize = bytesRead
                    if writeSize % 512 != 0 {
                        writeSize = ((writeSize + 511) / 512) * 512
                        // Zero-fill the padding
                        if Int(writeSize) <= buffer.count {
                            buffer.baseAddress?.advanced(by: Int(bytesRead)).initializeMemory(
                                as: UInt8.self,
                                repeating: 0,
                                count: Int(writeSize - bytesRead)
                            )
                        }
                    }

                    var bytesWritten: UInt32 = 0
                    guard WriteFile(disk, buffer.baseAddress, writeSize, &bytesWritten, nil) else {
                        let errorCode = GetLastError()
                        switch errorCode {
                        case 5:
                            throw DiskWriterError.writeFailed(
                                reason: "Access denied writing to disk device."
                            )
                        default:
                            throw DiskWriterError.writeFailed(
                                reason:
                                    "Failed to write to disk device (error \(errorCode)). TotalWritten=\(totalWritten), writeSize=\(writeSize)."
                            )
                        }
                    }
                    totalWritten += Int64(bytesRead)  // Track actual data written, not padding
                    progressHandler(
                        DiskWriteProgress(
                            bytesWritten: totalWritten,
                            totalBytes: initialTotalBytes
                        )
                    )
                }

                // Flush and send final progress
                FlushFileBuffers(disk)

                // Send final progress update
                progressHandler(
                    DiskWriteProgress(
                        bytesWritten: initialTotalBytes,
                        totalBytes: initialTotalBytes
                    )
                )

            } catch let error as DiskWriterError {
                throw error
            } catch {
                throw DiskWriterError.writeFailed(reason: error.localizedDescription)
            }
        }

        public func writeFromZip(
            zipPath: String,
            drive: Drive,
            progressHandler: @escaping (DiskWriteProgress) -> Void
        ) async throws {
            // Windows does not currently support streaming from zip
            // The image must be extracted first, then written using write()
            throw DiskWriterError.writeFailed(
                reason:
                    "Writing directly from zip is not supported on Windows. Please extract the image first."
            )
        }
    }
#endif
