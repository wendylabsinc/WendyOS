import Foundation

/// A bounded lexical scan for the reference extractor, not a Swift syntax parser.
/// Strings (including interpolation), quoted identifiers and comments are atomic:
/// their punctuation must never participate in declaration/body balancing.
// REFACTOR: Replace this custom Swift lexer with an established parser such as SwiftSyntax.
struct WendyE2EReferenceLexer {
    struct Token {
        var text: String
        var line: Int
        var range: Range<Int>
    }

    private let characters: [Character]
    let tokens: [Token]
    let codeLines: [String]

    init(_ source: String) {
        var scanner = Scanner(source.replacingOccurrences(of: "\r\n", with: "\n"))
        scanner.scan()
        characters = scanner.characters
        tokens = scanner.tokens
        codeLines = String(scanner.masked).components(separatedBy: .newlines)
    }

    func spelling(of tokens: [Token]) -> String? {
        guard let first = tokens.first, let last = tokens.last else { return nil }
        return String(characters[first.range.lowerBound..<last.range.upperBound])
    }

    private struct Scanner {
        let characters: [Character]
        var masked: [Character]
        var cursor = 0
        var line = 0
        var tokens: [Token] = []

        init(_ source: String) {
            characters = Array(source)
            masked = characters
        }

        mutating func scan() {
            while cursor < characters.count {
                if skipComment() { continue }
                if characters[cursor].isWhitespace {
                    advance()
                    continue
                }
                let start = cursor
                let startLine = line
                if let delimiter = stringDelimiter() {
                    skipString(hashes: delimiter.hashes, quotes: delimiter.quotes)
                    mask(start..<cursor)
                } else if characters[cursor] == "`" {
                    skipQuotedIdentifier()
                    mask(start..<cursor)
                } else if characters[cursor].isLetter || characters[cursor] == "_" {
                    advance()
                    while cursor < characters.count,
                        characters[cursor].isLetter || characters[cursor].isNumber
                            || characters[cursor] == "_"
                    {
                        advance()
                    }
                } else {
                    advance()
                }
                tokens.append(
                    Token(
                        text: String(characters[start..<cursor]),
                        line: startLine,
                        range: start..<cursor
                    )
                )
            }
        }

        mutating func skipQuotedIdentifier() {
            advance()
            while cursor < characters.count, characters[cursor] != "`" { advance() }
            if cursor < characters.count { advance() }
        }

        mutating func advance(_ count: Int = 1) {
            for _ in 0..<count where cursor < characters.count {
                if characters[cursor].isNewline { line += 1 }
                cursor += 1
            }
        }

        func matches(_ text: String) -> Bool {
            characters[cursor...].starts(with: text)
        }

        mutating func mask(_ range: Range<Int>) {
            for position in range where !characters[position].isNewline { masked[position] = " " }
        }

        mutating func skipComment() -> Bool {
            let start = cursor
            if matches("//") {
                while cursor < characters.count, !characters[cursor].isNewline { advance() }
            } else if matches("/*") {
                advance(2)
                var depth = 1
                while cursor < characters.count, depth > 0 {
                    if matches("/*") {
                        depth += 1
                        advance(2)
                    } else if matches("*/") {
                        depth -= 1
                        advance(2)
                    } else {
                        advance()
                    }
                }
            } else {
                return false
            }
            mask(start..<cursor)
            return true
        }

        func stringDelimiter() -> (hashes: Int, quotes: Int)? {
            var position = cursor
            while position < characters.count, characters[position] == "#" { position += 1 }
            guard position < characters.count, characters[position] == "\"" else { return nil }
            let quotes = characters[position...].starts(with: "\"\"\"") ? 3 : 1
            return (position - cursor, quotes)
        }

        mutating func skipString(hashes: Int, quotes: Int) {
            advance(hashes + quotes)
            let suffix = String(repeating: "#", count: hashes)
            let closing = String(repeating: "\"", count: quotes) + suffix
            let escape = "\\" + suffix
            while cursor < characters.count {
                if matches(closing) {
                    advance(quotes + hashes)
                    return
                }
                if matches(escape) {
                    advance(1 + hashes)
                    if matches("(") {
                        skipInterpolation()
                    } else {
                        advance()
                    }
                } else {
                    advance()
                }
            }
        }

        mutating func skipInterpolation() {
            advance()
            var depth = 1
            while cursor < characters.count, depth > 0 {
                if skipComment() { continue }
                if let delimiter = stringDelimiter() {
                    skipString(hashes: delimiter.hashes, quotes: delimiter.quotes)
                } else if matches("`") {
                    skipQuotedIdentifier()
                } else {
                    if matches("(") { depth += 1 }
                    if matches(")") { depth -= 1 }
                    advance()
                }
            }
        }
    }
}
