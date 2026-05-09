import Foundation
import SwiftParser
import SwiftSyntax

public enum Reference {
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
        public var requirements: Requirements
        public var sourceLocation: SourceLocation
        public var isDisabled: Bool

        public init(
            title: String,
            documentation: String,
            requirements: Requirements,
            sourceLocation: SourceLocation,
            isDisabled: Bool
        ) {
            self.title = title
            self.documentation = documentation
            self.requirements = requirements
            self.sourceLocation = sourceLocation
            self.isDisabled = isDisabled
        }
    }

    public struct Requirements: Sendable, Equatable, Encodable {
        public var given: [String]
        public var when: [String]
        public var then: [String]

        public init(given: [String] = [], when: [String] = [], then: [String] = []) {
            self.given = given
            self.when = when
            self.then = then
        }

        public var isEmpty: Bool {
            given.isEmpty && when.isEmpty && then.isEmpty
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

    public struct MarkdownOptions: Sendable, Equatable {
        public var includeRequirements: Bool
        public var includeSourceLocations: Bool
        public var includeDisabledState: Bool

        public init(
            includeRequirements: Bool,
            includeSourceLocations: Bool,
            includeDisabledState: Bool
        ) {
            self.includeRequirements = includeRequirements
            self.includeSourceLocations = includeSourceLocations
            self.includeDisabledState = includeDisabledState
        }

        public static let reference = MarkdownOptions(
            includeRequirements: false,
            includeSourceLocations: false,
            includeDisabledState: false
        )

        public static let specReview = MarkdownOptions(
            includeRequirements: true,
            includeSourceLocations: true,
            includeDisabledState: true
        )
    }

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
        _ = Parser.parse(source: source) as SourceFileSyntax

        var parser = SourceParser(source: source, path: path)
        return parser.parse()
    }

    // MARK: - Rendering Markdown

    public static func renderMarkdown(
        _ documents: [Document],
        options: MarkdownOptions = .reference
    ) -> String {
        documents.map { renderMarkdown($0, options: options) }
            .joined(separator: "\n\n---\n\n")
    }

    public static func markdownFileName(forTitle title: String) -> String {
        "\(markdownSlug(forTitle: title, fallback: "reference")).md"
    }

    public static func htmlFileName(forTitle title: String) -> String {
        "\(markdownSlug(forTitle: title, fallback: "reference")).html"
    }

    public static func jsonFileName(forTitle title: String) -> String {
        "\(markdownSlug(forTitle: title, fallback: "reference")).json"
    }

    public static func markdownAnchor(forTitle title: String) -> String {
        markdownSlug(forTitle: title, fallback: "section")
    }

    public static func renderMarkdownIndex(
        _ entries: [IndexEntry],
        title: String = "Reference"
    ) -> String {
        var markdown: [String] = ["# \(title)", ""]
        for entry in entries {
            let target = entry.anchor.map { "\(entry.fileName)#\($0)" } ?? entry.fileName
            markdown.append("- [\(entry.title)](\(target))")
        }
        return markdown.joined(separator: "\n").trimmingCharacters(in: .whitespacesAndNewlines)
            + "\n"
    }

    public static func renderMarkdown(
        _ document: Document,
        options: MarkdownOptions = .reference
    ) -> String {
        var markdown: [String] = []
        markdown.append("# \(document.title)")
        appendParagraph(document.overview, to: &markdown)
        appendMetadata(
            isDisabled: nil,
            sourceLocation: document.sourceLocation,
            options: options,
            to: &markdown
        )

        for section in document.sections where !section.entries.isEmpty {
            markdown.append("## \(section.title)")
            markdown.append("")

            for entry in section.entries {
                markdown.append("### \(entry.title)")
                appendMetadata(
                    isDisabled: entry.isDisabled,
                    sourceLocation: entry.sourceLocation,
                    options: options,
                    to: &markdown
                )
                appendParagraph(entry.documentation, to: &markdown)

                if options.includeRequirements && !entry.requirements.isEmpty {
                    markdown.append("#### Requirements")
                    markdown.append("")
                    appendRequirements(entry.requirements, to: &markdown)
                }
            }
        }

        return markdown.joined(separator: "\n").trimmingCharacters(in: .whitespacesAndNewlines)
            + "\n"
    }

    // MARK: - Rendering HTML

    public static func renderHTML(
        _ documents: [Document],
        options: MarkdownOptions = .reference
    ) -> String {
        let title = documents.first?.title ?? "Reference"
        let body = documents.map { renderHTMLBody($0, options: options) }
            .joined(separator: "\n<hr>\n")
        return renderHTMLDocument(title: title, body: body)
    }

    public static func renderHTMLIndex(
        _ entries: [IndexEntry],
        title: String = "Reference"
    ) -> String {
        var html: [String] = []
        html.append("<h1>\(renderInlineHTML(title))</h1>")
        html.append("<ul>")
        for entry in entries {
            let target = entry.anchor.map { "\(entry.fileName)#\($0)" } ?? entry.fileName
            html.append(
                "<li><a href=\"\(escapeHTMLAttribute(target))\">\(renderInlineHTML(entry.title))</a></li>"
            )
        }
        html.append("</ul>")
        return renderHTMLDocument(title: title, body: html.joined(separator: "\n"))
    }

    public static func renderHTML(
        _ document: Document,
        options: MarkdownOptions = .reference
    ) -> String {
        renderHTMLDocument(
            title: document.title,
            body: renderHTMLBody(document, options: options)
        )
    }

    // MARK: - Rendering JSON

    public static func renderJSON(
        _ documents: [Document],
        options: MarkdownOptions = .reference
    ) throws -> String {
        try renderJSONValue(documents.map { JSONDocument($0, options: options) })
    }

    public static func renderJSONIndex(
        _ entries: [IndexEntry],
        title: String = "Reference"
    ) throws -> String {
        try renderJSONValue(JSONIndex(title: title, entries: entries.map(JSONIndexEntry.init)))
    }

    public static func renderJSON(
        _ document: Document,
        options: MarkdownOptions = .reference
    ) throws -> String {
        try renderJSON([document], options: options)
    }
}

// MARK: - Private Parsing

private struct SourceParser {
    private enum RequirementContext {
        case none
        case given
        case when
        case then
    }

    private struct PendingTest {
        var documentation: String
        var isDisabled: Bool
    }

    private let lines: [String]
    private let path: String

    private var index: Int = 0
    private var pendingDocumentation: String?
    private var pendingSuiteDocumentation: String?
    private var pendingTest: PendingTest?
    private var currentDocument: Reference.Document?
    private var currentSections: [Reference.Section] = []
    private var documents: [Reference.Document] = []

    init(source: String, path: String) {
        self.lines = source.components(separatedBy: .newlines)
        self.path = path
    }

    mutating func parse() -> [Reference.Document] {
        while index < lines.count {
            let line = lines[index]
            let trimmed = line.trimmingCharacters(in: .whitespaces)

            if let documentation = parseDocumentationComment() {
                pendingDocumentation = documentation
                continue
            }

            if trimmed.hasPrefix("@Suite") {
                pendingSuiteDocumentation = pendingDocumentation ?? ""
                pendingDocumentation = nil
                index += 1
                continue
            }

            if let suiteTitle = parseSuiteTitle(from: trimmed) {
                finishCurrentDocument()
                currentDocument = Reference.Document(
                    title: suiteTitle,
                    overview: pendingSuiteDocumentation ?? "",
                    sections: [],
                    sourceLocation: Reference.SourceLocation(path: path, line: index + 1)
                )
                currentSections = []
                pendingSuiteDocumentation = nil
                index += 1
                continue
            }

            if let sectionTitle = parseMarkTitle(from: trimmed), currentDocument != nil {
                currentSections.append(Reference.Section(title: sectionTitle))
                index += 1
                continue
            }

            if trimmed.hasPrefix("@Test") {
                pendingTest = PendingTest(
                    documentation: pendingDocumentation ?? "",
                    isDisabled: trimmed.contains(".disabled(")
                )
                pendingDocumentation = nil
                index += 1
                continue
            }

            if let testTitle = parseFunctionTitle(from: trimmed), let pendingTest {
                appendEntry(
                    title: testTitle,
                    pendingTest: pendingTest,
                    functionLine: index
                )
                self.pendingTest = nil
                continue
            }

            index += 1
        }

        finishCurrentDocument()
        return documents
    }

    private mutating func finishCurrentDocument() {
        guard var document = currentDocument else {
            return
        }

        document.sections = currentSections
        documents.append(document)
        currentDocument = nil
        currentSections = []
    }

    private mutating func appendEntry(
        title: String,
        pendingTest: PendingTest,
        functionLine: Int
    ) {
        let requirements = parseRequirements(startingAt: functionLine)
        let entry = Reference.Entry(
            title: title,
            documentation: pendingTest.documentation,
            requirements: requirements,
            sourceLocation: Reference.SourceLocation(path: path, line: functionLine + 1),
            isDisabled: pendingTest.isDisabled
        )

        if currentSections.isEmpty {
            currentSections.append(Reference.Section(title: "Overview"))
        }
        currentSections[currentSections.count - 1].entries.append(entry)
    }

    private mutating func parseDocumentationComment() -> String? {
        let trimmed = lines[index].trimmingCharacters(in: .whitespaces)
        if trimmed.hasPrefix("///") {
            var commentLines: [String] = []
            while index < lines.count {
                let line = lines[index].trimmingCharacters(in: .whitespaces)
                guard line.hasPrefix("///") else {
                    break
                }
                commentLines.append(cleanLineDocumentation(line))
                index += 1
            }
            return trimBlankLines(commentLines).joined(separator: "\n")
        }

        if trimmed.hasPrefix("/**") {
            var commentLines: [String] = []
            var line = trimmed
            line.removeFirst(3)

            while true {
                if let range = line.range(of: "*/") {
                    let beforeEnd = String(line[..<range.lowerBound])
                    commentLines.append(cleanBlockDocumentation(beforeEnd))
                    index += 1
                    break
                }

                commentLines.append(cleanBlockDocumentation(line))
                index += 1
                guard index < lines.count else {
                    break
                }
                line = lines[index].trimmingCharacters(in: .whitespaces)
            }

            return trimBlankLines(commentLines).joined(separator: "\n")
        }

        return nil
    }

    private mutating func parseRequirements(startingAt functionLine: Int) -> Reference.Requirements
    {
        var requirements = Reference.Requirements()
        var context = RequirementContext.none
        var braceDepth = countBraces(in: lines[functionLine])
        var cursor = functionLine + 1

        while cursor < lines.count, braceDepth > 0 {
            let trimmed = lines[cursor].trimmingCharacters(in: .whitespaces)
            if trimmed.hasPrefix("//") {
                let comment = cleanOrdinaryComment(trimmed)
                appendRequirement(comment, context: &context, requirements: &requirements)
            }

            braceDepth += countBraces(in: lines[cursor])
            cursor += 1
        }

        index = cursor
        return requirements
    }

    private func appendRequirement(
        _ comment: String,
        context: inout RequirementContext,
        requirements: inout Reference.Requirements
    ) {
        if let value = comment.removingPrefix("Given:") {
            context = .given
            requirements.given.append(value)
        } else if let value = comment.removingPrefix("When:") {
            context = .when
            requirements.when.append(value)
        } else if let value = comment.removingPrefix("Then:") {
            context = .then
            if !value.isEmpty {
                requirements.then.append(value)
            }
        } else if let value = comment.removingPrefix("And:") {
            switch context {
            case .given:
                requirements.given.append(value)
            case .when:
                requirements.when.append(value)
            case .then:
                requirements.then.append(value)
            case .none:
                break
            }
        } else if let value = comment.removingPrefix("-") {
            requirements.then.append(value)
        }
    }

    private func parseSuiteTitle(from line: String) -> String? {
        guard line.hasPrefix("struct ") else {
            return nil
        }
        return parseBacktickedName(from: line)?.formattingQuotedSpansAsMarkdownCode()
    }

    private func parseFunctionTitle(from line: String) -> String? {
        guard line.hasPrefix("func ") else {
            return nil
        }
        return parseBacktickedName(from: line)?.formattingQuotedSpansAsMarkdownCode()
    }

    private func parseBacktickedName(from line: String) -> String? {
        guard let first = line.firstIndex(of: "`"),
            let last = line[line.index(after: first)...].firstIndex(of: "`")
        else {
            return nil
        }

        return String(line[line.index(after: first)..<last])
    }

    private func parseMarkTitle(from line: String) -> String? {
        guard line.hasPrefix("// MARK:") else {
            return nil
        }

        let title =
            line
            .dropFirst("// MARK:".count)
            .trimmingCharacters(in: .whitespaces)
            .removingPrefix("-")?
            .trimmingCharacters(in: .whitespaces)

        guard let title, !title.isEmpty else {
            return nil
        }
        return title
    }

    private func countBraces(in line: String) -> Int {
        line.reduce(0) { depth, character in
            switch character {
            case "{":
                depth + 1
            case "}":
                depth - 1
            default:
                depth
            }
        }
    }
}

// MARK: - Private Markdown Helpers

private func markdownSlug(forTitle title: String, fallback: String) -> String {
    let scalars = title.lowercased().unicodeScalars
    var result = ""
    var previousWasSeparator = false

    for scalar in scalars {
        if CharacterSet.alphanumerics.contains(scalar) {
            result.unicodeScalars.append(scalar)
            previousWasSeparator = false
        } else if !previousWasSeparator {
            result.append("-")
            previousWasSeparator = true
        }
    }

    let slug = result.trimmingCharacters(in: CharacterSet(charactersIn: "-"))
    return slug.isEmpty ? fallback : slug
}

// MARK: - Private JSON Rendering

private struct JSONIndex: Encodable {
    var title: String
    var entries: [JSONIndexEntry]
}

private struct JSONIndexEntry: Encodable {
    var title: String
    var fileName: String
    var anchor: String?

    init(_ entry: Reference.IndexEntry) {
        self.title = entry.title
        self.fileName = entry.fileName
        self.anchor = entry.anchor
    }
}

private struct JSONDocument: Encodable {
    var title: String
    var overview: String
    var sourceLocation: Reference.SourceLocation?
    var sections: [JSONSection]

    init(_ document: Reference.Document, options: Reference.MarkdownOptions) {
        self.title = document.title
        self.overview = document.overview
        self.sourceLocation = options.includeSourceLocations ? document.sourceLocation : nil
        self.sections = document.sections.map { JSONSection($0, options: options) }
    }
}

private struct JSONSection: Encodable {
    var title: String
    var entries: [JSONEntry]

    init(_ section: Reference.Section, options: Reference.MarkdownOptions) {
        self.title = section.title
        self.entries = section.entries.map { JSONEntry($0, options: options) }
    }
}

private struct JSONEntry: Encodable {
    var title: String
    var documentation: String
    var requirements: Reference.Requirements?
    var sourceLocation: Reference.SourceLocation?
    var isDisabled: Bool?

    init(_ entry: Reference.Entry, options: Reference.MarkdownOptions) {
        self.title = entry.title
        self.documentation = entry.documentation
        self.requirements = options.includeRequirements ? entry.requirements : nil
        self.sourceLocation = options.includeSourceLocations ? entry.sourceLocation : nil
        self.isDisabled = options.includeDisabledState ? entry.isDisabled : nil
    }
}

private func renderJSONValue(_ value: some Encodable) throws -> String {
    let encoder = JSONEncoder()
    encoder.outputFormatting = [.prettyPrinted, .sortedKeys, .withoutEscapingSlashes]
    let data = try encoder.encode(value)
    return String(decoding: data, as: UTF8.self) + "\n"
}

// MARK: - Private HTML Rendering

private func renderHTMLBody(
    _ document: Reference.Document,
    options: Reference.MarkdownOptions
) -> String {
    var html: [String] = []
    html.append(
        "<h1 id=\"\(escapeHTMLAttribute(Reference.markdownAnchor(forTitle: document.title)))\">\(renderInlineHTML(document.title))</h1>"
    )
    appendHTMLBlocks(document.overview, to: &html)
    appendHTMLMetadata(
        isDisabled: nil,
        sourceLocation: document.sourceLocation,
        options: options,
        to: &html
    )

    for section in document.sections where !section.entries.isEmpty {
        html.append(
            "<h2 id=\"\(escapeHTMLAttribute(Reference.markdownAnchor(forTitle: section.title)))\">\(renderInlineHTML(section.title))</h2>"
        )

        for entry in section.entries {
            html.append(
                "<h3 id=\"\(escapeHTMLAttribute(Reference.markdownAnchor(forTitle: entry.title)))\">\(renderInlineHTML(entry.title))</h3>"
            )
            appendHTMLMetadata(
                isDisabled: entry.isDisabled,
                sourceLocation: entry.sourceLocation,
                options: options,
                to: &html
            )
            appendHTMLBlocks(entry.documentation, to: &html)

            if options.includeRequirements && !entry.requirements.isEmpty {
                html.append("<h4>Requirements</h4>")
                appendHTMLRequirements(entry.requirements, to: &html)
            }
        }
    }

    return html.joined(separator: "\n")
}

private func renderHTMLDocument(title: String, body: String) -> String {
    let plainTitle = strippingInlineCodeMarkup(from: title)
    return """
        <!doctype html>
        <html lang="en">
        <head>
          <meta charset="utf-8" />
          <meta name="viewport" content="width=device-width, initial-scale=1" />
          <title>\(escapeHTMLText(plainTitle))</title>
          <script>
            (() => {
              try {
                const stored = localStorage.getItem('wendy-e2e-theme');
                const preferred = matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
                document.documentElement.dataset.theme = stored || preferred;
              } catch {
                document.documentElement.dataset.theme = 'light';
              }
            })();
          </script>
          <style>
            :root {
              color-scheme: light;
              --emerald-50: #ecfdf5;
              --emerald-100: #d1fae5;
              --emerald-200: #a7f3d0;
              --emerald-300: #6ee7b7;
              --emerald-400: #34d399;
              --emerald-500: #10b981;
              --emerald-600: #059669;
              --emerald-700: #047857;
              --emerald-800: #065f46;
              --emerald-900: #064e3b;
              --emerald-950: #022c22;

              --bg: #f8fafc;
              --card: rgba(255, 255, 255, .92);
              --panel: rgba(255, 255, 255, .78);
              --text: #111827;
              --muted: #64748b;
              --line: #e5e7eb;
              --soft: #f3f4f6;
              --blue: var(--emerald-600);
              --shadow: rgba(15, 23, 42, .08);
              --shadow-strong: rgba(15, 23, 42, .14);
              --focus-ring: rgba(16, 185, 129, .18);
              --code-bg: rgba(243, 244, 246, .90);
            }

            :root[data-theme="dark"] {
              color-scheme: dark;
              --bg: #020617;
              --card: rgba(15, 23, 42, .88);
              --panel: rgba(30, 41, 59, .58);
              --text: #f8fafc;
              --muted: #94a3b8;
              --line: rgba(148, 163, 184, .22);
              --soft: rgba(51, 65, 85, .48);
              --blue: var(--emerald-400);
              --shadow: rgba(0, 0, 0, .28);
              --shadow-strong: rgba(0, 0, 0, .38);
              --focus-ring: rgba(52, 211, 153, .22);
              --code-bg: rgba(51, 65, 85, .62);
            }

            * { box-sizing: border-box; }

            body {
              margin: 0;
              background: var(--bg);
              color: var(--text);
              font: 16px/1.6 Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
            }

            main {
              max-width: 1080px;
              margin: 0 auto;
              padding: 28px 24px 72px;
            }

            header {
              display: grid;
              grid-template-columns: minmax(0, 1fr) auto;
              gap: 20px;
              align-items: end;
              margin-bottom: 18px;
            }

            .brand-row {
              display: inline-flex;
              align-items: center;
              gap: 10px;
              margin-bottom: 14px;
            }

            .brand-mark {
              display: inline-grid;
              place-items: center;
              width: 34px;
              height: 34px;
              color: var(--text);
            }

            .brand-mark svg {
              display: block;
              width: 30px;
              height: 30px;
              fill: currentColor;
            }

            .brand-copy {
              display: grid;
              gap: 0;
              line-height: 1.1;
            }

            .brand-copy strong {
              font-size: 15px;
              letter-spacing: -.02em;
            }

            .brand-copy span {
              color: var(--muted);
              font-size: 12px;
              font-weight: 700;
              text-transform: uppercase;
              letter-spacing: .08em;
            }

            .header-side {
              display: grid;
              gap: 10px;
              justify-items: end;
            }

            .theme-toggle {
              appearance: none;
              display: inline-flex;
              align-items: center;
              gap: 8px;
              border: 1px solid var(--line);
              border-radius: 999px;
              background: var(--card);
              color: var(--text);
              cursor: pointer;
              font: inherit;
              font-size: 13px;
              font-weight: 900;
              padding: 8px 12px;
              box-shadow: 0 8px 22px var(--shadow);
            }

            .theme-toggle:hover {
              transform: translateY(-1px);
              box-shadow: 0 10px 26px var(--shadow-strong);
            }

            .theme-toggle:focus-visible {
              outline: 3px solid var(--focus-ring);
              outline-offset: 2px;
            }

            .theme-toggle-icon {
              color: var(--blue);
              font-size: 15px;
              line-height: 1;
            }

            .page-title {
              margin: 0 0 8px;
              font-size: clamp(28px, 4vw, 40px);
              line-height: 1.04;
              letter-spacing: -0.045em;
            }

            .lead {
              margin: 0;
              max-width: 720px;
              color: var(--muted);
              font-size: 15px;
              line-height: 1.45;
            }

            .card {
              margin-top: 30px;
              padding: 22px;
              background: var(--card);
              border: 1px solid var(--line);
              border-radius: 18px;
              box-shadow: 0 10px 28px var(--shadow);
            }

            h1, h2, h3, h4, h5 {
              color: var(--text);
              letter-spacing: -0.025em;
            }

            .card > h1:first-child {
              margin-top: 0;
              padding-bottom: 10px;
              border-bottom: 1px solid var(--line);
              font-size: 28px;
              line-height: 1.1;
            }

            h2 {
              margin: 28px 0 10px;
              padding-top: 18px;
              border-top: 1px solid var(--line);
              font-size: 22px;
              line-height: 1.2;
            }

            h3 {
              margin: 18px 0 7px;
              font-size: 18px;
              line-height: 1.35;
            }

            h4 {
              margin: 16px 0 8px;
              color: var(--muted);
              font-size: 12px;
              font-weight: 900;
              letter-spacing: .07em;
              text-transform: uppercase;
            }

            h5 {
              margin: 12px 0 4px;
              color: var(--muted);
              font-size: 13px;
            }

            p { margin: 0 0 12px; }

            ul {
              margin: 0 0 16px;
              padding: 0;
              list-style: none;
            }

            li {
              position: relative;
              padding: 3px 0 3px 20px;
            }

            li::before {
              content: "";
              position: absolute;
              left: 2px;
              top: .85em;
              width: 6px;
              height: 6px;
              border-radius: 999px;
              background: var(--blue);
            }

            a {
              color: var(--blue);
              font-weight: 800;
              text-decoration: none;
            }

            a:hover { text-decoration: underline; }

            code {
              font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
              font-size: .88em;
              background: var(--code-bg);
              border: 1px solid var(--line);
              border-radius: 5px;
              padding: .12em .34em;
            }

            pre {
              overflow-x: auto;
              margin: 0 0 16px;
              padding: 1rem;
              background: var(--soft);
              border: 1px solid var(--line);
              border-radius: 12px;
            }

            pre code {
              background: transparent;
              border: 0;
              padding: 0;
            }

            .metadata {
              margin: 8px 0 14px;
              color: var(--muted);
              font-size: 13px;
            }

            hr {
              border: 0;
              border-top: 1px solid var(--line);
              margin: 2rem 0;
            }

            footer {
              margin-top: 22px;
              color: var(--muted);
              font-size: 13px;
              text-align: center;
            }

            @media (max-width: 680px) {
              main { padding: 24px 16px 56px; }
              header { grid-template-columns: 1fr; }
              .header-side { justify-items: stretch; }
              .theme-toggle { justify-self: start; }
              .card { padding: 16px; }
            }
          </style>
        </head>
        <body>
          <main>
            <header>
              <div>
                <div class="brand-row" aria-label="Wendy E2E Reference">
                  <span class="brand-mark" aria-hidden="true"><svg viewBox="0 0 1024 1024" role="img"><rect x="407.04" y="299.64" width="424.72" height="424.72" transform="translate(-180.62 587.94) rotate(-45)"/><path d="M335.3,743.03l-231.03-231.03,231.03-231.02,231.02,231.02-231.02,231.03ZM179.04,512l156.27,156.27,156.27-156.27-156.27-156.27-156.27,156.27Z"/></svg></span>
                  <span class="brand-copy"><strong>E2E Reference</strong><span>Wendy Agent</span></span>
                </div>
                <h1 class="page-title">\(escapeHTMLText(plainTitle))</h1>
                <p class="lead">Behavioral reference generated from Swift E2E specs.</p>
              </div>
              <div class="header-side">
                <button class="theme-toggle" type="button" data-theme-toggle aria-label="Switch color theme">
                  <span class="theme-toggle-icon" data-theme-toggle-icon aria-hidden="true">◐</span>
                  <span data-theme-toggle-label>Theme</span>
                </button>
              </div>
            </header>

            <section class="card">
        \(body)
            </section>

            <footer>Generated by <code>swift-e2e-testing reference</code></footer>
          </main>
          <script>
            (() => {
              const themeToggle = document.querySelector('[data-theme-toggle]');
              const themeToggleIcon = document.querySelector('[data-theme-toggle-icon]');
              const themeToggleLabel = document.querySelector('[data-theme-toggle-label]');

              function currentTheme() {
                return document.documentElement.dataset.theme === 'dark' ? 'dark' : 'light';
              }

              function updateThemeToggle() {
                const theme = currentTheme();
                if (themeToggle) {
                  themeToggle.setAttribute('aria-pressed', String(theme === 'dark'));
                  themeToggle.setAttribute('title', `Switch to ${theme === 'dark' ? 'light' : 'dark'} mode`);
                }
                if (themeToggleIcon) themeToggleIcon.textContent = theme === 'dark' ? '☾' : '☼';
                if (themeToggleLabel) themeToggleLabel.textContent = theme === 'dark' ? 'Dark' : 'Light';
              }

              function setTheme(theme) {
                document.documentElement.dataset.theme = theme;
                try { localStorage.setItem('wendy-e2e-theme', theme); } catch {}
                updateThemeToggle();
              }

              themeToggle?.addEventListener('click', () => {
                setTheme(currentTheme() === 'dark' ? 'light' : 'dark');
              });

              updateThemeToggle();
            })();
          </script>
        </body>
        </html>
        """
}

private func appendHTMLBlocks(_ text: String, to html: inout [String]) {
    let lines = text.trimmingCharacters(in: .whitespacesAndNewlines).components(
        separatedBy: .newlines
    )
    guard !lines.isEmpty, !(lines.count == 1 && lines[0].isEmpty) else {
        return
    }

    var paragraph: [String] = []
    var listItems: [String] = []
    var codeLines: [String] = []
    var isInCodeFence = false

    func flushParagraph() {
        guard !paragraph.isEmpty else { return }
        let text = paragraph.map { $0.trimmingCharacters(in: .whitespaces) }.joined(separator: " ")
        html.append("<p>\(renderInlineHTML(text))</p>")
        paragraph.removeAll()
    }

    func flushList() {
        guard !listItems.isEmpty else { return }
        html.append("<ul>")
        for item in listItems {
            html.append("<li>\(renderInlineHTML(item))</li>")
        }
        html.append("</ul>")
        listItems.removeAll()
    }

    func flushCode() {
        guard !codeLines.isEmpty else { return }
        html.append("<pre><code>\(escapeHTMLText(codeLines.joined(separator: "\n")))</code></pre>")
        codeLines.removeAll()
    }

    for line in lines {
        let trimmed = line.trimmingCharacters(in: .whitespaces)
        if trimmed.hasPrefix("```") {
            if isInCodeFence {
                flushCode()
                isInCodeFence = false
            } else {
                flushParagraph()
                flushList()
                isInCodeFence = true
            }
            continue
        }

        if isInCodeFence {
            codeLines.append(line)
        } else if trimmed.isEmpty {
            flushParagraph()
            flushList()
        } else if let listItem = trimmed.removingPrefix("-") {
            flushParagraph()
            listItems.append(listItem)
        } else {
            flushList()
            paragraph.append(line)
        }
    }

    flushParagraph()
    flushList()
    flushCode()
}

private func appendHTMLMetadata(
    isDisabled: Bool?,
    sourceLocation: Reference.SourceLocation,
    options: Reference.MarkdownOptions,
    to html: inout [String]
) {
    var metadata: [String] = []
    if options.includeDisabledState, let isDisabled {
        metadata.append(isDisabled ? "disabled" : "enabled")
    }
    if options.includeSourceLocations {
        metadata.append(
            "<code>\(escapeHTMLText("\(sourceLocation.path):\(sourceLocation.line)"))</code>"
        )
    }

    guard !metadata.isEmpty else {
        return
    }

    html.append("<p class=\"metadata\">\(metadata.joined(separator: " · "))</p>")
}

private func appendHTMLRequirements(_ requirements: Reference.Requirements, to html: inout [String])
{
    appendHTMLRequirementGroup("Given", requirements.given, to: &html)
    appendHTMLRequirementGroup("When", requirements.when, to: &html)
    appendHTMLRequirementGroup("Then", requirements.then, to: &html)
}

private func appendHTMLRequirementGroup(
    _ title: String,
    _ values: [String],
    to html: inout [String]
) {
    guard !values.isEmpty else {
        return
    }

    html.append("<h5>\(escapeHTMLText(title))</h5>")
    html.append("<ul>")
    for value in values {
        html.append("<li>\(renderInlineHTML(value))</li>")
    }
    html.append("</ul>")
}

private func renderInlineHTML(_ value: String) -> String {
    var html = ""
    var cursor = value.startIndex

    while cursor < value.endIndex {
        guard value[cursor] == "`" else {
            html.append(escapeHTMLText(String(value[cursor])))
            cursor = value.index(after: cursor)
            continue
        }

        let contentStart = value.index(after: cursor)
        guard let contentEnd = value[contentStart...].firstIndex(of: "`") else {
            html.append(escapeHTMLText(String(value[cursor])))
            cursor = value.index(after: cursor)
            continue
        }

        html.append("<code>")
        html.append(escapeHTMLText(String(value[contentStart..<contentEnd])))
        html.append("</code>")
        cursor = value.index(after: contentEnd)
    }

    return html
}

private func escapeHTMLText(_ value: String) -> String {
    value
        .replacingOccurrences(of: "&", with: "&amp;")
        .replacingOccurrences(of: "<", with: "&lt;")
        .replacingOccurrences(of: ">", with: "&gt;")
}

private func escapeHTMLAttribute(_ value: String) -> String {
    escapeHTMLText(value)
        .replacingOccurrences(of: "\"", with: "&quot;")
}

private func strippingInlineCodeMarkup(from value: String) -> String {
    value.replacingOccurrences(of: "`", with: "")
}

// MARK: - Private Rendering

private func appendParagraph(_ paragraph: String, to markdown: inout [String]) {
    let paragraph = paragraph.trimmingCharacters(in: .whitespacesAndNewlines)
    guard !paragraph.isEmpty else {
        markdown.append("")
        return
    }

    markdown.append("")
    markdown.append(paragraph)
    markdown.append("")
}

private func appendMetadata(
    isDisabled: Bool?,
    sourceLocation: Reference.SourceLocation,
    options: Reference.MarkdownOptions,
    to markdown: inout [String]
) {
    var metadata: [String] = []
    if options.includeDisabledState, let isDisabled {
        metadata.append(isDisabled ? "disabled" : "enabled")
    }
    if options.includeSourceLocations {
        metadata.append("`\(sourceLocation.path):\(sourceLocation.line)`")
    }

    guard !metadata.isEmpty else {
        return
    }

    markdown.append("")
    markdown.append("_\(metadata.joined(separator: " · "))_")
    markdown.append("")
}

private func appendRequirements(_ requirements: Reference.Requirements, to markdown: inout [String])
{
    appendRequirementGroup("Given", requirements.given, to: &markdown)
    appendRequirementGroup("When", requirements.when, to: &markdown)
    appendRequirementGroup("Then", requirements.then, to: &markdown)
}

private func appendRequirementGroup(
    _ title: String,
    _ values: [String],
    to markdown: inout [String]
) {
    guard !values.isEmpty else {
        return
    }

    markdown.append("**\(title)**")
    markdown.append("")
    for value in values {
        markdown.append("- \(value)")
    }
    markdown.append("")
}

// MARK: - Private Comment Cleaning

private func cleanLineDocumentation(_ line: String) -> String {
    String(line.dropFirst(3)).removingOneLeadingSpace()
}

private func cleanBlockDocumentation(_ line: String) -> String {
    var line = line.trimmingCharacters(in: .whitespaces)
    if line.hasPrefix("*") {
        line.removeFirst()
    }
    return line.removingOneLeadingSpace()
}

private func cleanOrdinaryComment(_ line: String) -> String {
    String(line.dropFirst(2)).removingOneLeadingSpace()
}

private func trimBlankLines(_ lines: [String]) -> [String] {
    var lines = lines
    while lines.first?.trimmingCharacters(in: .whitespaces).isEmpty == true {
        lines.removeFirst()
    }
    while lines.last?.trimmingCharacters(in: .whitespaces).isEmpty == true {
        lines.removeLast()
    }
    return lines
}

// MARK: - Private String Helpers

extension String {
    fileprivate func removingOneLeadingSpace() -> String {
        if hasPrefix(" ") {
            return String(dropFirst())
        }
        return self
    }

    fileprivate func removingPrefix(_ prefix: String) -> String? {
        guard hasPrefix(prefix) else {
            return nil
        }
        return String(dropFirst(prefix.count)).trimmingCharacters(in: .whitespaces)
    }

    fileprivate func formattingQuotedSpansAsMarkdownCode() -> String {
        var result = ""
        var cursor = startIndex

        while cursor < endIndex {
            guard self[cursor] == "'" else {
                result.append(self[cursor])
                cursor = index(after: cursor)
                continue
            }

            let contentStart = index(after: cursor)
            guard let contentEnd = self[contentStart...].firstIndex(of: "'") else {
                result.append(self[cursor])
                cursor = index(after: cursor)
                continue
            }

            result.append("`")
            result.append(contentsOf: self[contentStart..<contentEnd])
            result.append("`")
            cursor = index(after: contentEnd)
        }

        return result.trimmingCharacters(in: .whitespaces)
    }
}
