#if os(Windows)
    import Foundation
    import Subprocess

    /// Windows implementation of the DiskWriter protocol using PowerShell and direct file I/O.
    public class WindowsDiskWriter: DiskWriter {
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
                // Use PowerShell to write the image using direct file I/O
                // This approach reads the image and writes directly to the physical disk device
                let escapedImagePath = imagePath.replacingOccurrences(of: "\\", with: "\\\\")
                
                let powerShellScript = #"""
                    $imagePath = '\#(escapedImagePath)'
                    $diskNumber = \#(diskNumber)
                    $diskPath = '\\\\\.\\PhysicalDrive$diskNumber'
                    $chunkSize = 4MB
                    
                    try {
                        # Open the image file for reading
                        $imageFile = [System.IO.File]::OpenRead($imagePath)
                        $totalBytes = $imageFile.Length
                        
                        # Open the disk for writing (requires admin)
                        $diskFile = [System.IO.File]::OpenWrite($diskPath)
                        
                        # Read and write in chunks
                        $buffer = New-Object byte[] $chunkSize
                        $bytesWritten = 0
                        
                        while ($true) {
                            $bytesRead = $imageFile.Read($buffer, 0, $chunkSize)
                            if ($bytesRead -eq 0) { break }
                            
                            $diskFile.Write($buffer, 0, $bytesRead)
                            $bytesWritten += $bytesRead
                        }
                        
                        $diskFile.Flush()
                        $diskFile.Close()
                        $imageFile.Close()
                        
                        exit 0
                    } catch {
                        Write-Error $_.Exception.Message
                        exit 1
                    }
                    """#

                let result = try await Subprocess.run(
                    Subprocess.Executable.name("powershell.exe"),
                    arguments: ["-NoProfile", "-Command", powerShellScript],
                    output: .discarded,
                    error: .string(limit: .max)
                )

                if !result.terminationStatus.isSuccess {
                    let stderr = result.standardError ?? "Unknown error"
                    throw DiskWriterError.writeFailed(reason: stderr)
                }

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
    }
#endif
