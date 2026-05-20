import Foundation

extension WendyE2EReference {
    // MARK: - Rendering JSON

    public static func jsonFileName(forTitle title: String) -> String {
        "\(markdownSlug(forTitle: title, fallback: "reference")).json"
    }

    public static func renderJSON(
        _ documents: [Document],
        options: RenderOptions = .reference
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
        options: RenderOptions = .reference
    ) throws -> String {
        try renderJSON([document], options: options)
    }
}

// MARK: - JSON Types

private struct JSONIndex: Encodable {
    var title: String
    var entries: [JSONIndexEntry]
}

private struct JSONIndexEntry: Encodable {
    var title: String
    var fileName: String
    var anchor: String?

    init(_ entry: WendyE2EReference.IndexEntry) {
        self.title = entry.title
        self.fileName = entry.fileName
        self.anchor = entry.anchor
    }
}

private struct JSONDocument: Encodable {
    var title: String
    var overview: String
    var sourceLocation: WendyE2EReference.SourceLocation?
    var sections: [JSONSection]

    init(_ document: WendyE2EReference.Document, options: WendyE2EReference.RenderOptions) {
        self.title = document.title
        self.overview = document.overview
        self.sourceLocation = options.includeSourceLocations ? document.sourceLocation : nil
        self.sections = document.sections.map {
            JSONSection($0, documentTitle: document.title, options: options)
        }
    }
}

private struct JSONSection: Encodable {
    var title: String
    var entries: [JSONEntry]

    init(
        _ section: WendyE2EReference.Section,
        documentTitle: String,
        options: WendyE2EReference.RenderOptions
    ) {
        self.title = section.title
        self.entries = section.entries.map {
            JSONEntry($0, documentTitle: documentTitle, options: options)
        }
    }
}

private struct JSONEntry: Encodable {
    var title: String
    var documentation: String
    var sourceLocation: WendyE2EReference.SourceLocation?
    var isDisabled: Bool?

    init(
        _ entry: WendyE2EReference.Entry,
        documentTitle: String,
        options: WendyE2EReference.RenderOptions
    ) {
        self.title = referenceBehaviorTitle(
            documentTitle: documentTitle,
            entryTitle: entry.title
        )
        self.documentation = entry.documentation
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
