import Foundation

@main
struct Persistence {
    static func main() {
        let filePath = "/app/run_count.txt"
        var count = 0

        // Try to read the existing count from the file
        if let data = FileManager.default.contents(atPath: filePath),
            let content = String(data: data, encoding: .utf8),
            let existingCount = Int(content.trimmingCharacters(in: .whitespacesAndNewlines))
        {
            count = existingCount
        }

        // Increment the count
        count += 1

        // Write the new count back to the file
        let countString = "\(count)"
        do {
            try countString.write(toFile: filePath, atomically: true, encoding: .utf8)
            print("This program has been run \(count) time(s).")
        } catch {
            print("Failed to write count to file: \(error)")
        }
    }
}
