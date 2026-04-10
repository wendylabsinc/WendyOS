import Testing

@testable import wendy_agent

@Suite("PermissionSetupRunner")
struct PermissionSetupTests {
    @Test("already granted permissions are reported without prompting")
    func grantedPermissionsDoNotPrompt() async throws {
        let authorizer = FakePermissionAuthorizer(statuses: [
            .camera: .granted,
            .microphone: .granted,
            .bluetooth: .granted,
        ])
        let runner = PermissionSetupRunner(authorizer: authorizer)

        let outcome = await runner.run()

        #expect(outcome.summaries == [
            .init(permission: .camera, status: .granted),
            .init(permission: .microphone, status: .granted),
            .init(permission: .bluetooth, status: .granted),
        ])
        #expect(await authorizer.requestedPermissions() == [])
    }

    @Test("missing permissions are prompted and summarized with their final status")
    func missingPermissionsArePrompted() async throws {
        let authorizer = FakePermissionAuthorizer(statuses: [
            .camera: .missing,
            .microphone: .granted,
            .bluetooth: .missing,
        ])
        await authorizer.setPromptResult(.camera, status: .granted)
        await authorizer.setPromptResult(.bluetooth, status: .missing)

        let runner = PermissionSetupRunner(authorizer: authorizer)
        let outcome = await runner.run()

        #expect(outcome.summaries == [
            .init(permission: .camera, status: .granted),
            .init(permission: .microphone, status: .granted),
            .init(permission: .bluetooth, status: .missing),
        ])
        #expect(await authorizer.requestedPermissions() == [.camera, .bluetooth])
    }

    @Test("unknown permissions still trigger best-effort prompting")
    func unknownPermissionsStillPrompt() async throws {
        let authorizer = FakePermissionAuthorizer(statuses: [
            .camera: .unknown,
            .microphone: .granted,
            .bluetooth: .unknown,
        ])
        await authorizer.setPromptResult(.camera, status: .unknown)
        await authorizer.setPromptResult(.bluetooth, status: .granted)

        let runner = PermissionSetupRunner(authorizer: authorizer)
        let outcome = await runner.run()

        #expect(outcome.summaries == [
            .init(permission: .camera, status: .unknown),
            .init(permission: .microphone, status: .granted),
            .init(permission: .bluetooth, status: .granted),
        ])
        #expect(await authorizer.requestedPermissions() == [.camera, .bluetooth])
    }

    @Test("warnings cover one permission per missing or unknown summary")
    func warningsCoverMissingAndUnknownSummaries() async throws {
        let outcome = PermissionSetupOutcome(summaries: [
            .init(permission: .camera, status: .missing),
            .init(permission: .microphone, status: .unknown),
            .init(permission: .bluetooth, status: .granted),
        ])

        #expect(outcome.warningLines == [
            "Warning: camera permission is missing. Run 'wendy-agent setup' to retry permission onboarding.",
            "Warning: microphone permission is unknown. Run 'wendy-agent setup' to retry permission onboarding.",
        ])
    }

    @Test("summary lines are one permission per line in setup order")
    func summaryLinesAreStable() async throws {
        let outcome = PermissionSetupOutcome(summaries: [
            .init(permission: .camera, status: .granted),
            .init(permission: .microphone, status: .missing),
            .init(permission: .bluetooth, status: .unknown),
        ])

        #expect(outcome.summaryLines == [
            "camera: granted",
            "microphone: missing",
            "bluetooth: unknown",
        ])
    }
}

private actor FakePermissionAuthorizer: PermissionAuthorizing {
    private var statuses: [PermissionKind: PermissionStatus]
    private var promptResults: [PermissionKind: PermissionStatus] = [:]
    private var requested: [PermissionKind] = []

    init(statuses: [PermissionKind: PermissionStatus]) {
        self.statuses = statuses
    }

    func status(for permission: PermissionKind) async -> PermissionStatus {
        statuses[permission, default: .unknown]
    }

    func requestAccess(for permission: PermissionKind) async -> PermissionStatus {
        requested.append(permission)
        let result = promptResults[permission] ?? statuses[permission, default: .unknown]
        statuses[permission] = result
        return result
    }

    func setPromptResult(_ permission: PermissionKind, status: PermissionStatus) {
        promptResults[permission] = status
    }

    func requestedPermissions() -> [PermissionKind] {
        requested
    }
}
