import Foundation

public enum WorldOperatorLaunchRequest {
    public static func recognizes(_ arguments: [String]) -> Bool {
        arguments.contains("--world-profile") || arguments.contains("--profile")
    }

    public static func parse(_ arguments: [String]) throws -> URL? {
        let flags = arguments.filter { $0 == "--world-profile" || $0 == "--profile" }
        guard !flags.isEmpty else { return nil }
        guard flags.count == 1, let index = arguments.firstIndex(of: flags[0]),
              index + 1 < arguments.count, arguments[index + 1].hasPrefix("/") else {
            throw InvalidProfile()
        }
        return URL(fileURLWithPath: arguments[index + 1])
    }

    public struct InvalidProfile: Error, LocalizedError {
        public var errorDescription: String? { "Choose one absolute World profile path with --world-profile." }
    }
}
