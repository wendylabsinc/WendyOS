import Testing
import WendyE2ETesting

@Suite
struct `reference documentation extraction` {
    @Test
    func `parses suite overview and title`() throws {
        let documents = Reference.parseSource(Self.fixtureSource, path: "DeviceInfoTests.swift")

        let document = try #require(documents.first)
        #expect(document.title == "`wendy device info`")
        #expect(document.overview.contains("Shows information reported by a Wendy agent."))
        #expect(document.overview.contains("Synopsis:"))
        #expect(document.sourceLocation.path == "DeviceInfoTests.swift")
        #expect(document.sourceLocation.line == 9)
    }

    @Test
    func `extracts mark sections in source order`() throws {
        let document = try #require(Reference.parseSource(Self.fixtureSource).first)

        #expect(
            document.sections.map(\.title) == [
                "Selecting Devices",
                "Printing Output",
            ]
        )
    }

    @Test
    func `extracts test entries into their containing sections`() throws {
        let document = try #require(Reference.parseSource(Self.fixtureSource).first)
        let selectingDevices = try #require(document.sections.first)
        let printingOutput = try #require(document.sections.dropFirst().first)

        #expect(
            selectingDevices.entries.map(\.title) == [
                "`--device` selects an explicit device",
                "uses the configured default device",
            ]
        )
        #expect(
            printingOutput.entries.map(\.title) == [
                "`--json --device` prints JSON device information"
            ]
        )
    }

    @Test
    func `extracts test documentation`() throws {
        let entry = try #require(
            Reference.parseSource(Self.fixtureSource).first?.sections.first?.entries.first
        )

        #expect(entry.documentation.contains("Selects a device explicitly with `--device`."))
        #expect(
            entry.documentation.contains("Use this form when the target device is already known.")
        )
    }

    @Test
    func `extracts disabled test state`() throws {
        let document = try #require(Reference.parseSource(Self.fixtureSource).first)
        let entries = document.sections.flatMap(\.entries)

        #expect(entries.map(\.isDisabled) == [true, true, false])
    }

    @Test
    func `extracts given when then requirements`() throws {
        let entry = try #require(
            Reference.parseSource(Self.fixtureSource).first?.sections.first?.entries.first
        )

        #expect(
            entry.requirements.given == [
                "a reachable Wendy agent address",
                "the CLI has an isolated HOME",
            ]
        )
        #expect(
            entry.requirements.when == [
                "`wendy device info --device <device>` is run"
            ]
        )
        #expect(
            entry.requirements.then == [
                "exits successfully",
                "connects to the selected device",
                "does not open the device picker",
                "prints device information",
            ]
        )
    }

    @Test
    func `parses multiple suites in one source file`() throws {
        let documents = Reference.parseSource(Self.fixtureSource)

        #expect(
            documents.map(\.title) == [
                "`wendy device info`",
                "`wendy device version`",
            ]
        )
        #expect(documents.last?.sections.first?.title == "Compatibility")
        #expect(documents.last?.sections.first?.entries.first?.title == "aliases device info")
    }

    @Test
    func `creates an overview section for tests before the first mark`() throws {
        let documents = Reference.parseSource(Self.fixtureWithoutMark)

        let document = try #require(documents.first)
        #expect(document.sections.map(\.title) == ["Overview"])
        #expect(document.sections.first?.entries.first?.title == "prints help")
    }

    @Test
    func `renders reference markdown without requirements or metadata`() throws {
        let document = try #require(Reference.parseSource(Self.fixtureSource).first)
        let markdown = Reference.renderMarkdown(document, options: .reference)

        #expect(markdown.contains("# `wendy device info`"))
        #expect(markdown.contains("## Selecting Devices"))
        #expect(markdown.contains("### `--device` selects an explicit device"))
        #expect(markdown.contains("Selects a device explicitly with `--device`."))
        #expect(!markdown.contains("#### Requirements"))
        #expect(!markdown.contains("_disabled"))
        #expect(!markdown.contains("<memory>:"))
    }

    @Test
    func `renders spec review markdown with requirements and metadata`() throws {
        let document = try #require(
            Reference.parseSource(Self.fixtureSource, path: "DeviceInfoTests.swift").first
        )
        let markdown = Reference.renderMarkdown(document, options: .specReview)

        #expect(markdown.contains("_`DeviceInfoTests.swift:9`_"))
        #expect(markdown.contains("_disabled · `DeviceInfoTests.swift:18`_"))
        #expect(markdown.contains("_enabled · `DeviceInfoTests.swift:46`_"))
        #expect(markdown.contains("#### Requirements"))
        #expect(markdown.contains("**Given**"))
        #expect(markdown.contains("- a reachable Wendy agent address"))
        #expect(markdown.contains("**When**"))
        #expect(markdown.contains("- `wendy device info --device <device>` is run"))
        #expect(markdown.contains("**Then**"))
        #expect(markdown.contains("- prints device information"))
    }

    @Test
    func `renders multiple documents separated by a thematic break`() {
        let documents = Reference.parseSource(Self.fixtureSource)
        let markdown = Reference.renderMarkdown(documents, options: .reference)

        #expect(markdown.contains("# `wendy device info`"))
        #expect(markdown.contains("\n\n---\n\n# `wendy device version`"))
    }

    @Test
    func `dasherizes document titles for markdown file names`() {
        #expect(
            Reference.markdownFileName(forTitle: "`wendy device info`") == "wendy-device-info.md"
        )
        #expect(Reference.markdownFileName(forTitle: "wendy --version") == "wendy-version.md")
        #expect(
            Reference.markdownAnchor(forTitle: "`wendy device version`") == "wendy-device-version"
        )
    }

    @Test
    func `renders markdown index entries`() {
        let markdown = Reference.renderMarkdownIndex(
            [
                Reference.IndexEntry(
                    title: "`wendy device info`",
                    fileName: "wendy-device-info.md"
                ),
                Reference.IndexEntry(
                    title: "`wendy device version`",
                    fileName: "wendy-device-info.md",
                    anchor: "wendy-device-version"
                ),
                Reference.IndexEntry(title: "wendy help", fileName: "wendy-help.md"),
            ],
            title: "Wendy E2E Reference"
        )

        #expect(markdown.contains("# Wendy E2E Reference"))
        #expect(markdown.contains("- [`wendy device info`](wendy-device-info.md)"))
        #expect(
            markdown.contains(
                "- [`wendy device version`](wendy-device-info.md#wendy-device-version)"
            )
        )
        #expect(markdown.contains("- [wendy help](wendy-help.md)"))
    }

    private static let fixtureWithoutMark = """
        /**
         Shows help.
         */
        @Suite
        struct `'wendy help'` {
            /**
             Prints top-level help.
             */
            @Test(.disabled("SPEC STUB"))
            func `prints help`() async throws {
                // Given: a CLI binary
                // When: `wendy help` is run
                // Then:
                // - exits successfully
            }
        }
        """

    private static let fixtureSource = """
        /**
         Shows information reported by a Wendy agent.

         Synopsis:

         `wendy [--device DEVICE] device info`
         */
        @Suite(.serialized)
        struct `'wendy device info'` {
            // MARK: - Selecting Devices

            /**
             Selects a device explicitly with `--device`.

             Use this form when the target device is already known.
             */
            @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
            func `'--device' selects an explicit device`() async throws {
                // Given: a reachable Wendy agent address
                // And: the CLI has an isolated HOME
                // When: `wendy device info --device <device>` is run
                // Then:
                // - exits successfully
                // - connects to the selected device
                // - does not open the device picker
                // - prints device information
            }

            /**
             Uses the configured default device.
             */
            @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
            func `uses the configured default device`() async throws {
                // Given: a reachable default device is configured
                // When: `wendy device info` is run
                // Then:
                // - exits successfully
            }

            // MARK: - Printing Output

            /**
             Prints JSON device information.
             */
            @Test
            func `'--json --device' prints JSON device information`() async throws {
                // Given: a reachable Wendy agent
                // When: `wendy --json device info --device <device>` is run
                // Then:
                // - emits one JSON object
            }
        }

        /**
         Deprecated compatibility alias for `wendy device info`.
         */
        @Suite(.serialized)
        struct `'wendy device version'` {
            // MARK: - Compatibility

            /**
             Preserves compatibility for existing scripts.
             */
            @Test(.disabled("SPEC STUB: behavior agreed, implementation pending"))
            func `aliases device info`() async throws {
                // Given: a reachable Wendy agent
                // When: `wendy device version --device <device>` is run
                // Then:
                // - exits successfully
            }
        }
        """
}
