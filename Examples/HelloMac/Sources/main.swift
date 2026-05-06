import Foundation

guard let resourceURL = Bundle.module.url(forResource: "Example", withExtension: "txt") else {
    fatalError("Missing bundled resource: Example.txt")
}

let resourceContents = try String(contentsOf: resourceURL, encoding: .utf8)
try FileHandle.standardOutput.write(
    contentsOf: Data("Resources work too, here is the contents of the text file:\n\(resourceContents)\n".utf8)
)

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
