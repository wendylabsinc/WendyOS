import Foundation
import Testing

@testable import SwiftE2ETestingCLI

@Suite
struct ReviewAggregateTests {
    @Test(arguments: ["failure", "skipped", "cancelled", ""])
    func `an incomplete review never reads as a clean result`(outcome: String) throws {
        let clean = try aggregate(reviewOutcome: "success")
        let incomplete = try aggregate(reviewOutcome: outcome)
        #expect(incomplete.hasPrefix("# Swift E2E Review\n"))
        #expect(incomplete.contains("AI review did not complete"))
        #expect(!incomplete.contains("No Swift E2E review issues were generated"))
        #expect(incomplete != clean)
    }

    @Test(arguments: ["failure", "skipped", "cancelled"])
    func `files from an incomplete review stay visible beside the notice`(outcome: String) throws {
        let output = try aggregate(reviewOutcome: outcome, withFinding: true)
        #expect(output.contains("AI review did not complete"))
        #expect(output.contains("Fixture finding"))
        #expect(output.contains("Fixture summary."))
        #expect(!output.contains("No Swift E2E review issues were generated"))
    }

    @Test(arguments: ["success", nil] as [String?])
    func `a completed or locally rendered review keeps its usual wording`(outcome: String?) throws {
        #expect(
            try aggregate(reviewOutcome: outcome)
                == "# Swift E2E Review\n\nNo Swift E2E review issues were generated for this run.\n"
        )
        let withFinding = try aggregate(reviewOutcome: outcome, withFinding: true)
        #expect(withFinding.contains("Fixture finding"))
        #expect(!withFinding.contains("did not complete"))
    }
}

private func aggregate(reviewOutcome: String?, withFinding: Bool = false) throws -> String {
    let run = FileManager.default.temporaryDirectory.appendingPathComponent(UUID().uuidString)
    let reviews = run.appendingPathComponent("review.fixture")
    try FileManager.default.createDirectory(at: reviews, withIntermediateDirectories: true)
    defer { try? FileManager.default.removeItem(at: run) }
    if withFinding {
        try """
        ---
        {"schema":"wendy.e2e.review.v1","title":"Fixture finding","scope":"report","reviewer":"fixture","severity":"concern"}
        ---
        # Fixture finding

        Fixture summary.

        ## Details

        Fixture details.
        """.write(
            to: reviews.appendingPathComponent("fixture-finding.md"),
            atomically: true,
            encoding: .utf8
        )
    }
    try writeE2EReviewAggregate(in: run, reviewOutcome: reviewOutcome)
    return try String(contentsOf: run.appendingPathComponent("review.md"), encoding: .utf8)
}
