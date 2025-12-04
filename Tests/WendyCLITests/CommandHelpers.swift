import ArgumentParser
import Foundation

struct InvalidArgumentsError: Error {}

extension AsyncParsableCommand {
    static func parse(_ arguments: [String]) throws -> Self {
        guard let result = try Self.parseAsRoot(arguments) as? Self else {
            throw InvalidArgumentsError()
        }
        return result
    }
}