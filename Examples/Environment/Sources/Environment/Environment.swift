import Foundation

@main
struct Environment {
    static func main() {
        for (key, value) in ProcessInfo.processInfo.environment {
            print("\(key): \(value)")
        }
    }
}
