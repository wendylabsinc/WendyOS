import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `reference declaration changes` {
    private let source = """
        @Suite
        struct `example` {
            /// Original guarantee.
            @Test
            func `existing`() {
                let value = 1
                let closingBrace = "}"
            }
            func helper() { let unrelated = 1 }
        }
        """

    @Test
    func `distinguishes new modified and unchanged declarations`() throws {
        let current = source.replacingOccurrences(of: "let value = 1", with: "let value = 2")
            .replacingOccurrences(
                of: "    func helper()",
                with:
                    "    @Test(.disabled(\"later\"))\n    func `new guarantee`() {}\n    func helper()"
            )
        let result = try WendyE2EReference.markingChanges(
            in: WendyE2EReference.parseValidatedSource(current),
            comparedTo: WendyE2EReference.parseValidatedSource(source)
        )
        let entries = result.flatMap(\.sections).flatMap(\.entries)
        #expect(entries.map(\.change) == [.modified, .new])
        #expect(entries.last?.isDisabled == true)
        let html = WendyE2EReference.renderHTML(result)
        #expect(html.contains("<code>MODIFIED</code>"))
        #expect(html.contains("<code>NEW</code>"))
    }

    @Test
    func `helper edits after the final test and line shifts do not modify specs`() throws {
        let current =
            "\n\n" + source.replacingOccurrences(of: "unrelated = 1", with: "unrelated = 2")
        let result = try WendyE2EReference.markingChanges(
            in: WendyE2EReference.parseValidatedSource(current, path: "renamed.swift"),
            comparedTo: WendyE2EReference.parseValidatedSource(source, path: "original.swift")
        )
        #expect(result.flatMap(\.sections).flatMap(\.entries).allSatisfy { $0.change == nil })
    }

    @Test
    func `documentation edits modify only their declaration`() throws {
        let current = source.replacingOccurrences(
            of: "Original guarantee.",
            with: "Updated guarantee."
        )
        let result = try WendyE2EReference.markingChanges(
            in: WendyE2EReference.parseValidatedSource(current),
            comparedTo: WendyE2EReference.parseValidatedSource(source)
        )
        #expect(result.first?.sections.first?.entries.first?.change == .modified)
    }

    @Test
    func `intervening helper content is outside the following declaration`() throws {
        let base = """
            @Suite
            struct `example` {
                /// Helper prose.
                func helper() { let value = 1 }
                @Test
                func `real spec`() {}
            }
            """
        let current = base.replacingOccurrences(of: "Helper prose.", with: "Changed helper prose.")
            .replacingOccurrences(of: "value = 1", with: "value = 2")
        let result = try WendyE2EReference.markingChanges(
            in: WendyE2EReference.parseValidatedSource(current),
            comparedTo: WendyE2EReference.parseValidatedSource(base)
        )
        #expect(result.first?.sections.first?.entries.first?.change == nil)
    }

    @Test
    func `ambiguous identities and malformed base attributes fail clearly`() throws {
        let documents = try WendyE2EReference.parseValidatedSource(source)
        #expect(throws: WendyE2EReference.Error.self) {
            try WendyE2EReference.markingChanges(in: documents, comparedTo: documents + documents)
        }
        #expect(throws: WendyE2EReference.Error.self) {
            try WendyE2EReference.parseValidatedSource(
                "@Suite\nstruct `bad` {\n@Test(.disabled(\"lost\")"
            )
        }
    }
}
