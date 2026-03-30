// StdinEcho: integration-test helper app.
//
// Behaviour:
//   - Prints "ready" on startup so tests can synchronise on container start.
//   - Reads stdin line by line and echoes each line back prefixed with "echo: ".
//   - Supports a small set of control commands:
//       exit <code>  – exit with the given integer status code
//       stderr <msg> – write <msg> to stderr
//   - Exits normally (code 0) when stdin is closed (EOF).

import Foundation

let stdoutHandle = FileHandle.standardOutput
let stderrHandle = FileHandle.standardError

func writeStdout(_ s: String) {
    stdoutHandle.write(Data((s + "\n").utf8))
}

func writeStderr(_ s: String) {
    stderrHandle.write(Data((s + "\n").utf8))
}

writeStdout("ready")

while let line = readLine() {
    let trimmed = line.trimmingCharacters(in: .whitespaces)

    if trimmed.hasPrefix("exit "), let code = Int(trimmed.dropFirst(5).trimmingCharacters(in: .whitespaces)) {
        writeStdout("exiting with code \(code)")
        exit(Int32(code))
    } else if trimmed.hasPrefix("stderr ") {
        writeStderr(String(trimmed.dropFirst(7)))
    } else {
        writeStdout("echo: \(line)")
    }
}
