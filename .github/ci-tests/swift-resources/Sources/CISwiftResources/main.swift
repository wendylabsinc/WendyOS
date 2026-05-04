import Foundation

guard let resourceURL = Bundle.module.url(forResource: "greeting", withExtension: "txt") else {
    fputs("FAIL: Bundle.module.url(forResource:withExtension:) returned nil — resource not synced\n", stderr)
    exit(1)
}

let contents: String
do {
    contents = try String(contentsOf: resourceURL, encoding: .utf8)
} catch {
    fputs("FAIL: Could not read resource file at \(resourceURL): \(error)\n", stderr)
    exit(1)
}

let trimmed = contents.trimmingCharacters(in: .whitespacesAndNewlines)
guard trimmed == "Hello from a bundled SwiftPM resource!" else {
    fputs("FAIL: Unexpected resource contents: \(trimmed)\n", stderr)
    exit(1)
}

print("PASS: SwiftPM resource bundle synced and loaded successfully")
print("Resource contents: \(trimmed)")
