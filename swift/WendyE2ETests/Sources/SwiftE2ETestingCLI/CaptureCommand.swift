import ArgumentParser
import Foundation

/// Private entry point for E2ETest.sh's existing execution lifecycle. Public
/// coverage derivation belongs to aggregate, never to execution or report.
struct CaptureCommand: ParsableCommand {
    static let configuration = CommandConfiguration(
        commandName: "_capture",
        abstract: "Internal E2ETest.sh counter capture.",
        shouldDisplay: false
    )
    enum Phase: String, ExpressibleByArgument { case prepare, start, finish }
    @Option var phase: Phase
    @Option var runDir: String
    @Option var repo: String?
    @Option var control: String?
    @Flag var managedAgent = false
    mutating func run() throws {
        let root = URL(fileURLWithPath: runDir)
        switch phase {
        case .prepare:
            try GoCoverage(repo: CoverageIO.repository(repo), run: root).prepare(
                managed: managedAgent
            )
        case .start:
            try GoCoverage(repo: CoverageIO.repository(repo), run: root).startCapture(
                control: control
            )
        case .finish:
            let status = try GoCoverage(repo: CoverageIO.repository(repo), run: root).finishCapture(
                control: control
            )
            if status != 0 { throw ExitCode(status) }
        }
    }
}
