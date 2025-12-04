import Testing

@testable import Analytics

// MARK: - Test Error Types

enum TestError: Error {
    case simpleError
    case errorWithPath(String)
    case errorWithHostname(String)
    case errorWithEmail(String)
    case complexError(path: String, host: String)
}

struct CustomError: Error {
    let message: String
}

// MARK: - Error Type Extraction Tests

@Test("Sanitize simple error")
func sanitizeSimpleError() {
    let error = TestError.simpleError
    let sanitized = ErrorSanitizer.sanitize(error)

    #expect(sanitized.name == "simpleError")
    #expect(sanitized.type.contains("TestError"))
    // Domain is the module name if available, otherwise the type name
    #expect(sanitized.domain == "AnalyticsTests" || sanitized.domain == "TestError")
}

@Test("Sanitize error with associated value")
func sanitizeErrorWithAssociatedValue() {
    let error = TestError.errorWithPath("/Users/test/file.txt")
    let sanitized = ErrorSanitizer.sanitize(error)

    // Should extract just the case name, not the associated value
    #expect(sanitized.name == "errorWithPath")
    #expect(!sanitized.name.contains("/Users"))
}

@Test("Sanitize custom error")
func sanitizeCustomError() {
    let error = CustomError(message: "Something went wrong at /home/user/project")
    let sanitized = ErrorSanitizer.sanitize(error)

    // For struct errors, the name is the type name
    #expect(sanitized.name.contains("CustomError"))
    // Domain is the module name if available, otherwise the type name
    #expect(sanitized.domain == "AnalyticsTests" || sanitized.domain == "CustomError")
}

// MARK: - String Sanitization Tests

@Test(
    "Sanitize file paths",
    arguments: [
        ("/Users/john/Documents/project/file.txt", "[PATH]"),
        ("Failed at /home/ubuntu/app/main.swift", "Failed at [PATH]"),
        ("Path: ~/Documents/secret.txt", "Path: [PATH]"),
        ("C:\\Users\\Admin\\Desktop\\file.txt", "[PATH]"),
        ("./relative/path/to/file", "[PATH]"),
        ("Multiple paths: /path1 and /path2", "Multiple paths: [PATH] and [PATH]"),
    ]
)
func sanitizeFilePaths(input: String, expected: String) {
    let sanitized = ErrorSanitizer.sanitizeString(input)
    #expect(sanitized == expected)
}

@Test(
    "Sanitize hostnames",
    arguments: [
        ("192.168.1.1", "[HOSTNAME]"),
        ("Connect to 10.0.0.1:8080", "Connect to [HOSTNAME]:8080"),
        ("device.local", "[HOSTNAME]"),
        ("raspberry-pi.local", "[HOSTNAME]"),
        ("my-device.home", "[HOSTNAME]"),
        ("server.internal", "[HOSTNAME]"),
        ("device-abc123def.lan", "[HOSTNAME]"),
    ]
)
func sanitizeHostnames(input: String, expected: String) {
    let sanitized = ErrorSanitizer.sanitizeString(input)
    #expect(sanitized == expected)
}

@Test(
    "Sanitize email addresses",
    arguments: [
        ("user@example.com", "[EMAIL]"),
        ("Contact john.doe@company.org for help", "Contact [EMAIL] for help"),
        ("admin+test@domain.co.uk", "[EMAIL]"),
    ]
)
func sanitizeEmails(input: String, expected: String) {
    let sanitized = ErrorSanitizer.sanitizeString(input)
    #expect(sanitized == expected)
}

@Test(
    "Sanitize URLs",
    arguments: [
        ("https://example.com/api/endpoint", "[URL]"),
        ("Failed to connect to http://localhost:3000", "Failed to connect to [URL]"),
        ("URL: https://api.service.com/v1/users?id=123", "URL: [URL]"),
    ]
)
func sanitizeURLs(input: String, expected: String) {
    let sanitized = ErrorSanitizer.sanitizeString(input)
    #expect(sanitized == expected)
}

@Test("Sanitize complex string with multiple patterns")
func sanitizeComplexString() {
    let input = """
        Error occurred at /Users/john/project/main.swift
        Failed to connect to 192.168.1.100
        Please contact admin@example.com
        Check https://docs.example.com/help
        Device raspberry-pi.local not found
        """

    let sanitized = ErrorSanitizer.sanitizeString(input)

    #expect(!sanitized.contains("/Users"))
    #expect(!sanitized.contains("192.168"))
    #expect(!sanitized.contains("admin@"))
    #expect(!sanitized.contains("https://"))
    #expect(!sanitized.contains("raspberry-pi.local"))

    #expect(sanitized.contains("[PATH]"))
    #expect(sanitized.contains("[HOSTNAME]"))
    #expect(sanitized.contains("[EMAIL]"))
    #expect(sanitized.contains("[URL]"))
}

@Test(
    "Safe strings should not be modified",
    arguments: [
        "Simple error message",
        "Connection failed",
        "Invalid argument",
        "Timeout occurred after 30 seconds",
        "Port 8080 is already in use",
    ]
)
func sanitizeSafeStrings(input: String) {
    let sanitized = ErrorSanitizer.sanitizeString(input)
    #expect(sanitized == input)
}
