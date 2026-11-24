import ArgumentParser
import Foundation
import Testing

@testable import SwiftE2ETestingCLI

@Suite(.serialized)
struct `aggregate Git comparison` {
    static let source = """
        package sample
        func Branch(value string) int {
            if value == "left" {
                value := 1
                // comment inside a covered block
                value++
                return value
            }
            return 0
        }
        """ + "\n"

    func measure(
        _ f: GoCoverageFixture,
        base: String
    ) throws -> (E2EGitComparison, [String: Any], URL) {
        let head = try f.commit()
        try f.buildBinaries(managed: false)
        let native = try f.native("observations/suite/test/coverage/go")
        try f.invoke(native, "left")
        _ = try GoCoverage.profile(
            [native],
            destination: f.run.appendingPathComponent("coverage"),
            source: f.source
        )
        return (
            try E2EGitComparison(repo: f.repo, range: base + ".." + head, testedRevision: head),
            try f.source, f.run.appendingPathComponent("coverage/coverage.out")
        )
    }

    @Test
    func `changed uncovered block and raw literal interior keep zero hits`() throws {
        for raw in [false, true] {
            let f = try GoCoverageFixture(build: false)
            let before =
                raw
                ? "package sample\nfunc Branch(value string) int {\n if value == \"left\" { return 1 }\n value = `first\noriginal\nlast`\n return len(value)\n}\n"
                : Self.source
            try f.write("go/internal/sample/branch.go", before)
            let base = try f.commit()
            try f.write(
                "go/internal/sample/branch.go",
                before.replacingOccurrences(
                    of: raw ? "original" : "return 0",
                    with: raw ? "changed" : "return 7"
                )
            )
            let (git, source, profile) = try measure(f, base: base)
            let result = try git.goCoverage(source: source, profile: profile, complete: true)
            #expect(result["status"] as? String == "collected")
            #expect(result["covered"] as? Int == 0)
            #expect(result["statements"] as? Int == (raw ? 2 : 1))
            #expect(result["percent"] as? Double == 0)
        }
    }

    @Test
    func `several hunks intersecting one real block count its statements only once`() throws {
        let f = try GoCoverageFixture(build: false)
        let expression =
            "        value := 1 +\n"
            + (2..<14).map { "            \($0) +" }.joined(separator: "\n") + "\n            14"
        let before = Self.source.replacingOccurrences(
            of: "        value := 1\n        // comment inside a covered block\n        value++",
            with: expression
        )
        try f.write("go/internal/sample/branch.go", before)
        let base = try f.commit()
        try f.write(
            "go/internal/sample/branch.go",
            before.replacingOccurrences(of: "value := 1 +", with: "value := 2 +")
                .replacingOccurrences(of: "            14", with: "            15")
        )
        let (git, source, profile) = try measure(f, base: base)
        let selected = try git.changedLines(path: "go/internal/sample/branch.go")
        #expect(selected.count == 2)
        let result = try git.goCoverage(source: source, profile: profile, complete: true)
        #expect(result["blocks"] as? Int == 1)
        #expect(result["percent"] as? Double == 100)
        #expect(result["statements"] as? Int == 2)
    }

    @Test
    func `comment and formatting edits deliberately count intersecting blocks`() throws {
        for after in [
            Self.source.replacingOccurrences(
                of: "value := 1",
                with: "value := 1 // inline comment"
            ), Self.source.replacingOccurrences(of: "value := 1", with: "value:=1"),
        ] {
            let f = try GoCoverageFixture(build: false)
            try f.write("go/internal/sample/branch.go", Self.source)
            let base = try f.commit()
            try f.write("go/internal/sample/branch.go", after)
            let (git, source, profile) = try measure(f, base: base)
            let result = try git.goCoverage(source: source, profile: profile, complete: true)
            let evidence =
                "selected=\(try git.changedLines(path: "go/internal/sample/branch.go")) profile=\(try String(contentsOf: profile, encoding: .utf8))"
            #expect(result["blocks"] as? Int == 1, Comment(rawValue: evidence))
            #expect(result["statements"] as? Int == 1, Comment(rawValue: evidence))
            #expect(result["percent"] as? Double == 100)
        }
    }

    @Test
    func `duplicate added bodies follow literal Git ranges and never disappear as no data`() throws
    {
        let f = try GoCoverageFixture(build: false)
        try f.write("go/internal/sample/branch.go", Self.source)
        let base = try f.commit()
        try f.write(
            "go/internal/sample/branch.go",
            Self.source.replacingOccurrences(of: "func Branch", with: "func Added") + "\n"
                + Self.source.components(separatedBy: "\n").dropFirst().joined(separator: "\n")
        )
        let (git, source, profile) = try measure(f, base: base)
        let result = try git.goCoverage(source: source, profile: profile, complete: true)
        #expect(result["status"] as? String == "collected")
        #expect(result["statements"] as? Int ?? 0 > 0)
        // Git decides which duplicate body is added. There is no semantic token
        // alignment against another declaration and no causal attribution claim.
        #expect(result["percent"] as? Double != nil)
    }

    @Test
    func
        `unlinked simulator and scripts code is explicitly incomplete and exclusions remain per file`()
        throws
    {
        let f = try GoCoverageFixture(build: false)
        let base = try f.git(["rev-parse", "HEAD"])
        for path in ["go/simulator/new.go", "go/scripts/new.go"] {
            try f.write(path, "package production\nfunc New() int { return 1 }\n")
        }
        let (git, source, profile) = try measure(f, base: base)
        let missing = try git.goCoverage(source: source, profile: profile, complete: true)
        #expect(missing["status"] as? String == "incomplete")
        #expect(missing["percent"] is NSNull)
        #expect(
            missing["missingFiles"] as? [String] == ["go/scripts/new.go", "go/simulator/new.go"]
        )
        let excluded = try GoCoverageFixture(build: false)
        let original = try excluded.git(["rev-parse", "HEAD"])
        for (path, text) in [
            "go/internal/sample/unit_test.go": "package sample\nfunc TestOnly() int {return 9}\n",
            "go/internal/sample/gen.go":
                "// Code generated by fixture. DO NOT EDIT.\npackage sample\nfunc Gen() int {return 9}\n",
            "go/internal/sample/attribute.go": "package sample\nfunc Attr() int {return 9}\n",
            "go/internal/sample/testdata/helper.go": "package data\nfunc New() int {return 9}\n",
            "go/internal/testfixture/helper.go":
                "package fixture\nfunc Helper() int { return 9 }\n",
            "Examples/main.go": "package main\nfunc main() {}\n",
        ] { try excluded.write(path, text) }
        try excluded.write(
            ".gitattributes",
            "go/internal/sample/attribute.go linguist-generated=true\n"
        )
        let (comp, files, output) = try measure(excluded, base: original)
        #expect(
            try comp.goCoverage(source: files, profile: output, complete: true)["status"] as? String
                == "not-applicable"
        )
    }

    @Test
    func `deleted lines pure rename empty comparison and wrong revision stay honest`() throws {
        for rename in [false, true] {
            let f = try GoCoverageFixture(build: false)
            try f.write("go/internal/sample/branch.go", Self.source)
            let base = try f.commit()
            if rename {
                try FileManager.default.moveItem(
                    at: f.repo.appendingPathComponent("go/internal/sample/branch.go"),
                    to: f.repo.appendingPathComponent("go/internal/sample/renamed.go")
                )
            } else {
                try f.write(
                    "go/internal/sample/branch.go",
                    Self.source.replacingOccurrences(of: "        value++\n", with: "")
                )
            }
            let (git, source, profile) = try measure(f, base: base)
            #expect(
                try git.goCoverage(source: source, profile: profile, complete: true)["status"]
                    as? String == "not-applicable"
            )
            let same = try E2EGitComparison(
                repo: f.repo,
                range: git.head + ".." + git.head,
                testedRevision: git.head
            )
            #expect(
                try same.goCoverage(source: source, profile: profile, complete: true)["status"]
                    as? String == "not-applicable"
            )
            #expect(throws: (any Error).self) {
                try E2EGitComparison(
                    repo: f.repo,
                    range: "missing...HEAD",
                    testedRevision: git.head
                )
            }
            #expect(throws: (any Error).self) {
                try E2EGitComparison(
                    repo: f.repo,
                    range: base + ".." + base,
                    testedRevision: git.head
                )
            }
        }
    }

    @Test
    func `spec declarations capture Added Removed Changed traits and ignore helper-only edits`()
        throws
    {
        let f = try GoCoverageFixture(build: false)
        let path = "swift/WendyE2ETests/Tests/WendyE2ETests/Spec.swift"
        let before = """
            @Suite(.disabled("waiting"))
            struct `'wendy example'` {
                /// Guarantee.
                @Test
                func `existing`() { let value = 1 }
                @Test
                func `removed`() {}
                func helper() { let helper = 1 }
            }
            """
        try f.write(path, before)
        let base = try f.commit()
        let after = before.replacingOccurrences(
            of: ".disabled(\"waiting\")",
            with: ".enabled(if: true)"
        )
        .replacingOccurrences(of: "func `removed`() {}", with: "func `added`() {}")
        .replacingOccurrences(of: "helper = 1", with: "helper = 2")
        try f.write(path, after)
        let head = try f.commit()
        let git = try E2EGitComparison(
            repo: f.repo,
            range: base + "..." + head,
            testedRevision: head
        )
        let entries = try git.specs()
        #expect(Set(entries.map(\.change)) == ["Added", "Removed", "Changed"])
        #expect(entries.first { $0.name == "existing" }?.change == "Changed")
        #expect(entries.first { $0.name == "removed" }?.change == "Removed")
        try f.write(path, after.replacingOccurrences(of: "helper = 2", with: "helper = 3"))
        let helperHead = try f.commit()
        let helper = try E2EGitComparison(
            repo: f.repo,
            range: head + ".." + helperHead,
            testedRevision: helperHead
        )
        #expect(try helper.specs().isEmpty)
        try f.write(path, "@Suite\nstruct `bad` {\n@Test(.disabled(\"lost\")")
        let badHead = try f.commit()
        let bad = try E2EGitComparison(
            repo: f.repo,
            range: head + ".." + badHead,
            testedRevision: badHead
        )
        #expect(throws: (any Error).self) { try bad.specs() }
    }
}

@Suite
struct `aggregate declaration boundaries` {
    @Test
    func
        `attached docs change a spec while intervening helper prose does not and duplicate identities fail`()
        throws
    {
        let f = try GoCoverageFixture(build: false)
        let path = "swift/WendyE2ETests/Tests/WendyE2ETests/BoundaryTests.swift"
        let before = """
            @Suite
            struct `example` {
                /// Helper prose.
                func helper() { let value = 1 }
                /// Original guarantee.
                @Test
                func `existing`() {}
            }
            """
        try f.write(path, before)
        let base = try f.commit()
        let docs = before.replacingOccurrences(
            of: "Original guarantee.",
            with: "Updated guarantee."
        )
        try f.write(path, docs)
        let head = try f.commit()
        let changed = try E2EGitComparison(
            repo: f.repo,
            range: base + ".." + head,
            testedRevision: head
        ).specs()
        #expect(changed.count == 1 && changed[0].change == "Changed")
        let helper = docs.replacingOccurrences(of: "Helper prose.", with: "Updated helper prose.")
            .replacingOccurrences(of: "value = 1", with: "value = 2")
        try f.write(path, helper)
        let helperHead = try f.commit()
        #expect(
            try E2EGitComparison(
                repo: f.repo,
                range: head + ".." + helperHead,
                testedRevision: helperHead
            ).specs().isEmpty
        )
        try f.write(
            path,
            helper.replacingOccurrences(
                of: "func `existing`() {}",
                with: "func `existing`() {}\n    @Test\n    func `existing`() {}"
            )
        )
        let duplicate = try f.commit()
        #expect(throws: (any Error).self) {
            try E2EGitComparison(
                repo: f.repo,
                range: head + ".." + duplicate,
                testedRevision: duplicate
            ).specs()
        }
    }
}
