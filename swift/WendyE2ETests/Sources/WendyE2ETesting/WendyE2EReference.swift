import Foundation

public enum WendyE2EReference {
    public struct Document: Sendable, Equatable {
        public var title: String
        public var overview: String
        public var sections: [Section]
        public var sourceLocation: SourceLocation

        public init(
            title: String,
            overview: String,
            sections: [Section],
            sourceLocation: SourceLocation
        ) {
            self.title = title
            self.overview = overview
            self.sections = sections
            self.sourceLocation = sourceLocation
        }
    }

    public struct Section: Sendable, Equatable {
        public var title: String
        public var entries: [Entry]

        public init(title: String, entries: [Entry] = []) {
            self.title = title
            self.entries = entries
        }
    }

    /// Static trait information, not the result of running a test or its conditions.
    public enum Availability: String, Sendable, Equatable, Encodable {
        case enabled
        case disabled
        case conditional
    }

    public struct Entry: Sendable, Equatable {
        public var title: String
        public var documentation: String
        public var sourceLocation: SourceLocation
        public var availability: Availability
        /// Source spelling of availability traits. Expressions are never evaluated.
        public var availabilityReason: String?
        /// Exact attribute/function source, bounded by the reference lexer.
        public var declarationSource = ""
        public var declarationStartLine = 0
        public var declarationEndLine = 0
        public var change: Change?

        /// Compatibility view: true only for unconditional disabling traits.
        /// False does not imply enabled; inspect `availability` for conditional tests.
        public var isDisabled: Bool {
            get { availability == .disabled }
            set {
                availability = newValue ? .disabled : .enabled
                availabilityReason = nil
            }
        }

        public init(
            title: String,
            documentation: String,
            sourceLocation: SourceLocation,
            isDisabled: Bool
        ) {
            self.title = title
            self.documentation = documentation
            self.sourceLocation = sourceLocation
            self.availability = isDisabled ? .disabled : .enabled
            self.availabilityReason = nil
        }

        public init(
            title: String,
            documentation: String,
            sourceLocation: SourceLocation,
            availability: Availability,
            availabilityReason: String? = nil
        ) {
            self.title = title
            self.documentation = documentation
            self.sourceLocation = sourceLocation
            self.availability = availability
            self.availabilityReason = availabilityReason
        }
    }

    public enum Change: String, Sendable {
        case new = "NEW"
        case modified = "MODIFIED"
    }

    public struct SourceLocation: Sendable, Equatable, Encodable {
        public var path: String
        public var line: Int

        public init(path: String, line: Int) {
            self.path = path
            self.line = line
        }
    }

    public struct RenderOptions: Sendable, Equatable {
        public var includeSourceLocations: Bool
        public var includeDisabledState: Bool

        public init(
            includeSourceLocations: Bool,
            includeDisabledState: Bool
        ) {
            self.includeSourceLocations = includeSourceLocations
            self.includeDisabledState = includeDisabledState
        }

        public static let reference = RenderOptions(
            includeSourceLocations: false,
            includeDisabledState: false
        )

        public static let specReview = RenderOptions(
            includeSourceLocations: true,
            includeDisabledState: true
        )
    }

    public typealias MarkdownOptions = RenderOptions

    public struct IndexEntry: Sendable, Equatable {
        public var title: String
        public var fileName: String
        public var anchor: String?

        public init(title: String, fileName: String, anchor: String? = nil) {
            self.title = title
            self.fileName = fileName
            self.anchor = anchor
        }
    }

    public enum Error: Swift.Error, Equatable, CustomStringConvertible {
        case fileNotFound(String)
        case directoryNotFound(String)
        case unreadableDirectory(String)
        case unterminatedAttribute(path: String, line: Int)
        case ambiguousSpec(String)

        public var description: String {
            switch self {
            case .fileNotFound(let path):
                "reference source file not found: \(path)"
            case .directoryNotFound(let path):
                "reference source directory not found: \(path)"
            case .unreadableDirectory(let path):
                "reference source directory cannot be read: \(path)"
            case .unterminatedAttribute(let path, let line):
                "\(path):\(line): unterminated reference attribute"
            case .ambiguousSpec(let identity):
                "ambiguous reference spec identity: \(identity)"
            }
        }
    }

    // MARK: - Parsing Reference Documents

    public static func parseFile(at path: String) throws -> [Document] {
        let url = URL(fileURLWithPath: path)
        guard FileManager.default.fileExists(atPath: url.path) else {
            throw Error.fileNotFound(path)
        }

        let source = try String(contentsOf: url, encoding: .utf8)
        return try parseValidatedSource(source, path: path)
    }

    public static func parseValidatedSource(
        _ source: String,
        path: String = "<memory>"
    ) throws -> [Document] {
        var parser = ReferenceSourceParser(source: source, path: path)
        let documents = parser.parse()
        if let error = parser.parsingError { throw error }
        return documents
    }

    public static func parseDirectory(at path: String) throws -> [Document] {
        var isDirectory: ObjCBool = false
        guard FileManager.default.fileExists(atPath: path, isDirectory: &isDirectory),
            isDirectory.boolValue
        else {
            throw Error.directoryNotFound(path)
        }

        guard let enumerator = FileManager.default.enumerator(atPath: path) else {
            throw Error.unreadableDirectory(path)
        }

        let sourceFilePaths = enumerator.compactMap { element -> String? in
            guard let relativePath = element as? String, relativePath.hasSuffix(".swift") else {
                return nil
            }
            return URL(fileURLWithPath: path).appendingPathComponent(relativePath).path
        }.sorted()

        var documents: [Document] = []
        for sourceFilePath in sourceFilePaths {
            documents.append(contentsOf: try parseFile(at: sourceFilePath))
        }
        return documents
    }

    /// Returns no documents when a recognized attribute is unterminated.
    /// Use `parseFile(at:)` for a throwing path-and-line diagnostic.
    public static func parseSource(_ source: String, path: String = "<memory>") -> [Document] {
        var parser = ReferenceSourceParser(source: source, path: path)
        return parser.parse()
    }
}
