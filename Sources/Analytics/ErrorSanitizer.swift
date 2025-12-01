import Foundation

/// Represents a sanitized error safe for analytics tracking
public struct SanitizedError: Sendable {
    /// The error type (e.g., "RunCommand.Error")
    public let type: String

    /// The error name (e.g., "noExecutableTarget")
    public let name: String

    /// The error domain (e.g., "RunCommand")
    public let domain: String
}

/// Sanitizes errors to remove sensitive information before analytics tracking
public struct ErrorSanitizer: Sendable {
    /// Regular expression to match file paths
    private static let pathPattern = try! NSRegularExpression(
        pattern: #"(/[^\s]+|~[^\s]+|[A-Z]:\\[^\s]+|\./[^\s]+)"#,
        options: []
    )

    /// Regular expression to match IP addresses and hostnames
    private static let hostnamePattern = try! NSRegularExpression(
        pattern:
            #"\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b|\b[a-z0-9-]+\.(local|lan|home|internal)\b|\b[a-z0-9-]+-[0-9a-f]{4,}\b"#,
        options: [.caseInsensitive]
    )

    /// Regular expression to match email addresses
    private static let emailPattern = try! NSRegularExpression(
        pattern: #"\b[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Z|a-z]{2,}\b"#,
        options: []
    )

    /// Regular expression to match URLs
    private static let urlPattern = try! NSRegularExpression(
        pattern: #"https?://[^\s]+"#,
        options: []
    )

    /// Sanitizes an error for safe analytics tracking
    /// - Parameter error: The error to sanitize
    /// - Returns: A sanitized error with no sensitive information
    public static func sanitize(_ error: Error) -> SanitizedError {
        let errorType = String(describing: type(of: error))

        // Get the error description which includes the case name for enums
        let errorDescription = String(describing: error)

        // Extract error name from description
        let errorName = sanitizeName(errorDescription)

        // Extract domain from the full type path
        let typeComponents = errorType.components(separatedBy: ".")

        // For nested types, get the module
        // For simple types, use the type itself
        let domain = typeComponents.count > 1 ? typeComponents.first! : errorType

        return SanitizedError(
            type: errorType,
            name: errorName,
            domain: domain
        )
    }

    /// Sanitizes an error name by removing sensitive patterns
    private static func sanitizeName(_ name: String) -> String {
        // For enum case names with associated values, extract just the case name
        if let caseNameEnd = name.firstIndex(of: "(") {
            return String(name[..<caseNameEnd])
        }
        return name
    }

    /// Sanitizes a string by removing sensitive information
    /// - Parameter string: The string to sanitize
    /// - Returns: A sanitized string with sensitive data replaced
    public static func sanitizeString(_ string: String) -> String {
        var sanitized = string

        // Remove URLs first (before paths, since URLs contain paths)
        sanitized = urlPattern.stringByReplacingMatches(
            in: sanitized,
            range: NSRange(sanitized.startIndex..., in: sanitized),
            withTemplate: "[URL]"
        )

        // Remove file paths
        sanitized = pathPattern.stringByReplacingMatches(
            in: sanitized,
            range: NSRange(sanitized.startIndex..., in: sanitized),
            withTemplate: "[PATH]"
        )

        // Remove hostnames/IPs
        sanitized = hostnamePattern.stringByReplacingMatches(
            in: sanitized,
            range: NSRange(sanitized.startIndex..., in: sanitized),
            withTemplate: "[HOSTNAME]"
        )

        // Remove email addresses
        sanitized = emailPattern.stringByReplacingMatches(
            in: sanitized,
            range: NSRange(sanitized.startIndex..., in: sanitized),
            withTemplate: "[EMAIL]"
        )

        return sanitized
    }
}
