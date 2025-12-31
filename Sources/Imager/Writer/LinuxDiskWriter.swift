import Foundation
import Logging
import Subprocess

#if os(Linux)
    /// A disk writer implementation for Linux that uses the `dd` command.
    public class LinuxDiskWriter: DiskWriter {
        private let logger = Logger(label: "wendy.imager.linux-disk-writer")

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

            // Get image file size to track total progress
            // Correctly determine the image file size as Int64. `FileManager` returns `NSNumber`.
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
            progressHandler(DiskWriteProgress(bytesWritten: 0, totalBytes: totalBytes))

            do {
                // First, unmount any partitions on the disk to ensure it's not busy
                // Try to unmount all partitions (e.g., /dev/sdb1, /dev/sdb2, etc.)
                // We'll try to unmount the base device and any numbered partitions
                for partition in 0...15 {
                    let partitionPath = partition == 0 ? drive.id : "\(drive.id)\(partition)"

                    // Try to unmount, but don't fail if it's not mounted
                    _ = try await Subprocess.run(
                        Subprocess.Executable.name("sudo"),
                        arguments: ["umount", partitionPath],
                        output: .string(limit: .max),
                        error: .string(limit: .max)
                    )
                }

                // On Linux, dd with status=progress automatically outputs progress information
                logger.info("Writing image: \(imagePath) -> \(drive.id)")
                let script = """
                    dd if="\(imagePath)" of="\(drive.id)" bs=1M status=progress conv=fsync 2>&1
                    """

                // Store the progress handler in a local variable to avoid capturing it in the closure
                let localProgressHandler = progressHandler
                let localTotalBytes = totalBytes

                // Collect any error output for debugging
                var errorOutput = ""

                // Use the Subprocess API with a closure to capture output in real-time
                let result = try await Subprocess.run(
                    Subprocess.Executable.name("sudo"),
                    arguments: ["bash", "-c", script]
                ) { execution, stdin, stdout, stderr in
                    // The script redirects stderr to stdout with 2>&1, so all output comes via stdout
                    for try await chunk in stdout {
                        // Convert the chunk to a string
                        let outputString = chunk.withUnsafeBytes {
                            String(decoding: $0, as: UTF8.self)
                        }

                        // Check for error messages in the output
                        if outputString.lowercased().contains("error")
                            || outputString.lowercased().contains("permission denied")
                            || outputString.lowercased().contains("no space")
                        {
                            errorOutput += outputString
                        }

                        // Parse the progress information
                        // dd on Linux with status=progress outputs lines like:
                        // "1234567890 bytes (1.2 GB, 1.1 GiB) copied, 10 s, 123 MB/s"
                        // We look for all occurrences of byte counts in the output
                        let lines = outputString.split(separator: "\r").map { String($0) }

                        for line in lines {
                            if let bytes = parseBytesTransferred(from: line) {
                                let progress = DiskWriteProgress(
                                    bytesWritten: bytes,
                                    totalBytes: localTotalBytes
                                )
                                localProgressHandler(progress)
                            }
                        }
                    }

                    return execution
                }

                // Check if the command was successful
                if !result.terminationStatus.isSuccess {
                    let reason =
                        errorOutput.isEmpty
                        ? "dd command failed with status: \(result.terminationStatus)"
                        : "dd command failed: \(errorOutput.trimmingCharacters(in: .whitespacesAndNewlines))"
                    throw DiskWriterError.writeFailed(reason: reason)
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
#else
    // Empty implementation for non-Linux platforms
    public class LinuxDiskWriter: DiskWriter {
        public init() {}

        public func write(
            imagePath: String,
            drive: Drive,
            progressHandler: @escaping (DiskWriteProgress) -> Void
        ) async throws {
            fatalError("LinuxDiskWriter is only available on Linux platforms")
        }
    }
#endif
