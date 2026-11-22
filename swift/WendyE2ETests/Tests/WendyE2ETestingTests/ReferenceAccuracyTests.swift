import Foundation
import Testing
import WendyE2ETesting

@Suite
struct `reference accuracy` {
    @Test
    func `retains actual auth status declarations after malformed JSON shell literals`() throws {
        // Unmodified source from the reported unchanged-parent reproduction, at
        // 597135afe02867325b5073c8d0617238377aa712. It is parsed, never executed.
        let url = try #require(
            Bundle.module.url(
                forResource: "WendyAuthStatusTests.swift",
                withExtension: "txt",
                subdirectory: "Fixtures"
            )
        )
        let source = try String(contentsOf: url, encoding: .utf8)
        let document = try #require(
            WendyE2EReference.parseSource(source, path: "WendyAuthStatusTests.swift").first
        )
        let entries = document.sections.flatMap(\.entries)
        #expect(entries.count == 8)
        #expect(
            entries.suffix(3).map(\.title) == [
                "reports invalid CLI configuration before acting",
                "rejects undocumented flags",
                "rejects undocumented positional arguments",
            ]
        )
        #expect(
            entries.map(\.availability) == [
                .enabled, .enabled, .enabled, .disabled, .disabled, .enabled, .enabled, .disabled,
            ]
        )
        #expect(entries.last?.availabilityReason?.contains("WDY-1934") == true)
        let lines = source.components(separatedBy: .newlines)
        for entry in entries {
            #expect(entry.sourceLocation.path == "WendyAuthStatusTests.swift")
            #expect(lines[entry.sourceLocation.line - 1].contains("func `"))
        }
        #expect(entries.last?.sourceLocation.line == 186)
    }

    @Test
    func `balances bodies across supported string and comment forms`() throws {
        let source = ####"""
            /* Outer comment {
               /* nested } */
               @Test
               func `not a declaration`() {}
            */
            @Suite
            struct `lexical controls` {
                @Test
                func `contains punctuation`() async throws
                {
                    let normal = "{ \" escaped quote } \\"
                    let raw = #"{ \" \\ }"#
                    let manyHashes = ###"{"###
                    let multiline = """
                        { // not a comment
                        /* not a comment }
                        """
                    let rawMultiline = ##"""
                        { " """ /*
                        """##
                    let interpolation = "{ \(call("nested {", other: #"}"#))"
                    let rawInterpolation = ##"{ \##(call("}", other: "("))"##
                    let quotedInterpolation = "\(self.`default`())"
                    // } "
                    /* { /* } */ { */
                    if true { call() }
                }
                /** Following declaration retains its documentation. */
                @Test
                func `follows punctuation`() {}
            }
            """####
        let document = try #require(WendyE2EReference.parseSource(source).first)
        let entries = document.sections.flatMap(\.entries)
        #expect(entries.map(\.title) == ["contains punctuation", "follows punctuation"])
        #expect(entries.last?.documentation == "Following declaration retains its documentation.")
        let followingLine =
            try #require(
                source.components(separatedBy: .newlines).firstIndex(where: {
                    $0.contains("func `follows punctuation`")
                })
            ) + 1
        #expect(entries.last?.sourceLocation.line == followingLine)
    }

    @Test
    func `reads complete availability traits without evaluating conditions`() throws {
        let document = try #require(WendyE2EReference.parseSource(Self.availabilitySource).first)
        let entries = document.sections.flatMap(\.entries)
        #expect(
            entries.map(\.availability) == [
                .disabled, .conditional, .conditional, .disabled, .enabled, .conditional,
            ]
        )
        #expect(entries.map(\.isDisabled) == [true, false, false, true, false, false])
        #expect(entries[0].availabilityReason?.contains("backend fix") == true)
        #expect(entries[1].availabilityReason?.contains("FirstAppFixture.pythonWheelhouse") == true)
        #expect(entries[1].availabilityReason?.contains("WENDY_E2E_PYTHON_WHEELHOUSE") == true)
        #expect(entries[2].availabilityReason?.contains("if: true") == true)
        #expect(entries[3].availabilityReason?.contains("always disabled") == true)
        #expect(entries[4].availabilityReason == nil)
    }

    @Test
    func `distinguishes disabling overloads without evaluating computed arguments`() throws {
        let documents = WendyE2EReference.parseSource(
            #"""
            @Suite
            struct `overloads` {
                @Test(.disabled())
                func `default comment`() {}
                @Test(.disabled(nil))
                func `nil comment`() {}
                @Test(.disabled("reason", sourceLocation: location()))
                func `literal comment and source location`() {}
                @Test(.disabled("reason") { predicate() })
                func `trailing predicate with comment`() {}
                @Test(.disabled("reason", { predicate() }))
                func `predicate inside parentheses`() {}
                @Test(.disabled("reason", predicate))
                func `computed predicate with comment`() {}
                @Test(.disabled(comment()))
                func `computed comment remains unconditional`() {}
            }
            """#
        )
        let entries = documents.flatMap(\.sections).flatMap(\.entries)
        #expect(
            entries.map(\.availability) == [
                .disabled, .disabled, .disabled, .conditional, .conditional, .conditional,
                .disabled,
            ]
        )
    }

    @Test
    func `rejects unterminated attributes without returning earlier entries`() throws {
        for attribute in ["Test", "Suite"] {
            let source = """
                @Suite
                struct `controls` {
                    @Test
                    func `first`() {}
                    @\(attribute)(.disabled("reason")
                    func `lost`() {}
                }
                """
            #expect(WendyE2EReference.parseSource(source).isEmpty)
            let directory = FileManager.default.temporaryDirectory.appendingPathComponent(
                UUID().uuidString
            )
            try FileManager.default.createDirectory(
                at: directory,
                withIntermediateDirectories: true
            )
            defer { try? FileManager.default.removeItem(at: directory) }
            let file = directory.appendingPathComponent("unterminated.swift")
            try source.write(to: file, atomically: true, encoding: .utf8)
            let error = WendyE2EReference.Error.unterminatedAttribute(path: file.path, line: 5)
            #expect(throws: error) { try WendyE2EReference.parseFile(at: file.path) }
            #expect(throws: error) { try WendyE2EReference.parseDirectory(at: directory.path) }
            #expect(error.description == "\(file.path):5: unterminated reference attribute")
        }
    }

    @Test
    func `retains generic trait source with explicitly limited comma classification`() throws {
        // The ungrouped generic comma is outside the bounded expression scanner.
        // Its conservative unknown state must retain the original expression;
        // grouping the same expression makes argument boundaries unambiguous.
        let documents = WendyE2EReference.parseSource(
            """
            @Suite
            struct `generic reason` {
                @Test(.disabled(Reason<Int, String>.comment))
                func `computed generic comment`() {}
                @Test(.disabled((Reason<Int, String>.comment)))
                func `grouped generic comment`() {}
                @Test(.disabled("literal comment"))
                func `literal comment`() {}
            }
            """
        )
        let entries = documents.flatMap(\.sections).flatMap(\.entries)
        #expect(entries.count == 3)
        #expect(entries.first?.availability != .enabled)
        #expect(entries.dropFirst().map(\.availability) == [.disabled, .disabled])
        #expect(entries.first?.availabilityReason == ".disabled(Reason<Int, String>.comment)")
        #expect(
            entries.map(\.title) == [
                "computed generic comment", "grouped generic comment", "literal comment",
            ]
        )
    }

    @Test
    func `preserves source lines with CRLF input`() throws {
        let source = Self.availabilitySource.replacingOccurrences(of: "\n", with: "\r\n")
        let entries = WendyE2EReference.parseSource(source).flatMap(\.sections).flatMap(\.entries)
        let original = WendyE2EReference.parseSource(Self.availabilitySource).flatMap(\.sections)
            .flatMap(\.entries)
        #expect(entries == original)
    }

    @Test
    func `inherits suite availability and resets it between documents`() throws {
        let documents = WendyE2EReference.parseSource(
            """
            @Suite(
                .enabled(if: environmentCheck(), "Suite prerequisite")
            )
            struct `conditional suite` {
                @Test
                func `inherits condition`() {}
                @Test(.disabled("Unimplemented"))
                func `remains disabled`() {}
            }
            @Suite
            struct `next suite` {
                @Test
                func `has no availability traits`() {}
            }
            """
        )
        let entries = documents.flatMap(\.sections).flatMap(\.entries)
        #expect(entries.map(\.availability) == [.conditional, .disabled, .enabled])
        #expect(entries.first?.availabilityReason?.contains("Suite prerequisite") == true)
    }

    @Test
    func `renders conditional metadata and preserves known failure caveats in every format`() throws
    {
        let document = try #require(
            WendyE2EReference.parseSource(Self.availabilitySource, path: "Traits.swift").first
        )
        let markdown = WendyE2EReference.renderMarkdown(document, options: .specReview)
        let html = WendyE2EReference.renderHTML(document, options: .specReview)
        let json = try WendyE2EReference.renderJSON(document, options: .specReview)
        for rendered in [markdown, html, json] {
            #expect(rendered.contains("conditional"))
            #expect(rendered.contains("WENDY_E2E_PYTHON_WHEELHOUSE"))
            #expect(rendered.contains("Known failure remains visible."))
            #expect(rendered.contains("Traits.swift"))
        }
        #expect(markdown.contains("_conditional · `Traits.swift:"))
        #expect(html.contains("conditional · <code>Traits.swift:"))
        let objects = try #require(
            JSONSerialization.jsonObject(with: Data(json.utf8)) as? [[String: Any]]
        )
        let sections = try #require(objects.first?["sections"] as? [[String: Any]])
        let entries = try #require(sections.first?["entries"] as? [[String: Any]])
        #expect(entries[0]["isDisabled"] as? Bool == true)
        #expect(entries[1]["isDisabled"] == nil)
        #expect(entries[1]["availability"] as? String == "conditional")
        #expect(entries[4]["isDisabled"] as? Bool == false)
        #expect(!WendyE2EReference.renderMarkdown(document).contains("Availability traits:"))
        #expect(!WendyE2EReference.renderHTML(document).contains("Availability traits:"))
        #expect(try !WendyE2EReference.renderJSON(document).contains("\"availability\""))
    }

    @Test
    func `preserves the legacy initializer and mutable disabled view`() {
        var entry = WendyE2EReference.Entry(
            title: "test",
            documentation: "",
            sourceLocation: .init(path: "file", line: 1),
            isDisabled: true
        )
        #expect(entry.availability == .disabled)
        entry.isDisabled = false
        #expect(entry.availability == .enabled)
        entry.availability = .conditional
        entry.availabilityReason = "environment"
        entry.isDisabled = true
        #expect(entry.availability == .disabled)
        #expect(entry.availabilityReason == nil)
    }

    // The Python attribute is copied from the real first-app source. Controls also
    // ensure that trait-like text inside arguments or comments is not executed.
    private static let availabilitySource = #"""
        @Suite
        struct `traits` {
            /** Known failure remains visible. */
            @Test(
                .disabled(
                    "Waiting for backend fix"
                )
            )
            func `disabled across lines`() {}
            @Test(
                .enabled(
                    if: FirstAppFixture.pythonWheelhouse != nil,
                    "FIXTURE: set WENDY_E2E_PYTHON_WHEELHOUSE to a provisioned offline Hatchling wheel directory."
                )
            )
            func `requires wheelhouse`() {}
            @Test(.disabled(if: true, "Do not evaluate even literal expressions"))
            func `conditional disabling`() {}
            @Test(.enabled(if: check(".disabled(", [1, 2])), .disabled("always disabled"))
            func `unconditional disable wins`() {}
            @Test(".disabled(if: true)", .bug(".enabled(if: false)")) // .disabled("comment")
            func `trait text is not a trait`() {}
            @Test(.enabled { fixtureIsReady() })
            func `closure condition`() {}
        }
        """#
}
