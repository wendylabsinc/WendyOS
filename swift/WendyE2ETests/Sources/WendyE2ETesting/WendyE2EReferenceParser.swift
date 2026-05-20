import Foundation

// MARK: - Parsing

struct ReferenceSourceParser {
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
    private var currentDocument: WendyE2EReference.Document?
    private var currentSections: [WendyE2EReference.Section] = []
    private var documents: [WendyE2EReference.Document] = []

    init(source: String, path: String) {
        self.lines = source.components(separatedBy: .newlines)
        self.path = path
    }

    mutating func parse() -> [WendyE2EReference.Document] {
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
                currentDocument = WendyE2EReference.Document(
                    title: suiteTitle,
                    overview: pendingSuiteDocumentation ?? "",
                    sections: [],
                    sourceLocation: WendyE2EReference.SourceLocation(path: path, line: index + 1)
                )
                currentSections = []
                pendingSuiteDocumentation = nil
                index += 1
                continue
            }

            if let sectionTitle = parseMarkTitle(from: trimmed), currentDocument != nil {
                currentSections.append(WendyE2EReference.Section(title: sectionTitle))
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
        skipFunctionBody(startingAt: functionLine)
        let entry = WendyE2EReference.Entry(
            title: title,
            documentation: pendingTest.documentation,
            sourceLocation: WendyE2EReference.SourceLocation(path: path, line: functionLine + 1),
            isDisabled: pendingTest.isDisabled
        )

        if currentSections.isEmpty {
            currentSections.append(WendyE2EReference.Section(title: "Overview"))
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

    private mutating func skipFunctionBody(startingAt functionLine: Int) {
        var braceDepth = countBraces(in: lines[functionLine])
        var cursor = functionLine + 1

        while cursor < lines.count, braceDepth > 0 {
            braceDepth += countBraces(in: lines[cursor])
            cursor += 1
        }

        index = cursor
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

// MARK: - Comment Cleaning

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
