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

    public struct Entry: Sendable, Equatable {
        public var title: String
        public var documentation: String
        public var sourceLocation: SourceLocation
        public var isDisabled: Bool

        public init(
            title: String,
            documentation: String,
            sourceLocation: SourceLocation,
            isDisabled: Bool
        ) {
            self.title = title
            self.documentation = documentation
            self.sourceLocation = sourceLocation
            self.isDisabled = isDisabled
        }
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

        public var description: String {
            switch self {
            case .fileNotFound(let path):
                "reference source file not found: \(path)"
            case .directoryNotFound(let path):
                "reference source directory not found: \(path)"
            case .unreadableDirectory(let path):
                "reference source directory cannot be read: \(path)"
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
        return parseSource(source, path: path)
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

    public static func parseSource(_ source: String, path: String = "<memory>") -> [Document] {
        var parser = ReferenceSourceParser(source: source, path: path)
        return parser.parse()
    }
}
