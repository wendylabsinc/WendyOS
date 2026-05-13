import ArgumentParser
import Foundation
import WendyE2ETesting

@main
struct SwiftE2ETestingCLI: ParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "swift-e2e-testing",
        abstract: "Utilities for Swift E2E behavioral specs.",
        subcommands: [ReferenceCommand.self, ReportCommand.self]
    )
}

struct ReferenceCommand: ParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "reference",
        abstract: "Generate reference documentation from Swift E2E tests.",
        discussion: """
            Each path may be a Swift test file or a directory containing Swift test files.
            Without --output, generated documentation is written to stdout. With --output,
            one document is written per Swift test file, along with an index.
            """
    )

    @Flag(help: "Include source file and line metadata.")
    var includeSourceLocations = false

    @Flag(help: "Include enabled/disabled test metadata.")
    var includeDisabledState = false

    @Flag(help: "Include source locations and disabled state.")
    var specReview = false

    @Option(name: .long, help: "Output format: markdown, md, html, or json.")
    var format: ReferenceFormat = .markdown

    @Option(name: [.short, .long], help: "Write generated files and index to this directory.")
    var output: String?

    @Argument(help: "Swift test files or directories containing Swift test files.")
    var paths: [String] = []

    mutating func run() throws {
        guard !paths.isEmpty else {
            throw ValidationError("Missing path.")
        }

        let options = Reference.MarkdownOptions(
            includeSourceLocations: includeSourceLocations || specReview,
            includeDisabledState: includeDisabledState || specReview
        )
        try validateFormatOption()
        let sourceFiles = try paths.flatMap(referenceSourceFiles)

        if let output {
            let fileCount = try writeReferenceFiles(
                sourceFiles: sourceFiles,
                outputDirectory: output,
                format: format,
                options: options
            )
            print("Wrote \(fileCount) \(format.description) reference file(s) to \(output)")
            return
        }

        var documents: [Reference.Document] = []
        for sourceFile in sourceFiles {
            documents.append(contentsOf: try Reference.parseFile(at: sourceFile))
        }
        print(try format.render(documents, options: options), terminator: "")
    }

    private func validateFormatOption() throws {
        let count = CommandLine.arguments.filter { argument in
            argument == "--format" || argument.hasPrefix("--format=")
        }.count
        guard count <= 1 else {
            throw ValidationError("Specify --format only once.")
        }
    }
}

enum ReferenceFormat: String, ExpressibleByArgument, CustomStringConvertible {
    case markdown
    case html
    case json

    init?(argument: String) {
        switch argument.lowercased() {
        case "markdown", "md":
            self = .markdown
        case "html":
            self = .html
        case "json":
            self = .json
        default:
            return nil
        }
    }

    var description: String {
        switch self {
        case .markdown:
            "Markdown"
        case .html:
            "HTML"
        case .json:
            "JSON"
        }
    }

    var indexFileName: String {
        switch self {
        case .markdown:
            "index.md"
        case .html:
            "index.html"
        case .json:
            "index.json"
        }
    }

    func fileName(forTitle title: String) -> String {
        switch self {
        case .markdown:
            Reference.markdownFileName(forTitle: title)
        case .html:
            Reference.htmlFileName(forTitle: title)
        case .json:
            Reference.jsonFileName(forTitle: title)
        }
    }

    func render(
        _ documents: [Reference.Document],
        options: Reference.MarkdownOptions
    ) throws -> String {
        switch self {
        case .markdown:
            Reference.renderMarkdown(documents, options: options)
        case .html:
            Reference.renderHTML(documents, options: options)
        case .json:
            try Reference.renderJSON(documents, options: options)
        }
    }

    func renderIndex(_ entries: [Reference.IndexEntry], title: String) throws -> String {
        switch self {
        case .markdown:
            Reference.renderMarkdownIndex(entries, title: title)
        case .html:
            Reference.renderHTMLIndex(entries, title: title)
        case .json:
            try Reference.renderJSONIndex(entries, title: title)
        }
    }
}

private func writeReferenceFiles(
    sourceFiles: [String],
    outputDirectory: String,
    format: ReferenceFormat,
    options: Reference.MarkdownOptions
) throws -> Int {
    let outputURL = URL(fileURLWithPath: outputDirectory)
    try FileManager.default.createDirectory(
        at: outputURL,
        withIntermediateDirectories: true
    )

    var fileCount = 0
    var indexEntries: [Reference.IndexEntry] = []
    for sourceFile in sourceFiles {
        let documents = try Reference.parseFile(at: sourceFile)
        guard let topLevelDocument = documents.first else {
            continue
        }

        let fileName = format.fileName(forTitle: topLevelDocument.title)
        let rendered = try format.render(documents, options: options)
        try rendered.write(
            to: outputURL.appendingPathComponent(fileName),
            atomically: true,
            encoding: .utf8
        )
        fileCount += 1
        for (documentIndex, document) in documents.enumerated() {
            indexEntries.append(
                Reference.IndexEntry(
                    title: document.title,
                    fileName: fileName,
                    anchor: documentIndex == 0
                        ? nil : Reference.markdownAnchor(forTitle: document.title)
                )
            )
        }
    }

    let index = try format.renderIndex(indexEntries, title: "Wendy E2E Reference")
    try index.write(
        to: outputURL.appendingPathComponent(format.indexFileName),
        atomically: true,
        encoding: .utf8
    )

    return fileCount
}

private func referenceSourceFiles(for path: String) throws -> [String] {
    var isDirectory: ObjCBool = false
    guard FileManager.default.fileExists(atPath: path, isDirectory: &isDirectory) else {
        throw ValidationError("Path not found: \(path)")
    }

    if !isDirectory.boolValue {
        return [path]
    }

    guard let enumerator = FileManager.default.enumerator(atPath: path) else {
        throw ValidationError("Directory cannot be read: \(path)")
    }

    return enumerator.compactMap { element -> String? in
        guard let relativePath = element as? String, relativePath.hasSuffix(".swift") else {
            return nil
        }
        return URL(fileURLWithPath: path).appendingPathComponent(relativePath).path
    }.sorted()
}
