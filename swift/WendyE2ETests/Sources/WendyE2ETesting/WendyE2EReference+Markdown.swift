import Foundation

extension WendyE2EReference {
    // MARK: - Rendering Markdown

    public static func renderMarkdown(
        _ documents: [Document],
        options: RenderOptions = .reference
    ) -> String {
        documents.map { renderMarkdown($0, options: options) }
            .joined(separator: "\n\n---\n\n")
    }

    public static func markdownFileName(forTitle title: String) -> String {
        "\(markdownSlug(forTitle: title, fallback: "reference")).md"
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
        options: RenderOptions = .reference
    ) -> String {
        var markdown: [String] = []
        markdown.append("# \(document.title)")
        appendParagraph(document.overview, to: &markdown)
        appendMetadata(
            availability: nil,
            sourceLocation: document.sourceLocation,
            options: options,
            to: &markdown
        )

        for section in document.sections where !section.entries.isEmpty {
            markdown.append("## \(section.title)")
            markdown.append("")

            for entry in section.entries {
                let title = referenceBehaviorTitle(
                    documentTitle: document.title,
                    entryTitle: entry.title
                )
                markdown.append("### \(title)")
                appendMetadata(
                    availability: entry.availability,
                    availabilityReason: entry.availabilityReason,
                    sourceLocation: entry.sourceLocation,
                    options: options,
                    to: &markdown
                )
                appendParagraph(entry.documentation, to: &markdown)

            }
        }

        return markdown.joined(separator: "\n").trimmingCharacters(in: .whitespacesAndNewlines)
            + "\n"
    }
}

// MARK: - Markdown Rendering

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
    availability: WendyE2EReference.Availability?,
    availabilityReason: String? = nil,
    sourceLocation: WendyE2EReference.SourceLocation,
    options: WendyE2EReference.RenderOptions,
    to markdown: inout [String]
) {
    var metadata: [String] = []
    if options.includeDisabledState, let availability {
        metadata.append(availability.rawValue)
    }
    if options.includeSourceLocations {
        metadata.append("`\(sourceLocation.path):\(sourceLocation.line)`")
    }

    guard !metadata.isEmpty else {
        return
    }

    markdown.append("")
    markdown.append("_\(metadata.joined(separator: " · "))_")
    if options.includeDisabledState, let availabilityReason {
        let availabilityReason = availabilityReason.components(separatedBy: .newlines).joined(
            separator: " "
        )
        let longestRun =
            availabilityReason.split(whereSeparator: { $0 != "`" }).map(\.count).max() ?? 0
        let delimiter = String(repeating: "`", count: longestRun + 1)
        markdown.append("Availability traits: \(delimiter) \(availabilityReason) \(delimiter)")
    }
    markdown.append("")
}
