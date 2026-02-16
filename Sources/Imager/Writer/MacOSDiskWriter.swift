#if os(macOS)
    import Foundation
    import Subprocess
    import NIOFileSystem

    /// A disk writer implementation for macOS that uses the `dd` command.
    public final class MacOSDiskWriter: DiskWriter {
        public init() {}

        /// Information about an image entry in a zip archive
        private struct ZipImageInfo {
            let entryName: String
            let uncompressedSize: Int64
        }

        /// Finds the .img entry in a zip archive and returns its name and uncompressed size
        private func findImageInZip(zipPath: String) async throws -> ZipImageInfo {
            let result = try await Subprocess.run(
                Subprocess.Executable.name("unzip"),
                arguments: ["-l", zipPath],
                output: .string(limit: .max),
                error: .string(limit: .max)
            )

            guard result.terminationStatus.isSuccess else {
                throw DiskWriterError.writeFailed(
                    reason: "Failed to list zip contents: \(result.standardError ?? "")"
                )
            }

            guard let output = result.standardOutput else {
                throw DiskWriterError.writeFailed(reason: "No output from unzip -l")
            }

            // Parse unzip -l output to find .img entry
            // Format: "  123456  mm-dd-yy  hh:mm   path/to/file.img"
            for line in output.split(separator: "\n") {
                let lineStr = String(line)
                guard lineStr.lowercased().contains(".img") else { continue }

                let parts = lineStr.split(separator: " ", omittingEmptySubsequences: true)
                guard parts.count >= 4,
                    let size = Int64(parts[0])
                else { continue }

                // Extract the filename (last part after spaces)
                if let nameStart = lineStr.range(of: " ", options: String.CompareOptions.backwards)?
                    .upperBound
                {
                    let name = String(lineStr[nameStart...]).trimmingCharacters(in: .whitespaces)
                    if name.lowercased().hasSuffix(".img") {
                        return ZipImageInfo(entryName: name, uncompressedSize: size)
                    }
                }
            }

            throw DiskWriterError.writeFailed(reason: "No .img file found in zip archive")
        }

        /// Unmounts the disk, attempting force unmount if normal unmount fails
        private func unmountDisk(devicePath: String) async throws {
            let unmountResult = try await Subprocess.run(
                Subprocess.Executable.name("sudo"),
                arguments: ["diskutil", "unmountDisk", devicePath],
                output: .string(limit: .max),
                error: .string(limit: .max)
            )

            if !unmountResult.terminationStatus.isSuccess {
                // Attempt a force unmount if the normal unmount fails
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
        }

        public func writeFromZip(
            zipPath: String,
            drive: Drive,
            progressHandler: @escaping (DiskWriteProgress) -> Void
        ) async throws {
            // Check if zip exists
            guard FileManager.default.fileExists(atPath: zipPath) else {
                throw DiskWriterError.imageNotFoundInPath(path: zipPath)
            }

            // Find the .img entry in the zip
            let imageInfo = try await findImageInZip(zipPath: zipPath)
            let totalBytes = imageInfo.uncompressedSize

            // Send initial progress update
            progressHandler(DiskWriteProgress(bytesWritten: 0, totalBytes: totalBytes))

            // Ensure drive ID is properly formatted with /dev/ prefix
            let devicePath: String
            if drive.id.hasPrefix("/dev/") {
                devicePath = drive.id
            } else {
                devicePath = "/dev/\(drive.id)"
            }

            // Use raw disk device for faster access
            let rawDevicePath = devicePath.replacingOccurrences(of: "/dev/disk", with: "/dev/rdisk")

            // Unmount the disk
            try await unmountDisk(devicePath: devicePath)

            // Stream unzip directly to dd using a shell pipe
            // unzip -p extracts to stdout, which we pipe to dd
            let script = """
                /usr/bin/unzip -p '\(zipPath)' '\(imageInfo.entryName)' | dd of='\(rawDevicePath)' bs=4m
                """

            // We need to track progress. Since we're piping, we can't easily get byte counts.
            // Instead, we'll use a timer-based estimator that advances progress over time.
            let estimatorQueue = DispatchQueue(label: "wendy.write.progress")
            let estimator = DispatchSource.makeTimerSource(queue: estimatorQueue)
            let startTime = Date()

            // Estimate write speed based on typical SD card/USB speeds (~30-50 MB/s)
            // We'll assume ~40 MB/s average and adjust the progress curve
            let estimatedWriteSpeed: Double = 40 * 1024 * 1024  // 40 MB/s in bytes
            let estimatedDuration = Double(totalBytes) / estimatedWriteSpeed

            estimator.schedule(deadline: .now() + 0.5, repeating: 0.5)
            estimator.setEventHandler {
                let elapsed = Date().timeIntervalSince(startTime)
                // Use an asymptotic curve that approaches 95% over the estimated duration
                let progress = min(0.95, elapsed / estimatedDuration * 0.95)
                let estimatedBytes = Int64(Double(totalBytes) * progress)
                progressHandler(
                    DiskWriteProgress(bytesWritten: estimatedBytes, totalBytes: totalBytes)
                )
            }
            estimator.resume()

            defer { estimator.cancel() }

            do {
                let result = try await Subprocess.run(
                    Subprocess.Executable.name("sudo"),
                    arguments: ["bash", "-c", script],
                    output: .discarded,
                    error: .string(limit: .max)
                )

                if !result.terminationStatus.isSuccess {
                    throw DiskWriterError.writeFailed(
                        reason: "Write failed: \(result.standardError ?? "Unknown error")"
                    )
                }

                // Send final 100% progress
                progressHandler(DiskWriteProgress(bytesWritten: totalBytes, totalBytes: totalBytes))
            } catch let error as DiskWriterError {
                throw error
            } catch {
                throw DiskWriterError.writeFailed(reason: "Error: \(error.localizedDescription)")
            }
        }

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
                    try await FileSystem.shared.withFileHandle(forReadingAt: FilePath(imagePath)) {
                        handle in
                        var reader = handle.bufferedReader()
                        var totalWritten: Int64 = 0
                        let totalBytes = totalBytes  // capture

                        while true {
                            try Task.checkCancellation()
                            var chunk = try await reader.read(.bytes(4 * 1024 * 1024))  // 4 MiB
                            if chunk.readableBytes == 0 {
                                return execution
                            }

                            totalWritten += try await Int64(
                                stdinWriter.write(chunk.readBytes(length: chunk.readableBytes)!)
                            )
                            if let totalBytes, totalBytes > 0 {
                                progressHandler(
                                    DiskWriteProgress(
                                        bytesWritten: min(totalWritten, totalBytes),
                                        totalBytes: totalBytes
                                    )
                                )
                            }
                        }
                    }
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
