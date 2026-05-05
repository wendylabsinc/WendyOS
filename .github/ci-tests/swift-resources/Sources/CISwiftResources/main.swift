import Foundation

func fail(_ message: String) -> Never {
    FileHandle.standardError.write(Data((message + "\n").utf8))
    exit(1)
}

guard let resourceURL = Bundle.module.url(forResource: "greeting", withExtension: "txt") else {
    fail("FAIL: Bundle.module.url(forResource:withExtension:) returned nil — resource not synced")
}

let contents: String
do {
    contents = try String(contentsOf: resourceURL, encoding: .utf8)
} catch {
    fail("FAIL: Could not read resource file at \(resourceURL): \(error)")
}

let trimmed = contents.trimmingCharacters(in: .whitespacesAndNewlines)
guard trimmed == "Hello from a bundled SwiftPM resource!" else {
    fail("FAIL: Unexpected resource contents: \(trimmed)")
}

print("PASS: SwiftPM resource bundle synced and loaded successfully")
print("Resource contents: \(trimmed)")
