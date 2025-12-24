#if os(macOS)
    import Foundation
    import Subprocess

    /// A disk writer implementation for macOS that uses the `dd` command.
    public class MacOSDiskWriter: DiskWriter {
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

            // Correctly determine the image file size as Int64. `FileManager` returns `NSNumber`,
            // so we need to bridge it instead of casting directly to `Int64`.
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

            // Ensure drive ID is properly formatted with /dev/ prefix
            let devicePath: String
            if drive.id.hasPrefix("/dev/") {
                devicePath = drive.id
            } else {
                devicePath = "/dev/\(drive.id)"
            }

            // Use raw disk device for faster access
            let rawDevicePath = devicePath.replacingOccurrences(of: "/dev/disk", with: "/dev/rdisk")

            do {
                // First, unmount the disk to ensure it's not busy
                let unmountResult = try await Subprocess.run(
                    Subprocess.Executable.name("sudo"),
                    arguments: ["diskutil", "unmountDisk", devicePath],
                    output: .string(limit: .max),
                    error: .string(limit: .max)
                )

                if !unmountResult.terminationStatus.isSuccess {
                    // Attempt a force unmount if the normal unmount fails (common when Finder or Spotlight holds a handle)
                    let forceResult = try await Subprocess.run(
                        Subprocess.Executable.name("sudo"),
                        arguments: ["diskutil", "unmountDisk", "force", devicePath],
                        output: .string(limit: .max),
                        error: .string(limit: .max)
                    )

                    if !forceResult.terminationStatus.isSuccess {
                        let stderr = [unmountResult.standardError, forceResult.standardError]
                            .compactMap { $0 }
                            .joined(separator: "\n")
                            .trimmingCharacters(in: .whitespacesAndNewlines)

                        let hint =
                            "Hint: Close Finder windows, Disk Utility, or any apps using the disk, then retry."

                        if !stderr.isEmpty {
                            throw DiskWriterError.writeFailed(
                                reason: "Failed to unmount disk. \(stderr)\n\(hint)"
                            )
                        } else {
                            throw DiskWriterError.writeFailed(
                                reason:
                                    "Failed to unmount disk (normal and force). Status: \(forceResult.terminationStatus).\n\(hint)"
                            )
                        }
                    }
                }

                // Stream the image to dd via stdin. We count bytes written ourselves for progress.
                // Use a larger block size on the dd side to improve throughput, and avoid conv=sync
                // which can slow writes and pad short reads when stdin is a pipe.
                let chunkSize = 4 * 1024 * 1024  // 4 MiB
                let result = try await Subprocess.run(
                    Subprocess.Executable.name("sudo"),
                    arguments: [
                        "dd",
                        "of=\(rawDevicePath)",
                        "bs=4m",
                    ],
                    error: .discarded,
                    preferredBufferSize: nil
                ) { execution, stdinWriter, _ in
                    // Feed file chunks to dd's stdin and report progress
                    let fileURL = URL(fileURLWithPath: imagePath)
                    guard let handle = try? FileHandle(forReadingFrom: fileURL) else {
                        throw DiskWriterError.imageNotFoundInPath(path: imagePath)
                    }
                    defer { try? handle.close() }

                    var totalWritten: Int64 = 0
                    let totalBytes = totalBytes  // capture

                    while true {
                        // Read next chunk synchronously
                        let data = try? handle.read(upToCount: chunkSize)
                        guard let data, !data.isEmpty else { break }

                        // Write to dd's stdin (async) and propagate any write error
                        do {
                            totalWritten += try await Int64(stdinWriter.write(data.bytes))
                        } catch {
                            throw DiskWriterError.writeFailed(
                                reason: "Failed piping data to dd: \(error.localizedDescription)"
                            )
                        }
                        if let totalBytes, totalBytes > 0 {
                            progressHandler(
                                DiskWriteProgress(
                                    bytesWritten: min(totalWritten, totalBytes),
                                    totalBytes: totalBytes
                                )
                            )
                        }
                    }
                    // Signal EOF to dd
                    try? await stdinWriter.finish()
                    return execution
                }

                if !result.terminationStatus.isSuccess {
                    throw DiskWriterError.writeFailed(
                        reason: "dd command failed with status: \(result.terminationStatus)"
                    )
                }

                // If we get here, the command completed successfully
                // Send a final progress update showing 100% completion
                if let totalBytes = totalBytes {
                    // Ensure we show exactly 100% by setting bytesWritten = totalBytes
                    let finalProgress = DiskWriteProgress(
                        bytesWritten: totalBytes,
                        totalBytes: totalBytes
                    )
                    progressHandler(finalProgress)
                } else {
                    progressHandler(
                        DiskWriteProgress(
                            bytesWritten: Int64(100),
                            totalBytes: Int64(100)
                        )
                    )
                }
            } catch let error as DiskWriterError {
                // Re-throw DiskWriterError
                throw error
            } catch {
                // Convert other errors to DiskWriterError with detailed message
                throw DiskWriterError.writeFailed(reason: "Error: \(error.localizedDescription)")
            }
        }
    }
#endif
