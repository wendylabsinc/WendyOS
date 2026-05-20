import Foundation

// MARK: - Slugs and Titles

func markdownSlug(forTitle title: String, fallback: String) -> String {
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

func referenceBehaviorTitle(documentTitle: String, entryTitle: String) -> String {
    let entryTitle = entryTitle.trimmingCharacters(in: .whitespacesAndNewlines)
    let documentTitle = documentTitle.trimmingCharacters(in: .whitespacesAndNewlines)

    guard let documentCommand = documentTitle.leadingCodeSpan else {
        return entryTitle
    }
    guard !entryTitle.isEmpty else {
        return "`\(documentCommand)`"
    }

    guard let entryCommand = entryTitle.leadingCodeSpan else {
        return "`\(documentCommand)` \(entryTitle)"
    }
    guard !entryCommand.hasPrefix(documentCommand) else {
        return entryTitle
    }

    let remainderStart = entryTitle.index(
        entryTitle.startIndex,
        offsetBy: entryCommand.count + 2
    )
    let remainder = entryTitle[remainderStart...].trimmingCharacters(in: .whitespaces)
    let behaviorCommand = "\(documentCommand) \(entryCommand)"
    if remainder.isEmpty {
        return "`\(behaviorCommand)`"
    }
    return "`\(behaviorCommand)` \(remainder)"
}

// MARK: - String Helpers

extension String {
    var leadingCodeSpan: String? {
        guard first == "`" else {
            return nil
        }

        let contentStart = index(after: startIndex)
        guard let contentEnd = self[contentStart...].firstIndex(of: "`") else {
            return nil
        }

        return String(self[contentStart..<contentEnd])
    }

    func removingOneLeadingSpace() -> String {
        if hasPrefix(" ") {
            return String(dropFirst())
        }
        return self
    }

    func removingPrefix(_ prefix: String) -> String? {
        guard hasPrefix(prefix) else {
            return nil
        }
        return String(dropFirst(prefix.count)).trimmingCharacters(in: .whitespaces)
    }

    func formattingQuotedSpansAsMarkdownCode() -> String {
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
