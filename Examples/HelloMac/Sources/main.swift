import Foundation

@main
struct HelloMac {
    static func main() async throws {
        var i = 0
        while true {
            try FileHandle.standardOutput.write(
                contentsOf: Data("[\(Date())] Hello from Mac (stdout) #\(i)\n".utf8)
            )
            try FileHandle.standardError.write(
                contentsOf: Data("[\(Date())] Hello from Mac (stderr) #\(i)\n".utf8)
            )
            i += 1
            try await Task.sleep(for: .seconds(2))
        }
    }
}
