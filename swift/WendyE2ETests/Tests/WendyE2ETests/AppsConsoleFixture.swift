import Foundation
import Testing
import WendyE2ETesting

let appsBrowserTestsEnabled =
    appsDeploymentTestsEnabled
    && ProcessInfo.processInfo.environment["WENDY_E2E_APPS_BROWSER"] == "true"

struct AppsConsoleObservation: Decodable, Sendable {
    struct Row: Decodable, Sendable {
        let cells: [String]
        let links: [String]
        let titles: [String]
    }
    struct Fact: Decodable, Sendable {
        let panel: String
        let term: String
        let value: String
    }
    let facts: [Fact]
    let alerts: [String]
    let loading: Bool
    let appDetailLoaded: Bool
    let deploymentDetailLoaded: Bool
    let appListLoaded: Bool
    let appListComplete: Bool
    let assetDetailLoaded: Bool
    let url: String
    let text: String
    let headings: [String]
    let rows: [Row]
    let links: [String]
}

extension AppsDeploymentFixture {
    func console(_ cli: WendyE2ESession, path: String) async throws -> AppsConsoleObservation {
        let script = URL(fileURLWithPath: Self.observerPath).deletingLastPathComponent()
            .deletingLastPathComponent().appendingPathComponent(
                ".github/ci-tests/apps-deployments/browser/observe-dom.mjs"
            ).path
        let result = try await succeed(
            cli,
            "node \(Self.quote(script)) \(Self.quote(path)) \(Self.quote("console-" + path.replacingOccurrences(of: "/", with: "-")))"
        )
        let observation = try JSONDecoder().decode(
            AppsConsoleObservation.self,
            from: Data(result.stdout.utf8)
        )
        try #require(
            URL(string: observation.url)?.path == path,
            "The requested page must be present, not a sign-in redirect"
        )
        try #require(
            observation.alerts.isEmpty,
            "Rendered errors cannot establish a page assertion"
        )
        try #require(!observation.loading)
        try #require(!observation.text.contains("Not yours to see"))
        if path == "/apps" {
            try #require(observation.appListLoaded && observation.appListComplete)
        }
        if path.hasPrefix("/assets/") { try #require(observation.assetDetailLoaded) }
        if path.hasPrefix("/apps/") { try #require(observation.appDetailLoaded) }
        if path.hasPrefix("/deployments/") { try #require(observation.deploymentDetailLoaded) }
        return observation
    }

    func requireConsoleDevice(
        _ cli: WendyE2ESession,
        deploymentID: String,
        release: Release,
        state: String
    ) async throws {
        let dom = try await pollConsole(cli, path: "/deployments/\(deploymentID)/devices") { dom in
            dom.rows.contains {
                $0.links.contains("/assets/\(asset)") && $0.cells.count >= 4
                    && $0.cells[1] == release.tag && $0.cells[2] == release.tag
                    && $0.cells[3].lowercased().contains(state)
            }
        }
        let rows = dom.rows.filter { $0.links.contains("/assets/\(asset)") }
        try #require(rows.count == 1, "One exact target must be rendered in the DOM")
        try #require(rows[0].cells.count >= 4)
        #expect(rows[0].cells[1] == release.tag)
        #expect(rows[0].cells[2] == release.tag)
        #expect(rows[0].cells[3].lowercased().contains(state))
    }
    func pollConsole(
        _ cli: WendyE2ESession,
        path: String,
        until condition: @Sendable (AppsConsoleObservation) -> Bool
    ) async throws -> AppsConsoleObservation {
        let deadline = ContinuousClock.now.advanced(by: .seconds(90))
        repeat {
            let dom = try await console(cli, path: path)
            if condition(dom) { return dom }
            try await Task.sleep(for: .seconds(2))
        } while ContinuousClock.now < deadline
        throw Prerequisite(
            description:
                "Rendered Console state did not converge on \(path); last DOM and screenshot retained"
        )
    }

}
