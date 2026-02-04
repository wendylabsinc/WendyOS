import Foundation

enum FindExecutableError: Error {
    case executableNotFound(String)
}

public func findExecutable(name: String, standardPath: String) throws -> String {
    #if os(Windows)
        // On Windows, check common locations
        let windowsPaths = [
            "C:\\Windows\\System32\\\(name).exe",
            "C:\\Windows\\\(name).exe",
            standardPath,
        ]

        for path in windowsPaths {
            if FileManager.default.fileExists(atPath: path) {
                return path
            }
        }

        // Try using 'where' command on Windows (equivalent to 'which')
        let whereProc = Process()
        whereProc.executableURL = URL(fileURLWithPath: "C:\\Windows\\System32\\where.exe")
        whereProc.arguments = [name]

        let outputPipe = Pipe()
        whereProc.standardOutput = outputPipe
        whereProc.standardError = Pipe()

        try? whereProc.run()
        whereProc.waitUntilExit()

        if whereProc.terminationStatus == 0 {
            let outputData = outputPipe.fileHandleForReading.readDataToEndOfFile()
            if let path = String(data: outputData, encoding: .utf8)?
                .split(separator: "\r\n")
                .first?
                .trimmingCharacters(in: .whitespaces)
            {
                return String(path)
            }
        }

        throw FindExecutableError.executableNotFound(name)
    #else
        // Check if unzip is available at the standard locations
        var standardPath = standardPath
        if FileManager.default.fileExists(atPath: standardPath) {
            return standardPath
        }

        standardPath = "/bin/\(name)"
        if FileManager.default.fileExists(atPath: standardPath) {
            return standardPath
        }

        // Try to find unzip in PATH
        let whichUnzip = Process()
        whichUnzip.executableURL = URL(fileURLWithPath: "/bin/sh")
        whichUnzip.arguments = ["-c", "which \(name)"]

        let outputPipe = Pipe()
        whichUnzip.standardOutput = outputPipe

        try whichUnzip.run()
        whichUnzip.waitUntilExit()

        if whichUnzip.terminationStatus == 0 {
            let outputData = outputPipe.fileHandleForReading.readDataToEndOfFile()
            if let path = String(data: outputData, encoding: .utf8)?.trimmingCharacters(
                in: .whitespacesAndNewlines
            ) {
                return path
            }
        }

        throw FindExecutableError.executableNotFound(name)
    #endif
}
