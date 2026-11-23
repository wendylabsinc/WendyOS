import Foundation

extension WendyE2EReference {
    /// Compare declarations, not file-level edits or execution outcomes.
    public static func markingChanges(
        in current: [Document],
        comparedTo base: [Document]
    ) throws -> [Document] {
        var original: [String: Entry] = [:]
        for document in base {
            for entry in document.sections.flatMap(\.entries) {
                let identity = document.title + "\u{0}" + entry.title
                guard original.updateValue(entry, forKey: identity) == nil else {
                    throw Error.ambiguousSpec(document.title + " / " + entry.title)
                }
            }
        }
        var result = current
        for d in result.indices {
            for s in result[d].sections.indices {
                for e in result[d].sections[s].entries.indices {
                    var entry = result[d].sections[s].entries[e]
                    if let before = original[result[d].title + "\u{0}" + entry.title] {
                        entry.change =
                            before.declarationSource != entry.declarationSource
                                || before.availability != entry.availability
                                || before.availabilityReason != entry.availabilityReason
                            ? .modified : nil
                    } else {
                        entry.change = .new
                    }
                    result[d].sections[s].entries[e] = entry
                }
            }
        }
        return result
    }
}
