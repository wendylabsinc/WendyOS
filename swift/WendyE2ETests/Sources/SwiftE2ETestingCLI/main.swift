import Foundation
import WendyE2ETesting

@main
struct SwiftE2ETestingCLI {
    static func main() {
        do {
            let output = try run(arguments: Array(CommandLine.arguments.dropFirst()))
            if !output.isEmpty {
                print(output, terminator: output.hasSuffix("\n") ? "" : "\n")
            }
        } catch let error as CLIError {
            FileHandle.standardError.write(Data(error.description.utf8))
            FileHandle.standardError.write(Data("\n".utf8))
            Foundation.exit(error.exitCode)
        } catch {
            FileHandle.standardError.write(Data("error: \(error)\n".utf8))
            Foundation.exit(1)
        }
    }

    static func run(arguments: [String]) throws -> String {
        guard let command = arguments.first else {
            throw CLIError.usage
        }

        switch command {
        case "reference":
            return try renderReference(arguments: Array(arguments.dropFirst()))
        case "help", "--help", "-h":
            return usage
        default:
            throw CLIError.unknownCommand(command)
        }
    }

    private static func renderReference(arguments: [String]) throws -> String {
        var includeRequirements = false
        var includeSourceLocations = false
        var includeDisabledState = false
        var outputDirectory: String?
        var paths: [String] = []

        var index = 0
        while index < arguments.count {
            let argument = arguments[index]
            switch argument {
            case "--include-requirements":
                includeRequirements = true
            case "--include-source-locations":
                includeSourceLocations = true
            case "--include-disabled-state":
                includeDisabledState = true
            case "--spec-review":
                includeRequirements = true
                includeSourceLocations = true
                includeDisabledState = true
            case "--output", "-o":
                index += 1
                guard index < arguments.count else {
                    throw CLIError.missingOptionValue(argument)
                }
                outputDirectory = arguments[index]
            case "--help", "-h":
                return referenceUsage
            default:
                if argument.hasPrefix("-") {
                    throw CLIError.unknownOption(argument)
                }
                paths.append(argument)
            }
            index += 1
        }

        guard !paths.isEmpty else {
            throw CLIError.missingReferencePath
        }

        let options = Reference.MarkdownOptions(
            includeRequirements: includeRequirements,
            includeSourceLocations: includeSourceLocations,
            includeDisabledState: includeDisabledState
        )
        let sourceFiles = try paths.flatMap(referenceSourceFiles)

        if let outputDirectory {
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

                let fileName = Reference.markdownFileName(forTitle: topLevelDocument.title)
                let markdown = Reference.renderMarkdown(documents, options: options)
                try markdown.write(
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

            let indexMarkdown = Reference.renderMarkdownIndex(
                indexEntries,
                title: "Wendy E2E Reference"
            )
            try indexMarkdown.write(
                to: outputURL.appendingPathComponent("index.md"),
                atomically: true,
                encoding: .utf8
            )

            return "Wrote \(fileCount) reference file(s) to \(outputDirectory)"
        }

        var documents: [Reference.Document] = []
        for sourceFile in sourceFiles {
            documents.append(contentsOf: try Reference.parseFile(at: sourceFile))
        }
        return Reference.renderMarkdown(documents, options: options)
    }

    private static func referenceSourceFiles(for path: String) throws -> [String] {
        var isDirectory: ObjCBool = false
        guard FileManager.default.fileExists(atPath: path, isDirectory: &isDirectory) else {
            throw CLIError.pathNotFound(path)
        }

        if !isDirectory.boolValue {
            return [path]
        }

        guard let enumerator = FileManager.default.enumerator(atPath: path) else {
            throw CLIError.unreadableDirectory(path)
        }

        return enumerator.compactMap { element -> String? in
            guard let relativePath = element as? String, relativePath.hasSuffix(".swift") else {
                return nil
            }
            return URL(fileURLWithPath: path).appendingPathComponent(relativePath).path
        }.sorted()
    }

    private static let usage = """
        Usage: swift-e2e-testing <command> [options]

        Commands:
          reference       Generate Markdown reference documentation from Swift E2E tests.

        Run `swift-e2e-testing reference --help` for reference options.
        """

    private static let referenceUsage = """
        Usage: swift-e2e-testing reference [OPTIONS] <PATH> [PATH ...]

        Generate Markdown reference documentation from Swift E2E tests.

        Each PATH may be a Swift test file or a directory containing Swift test files.
        Without --output, all generated Markdown is written to stdout. With --output,
        one Markdown file is written per Swift test file, along with an index.md.

        Options:
          -o, --output DIR             Write Markdown files and index.md to DIR.
          --include-requirements       Include Given/When/Then requirement comments.
          --include-source-locations   Include source file and line metadata.
          --include-disabled-state     Include enabled/disabled test metadata.
          --spec-review                Include requirements, source locations, and disabled state.
          --help                       Show this help message.
        """
}

enum CLIError: Error, CustomStringConvertible {
    case usage
    case unknownCommand(String)
    case unknownOption(String)
    case missingOptionValue(String)
    case missingReferencePath
    case pathNotFound(String)
    case unreadableDirectory(String)

    var exitCode: Int32 {
        switch self {
        case .usage:
            64
        case .unknownCommand:
            64
        case .unknownOption:
            64
        case .missingOptionValue:
            64
        case .missingReferencePath:
            64
        case .pathNotFound:
            66
        case .unreadableDirectory:
            66
        }
    }

    var description: String {
        switch self {
        case .usage:
            """
            Usage: swift-e2e-testing <command> [options]

            Commands:
              reference       Generate Markdown reference documentation from Swift E2E tests.

            Run `swift-e2e-testing reference --help` for reference options.
            """
        case .unknownCommand(let command):
            "Unknown command: \(command)\n\nRun `swift-e2e-testing --help` for usage."
        case .unknownOption(let option):
            "Unknown option: \(option)\n\nRun `swift-e2e-testing reference --help` for usage."
        case .missingOptionValue(let option):
            "Missing value for \(option).\n\nRun `swift-e2e-testing reference --help` for usage."
        case .missingReferencePath:
            "Missing path.\n\nRun `swift-e2e-testing reference --help` for usage."
        case .pathNotFound(let path):
            "Path not found: \(path)"
        case .unreadableDirectory(let path):
            "Directory cannot be read: \(path)"
        }
    }
}
