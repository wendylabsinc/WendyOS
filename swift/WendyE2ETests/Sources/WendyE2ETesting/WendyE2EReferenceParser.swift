import Foundation

// MARK: - Parsing

struct ReferenceSourceParser {
    private struct PendingTest {
        var documentation: String
        var availability: StaticAvailability
        var startLine: Int
        var attributeLine: Int
        var attachedDocumentation: String
    }

    private let lines: [String]
    private let lexer: WendyE2EReferenceLexer
    private let path: String

    private var index: Int = 0
    private var pendingDocumentation: String?
    private var pendingDocumentationLine: Int?
    private var pendingSuiteDocumentation: String?
    private var suiteAvailability = StaticAvailability()
    private var pendingSuiteAvailability = StaticAvailability()
    private var suiteTraitRange: ClosedRange<Int>?
    private var pendingSuiteTraitRange: ClosedRange<Int>?
    private var pendingTest: PendingTest?
    private var currentDocument: WendyE2EReference.Document?
    private var currentSections: [WendyE2EReference.Section] = []
    private var documents: [WendyE2EReference.Document] = []
    private(set) var parsingError: WendyE2EReference.Error?

    init(source: String, path: String) {
        self.lines = source.replacingOccurrences(of: "\r\n", with: "\n").components(
            separatedBy: .newlines
        )
        self.path = path
        self.lexer = WendyE2EReferenceLexer(source)
    }

    mutating func parse() -> [WendyE2EReference.Document] {
        while index < lines.count {
            let line = lines[index]
            let trimmed = line.trimmingCharacters(in: .whitespaces)
            let code = lexer.codeLines[index].trimmingCharacters(in: .whitespaces)

            let documentationLine = index
            if let documentation = parseDocumentationComment() {
                pendingDocumentation = documentation
                pendingDocumentationLine = documentationLine
                continue
            }

            if code.hasPrefix("@Suite") {
                let startLine = index + 1
                pendingSuiteDocumentation = pendingDocumentation ?? ""
                pendingDocumentation = nil
                pendingDocumentationLine = nil
                pendingSuiteAvailability = parseAttributeAvailability()
                pendingSuiteTraitRange = startLine...max(startLine, index)
                continue
            }

            if code.hasPrefix("struct "), let suiteTitle = parseSuiteTitle(from: trimmed) {
                finishCurrentDocument()
                currentDocument = WendyE2EReference.Document(
                    title: suiteTitle,
                    overview: pendingSuiteDocumentation ?? "",
                    sections: [],
                    sourceLocation: WendyE2EReference.SourceLocation(path: path, line: index + 1)
                )
                currentSections = []
                suiteAvailability = pendingSuiteAvailability
                suiteTraitRange = pendingSuiteTraitRange
                pendingSuiteAvailability = StaticAvailability()
                pendingSuiteTraitRange = nil
                pendingSuiteDocumentation = nil
                index += 1
                continue
            }

            if let sectionTitle = parseMarkTitle(from: trimmed), currentDocument != nil {
                currentSections.append(WendyE2EReference.Section(title: sectionTitle))
                index += 1
                continue
            }

            if code.hasPrefix("@Test") {
                let attributeLine = index
                let attached =
                    pendingDocumentationLine.map { start in
                        lexer.codeLines[start..<index].allSatisfy {
                            $0.trimmingCharacters(in: .whitespaces).isEmpty
                        }
                    } ?? false
                let startLine = attached ? pendingDocumentationLine! : index
                pendingTest = PendingTest(
                    documentation: pendingDocumentation ?? "",
                    availability: suiteAvailability.merging(parseAttributeAvailability()),
                    startLine: startLine,
                    attributeLine: attributeLine,
                    attachedDocumentation: attached ? pendingDocumentation ?? "" : ""
                )
                pendingDocumentation = nil
                pendingDocumentationLine = nil
                continue
            }

            if code.hasPrefix("func "), let testTitle = parseFunctionTitle(from: trimmed),
                let pendingTest
            {
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
        return parsingError == nil ? documents : []
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
        let end = skipFunctionBody(startingAt: functionLine)
        var entry = WendyE2EReference.Entry(
            title: title,
            documentation: pendingTest.documentation,
            sourceLocation: WendyE2EReference.SourceLocation(path: path, line: functionLine + 1),
            availability: pendingTest.availability.state,
            availabilityReason: pendingTest.availability.reason
        )
        entry.declarationSource =
            pendingTest.attachedDocumentation + "\u{0}"
            + (lexer.spelling(
                of: lexer.tokens.filter {
                    $0.line >= pendingTest.attributeLine && $0.range.upperBound <= end
                }
            ) ?? "")
        entry.declarationStartLine = pendingTest.startLine + 1
        entry.declarationEndLine = index
        entry.inheritedSuiteTraitRange = suiteTraitRange

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

    private mutating func skipFunctionBody(startingAt functionLine: Int) -> Int {
        var depth = 0
        var foundBody = false
        for token in lexer.tokens where token.line >= functionLine {
            if token.text == "{" {
                foundBody = true
                depth += 1
            } else if token.text == "}" {
                depth -= 1
                if foundBody, depth == 0 {
                    index = token.line + 1
                    return token.range.upperBound
                }
            }
        }
        // Incomplete input: do not interpret declarations embedded in an unfinished body.
        index = lines.count
        return lexer.tokens.last?.range.upperBound ?? 0
    }

    private mutating func parseAttributeAvailability() -> StaticAvailability {
        let tokens = lexer.tokens
        guard let start = tokens.firstIndex(where: { $0.line == index && $0.text == "@" }) else {
            index += 1
            return StaticAvailability()
        }
        let opening = start + 2
        guard opening < tokens.count, tokens[opening].text == "(" else {
            index += 1
            return StaticAvailability()
        }
        var depth = 1
        var cursor = opening + 1
        while cursor < tokens.count {
            if tokens[cursor].text == "(" { depth += 1 }
            if tokens[cursor].text == ")" { depth -= 1 }
            if depth == 0 { break }
            cursor += 1
        }
        guard cursor < tokens.count else {
            parsingError = .unterminatedAttribute(path: path, line: index + 1)
            index = lines.count
            return StaticAvailability()
        }
        index = tokens[cursor].line + 1
        return StaticAvailability(traits: Array(tokens[(opening + 1)..<cursor]), lexer: lexer)
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

}

/// Only direct availability traits are recognized. Keep their source spelling rather
/// than guessing values of environment checks, calls, constants or closures.
private struct StaticAvailability {
    var state: WendyE2EReference.Availability = .enabled
    var reason: String?

    init(state: WendyE2EReference.Availability = .enabled, reason: String? = nil) {
        self.state = state
        self.reason = reason
    }

    init(traits: [WendyE2EReferenceLexer.Token], lexer: WendyE2EReferenceLexer) {
        self.init()
        var depth = 0
        var start = 0
        for cursor in 0...traits.count {
            if cursor == traits.count || (traits[cursor].text == "," && depth == 0) {
                let trait = Array(traits[start..<cursor]).map(\.text)
                if trait.count >= 3, trait[0] == ".", ["enabled", "disabled"].contains(trait[1]) {
                    let conditional = trait[1] == "enabled" || !Self.isUnconditionalDisable(trait)
                    self = merging(
                        StaticAvailability(
                            state: conditional ? .conditional : .disabled,
                            reason: lexer.spelling(of: Array(traits[start..<cursor]))
                        )
                    )
                }
                start = cursor + 1
            } else if ["(", "[", "{"].contains(traits[cursor].text) {
                depth += 1
            } else if [")", "]", "}"].contains(traits[cursor].text) {
                depth -= 1
            }
        }
    }

    /// A positional first argument is the comment. A later unlabeled argument
    /// or a trailing closure selects the predicate overload. Keep computed
    /// comments unconditional without evaluating their expression.
    private static func isUnconditionalDisable(_ trait: [String]) -> Bool {
        guard trait[2] == "(", trait.last == ")" else { return false }
        var depth = 0
        var start = 3
        var arguments: [[String]] = []
        for cursor in 3..<(trait.count - 1) {
            let token = trait[cursor]
            if token == ",", depth == 0 {
                arguments.append(Array(trait[start..<cursor]))
                start = cursor + 1
            } else if ["(", "[", "{"].contains(token) {
                depth += 1
            } else if [")", "]", "}"].contains(token) {
                depth -= 1
            }
        }
        arguments.append(Array(trait[start..<(trait.count - 1)]))
        return arguments.enumerated().allSatisfy { position, argument in
            if argument.isEmpty { return true }
            if argument.count >= 3, argument[0] == "sourceLocation", argument[1] == ":" {
                return true
            }
            return position == 0 && (argument.count < 2 || argument[1] != ":")
        }
    }

    func merging(_ other: Self) -> Self {
        let state: WendyE2EReference.Availability
        if self.state == .disabled || other.state == .disabled {
            state = .disabled
        } else if self.state == .conditional || other.state == .conditional {
            state = .conditional
        } else {
            state = .enabled
        }
        let reasons = [reason, other.reason].compactMap { $0 }
        return Self(state: state, reason: reasons.isEmpty ? nil : reasons.joined(separator: "; "))
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
