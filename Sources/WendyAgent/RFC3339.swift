import Foundation

extension Date {
    func rfc3339Formatted() -> String {
        return ISO8601DateFormatter().string(from: self)
    }
}
