import Foundation
import Testing
import WendyE2ETesting

/// Isolated admission/protocol observations for the approved Auth Modes cases.
/// See AuthModesCoverage.json for sub-assertions and hardware limits.
@Suite(
    .serialized,
    .enabled(if: authModesLocalTestsSupported, "Requires a local POSIX CLI and Go fixture runner")
)
struct `'Auth Modes isolated acceptance'` {
    @Test
    func `A01 accepts whole public output by file and stdin`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/cli/commands",
            ["TestAuthModesA01PublicOutput"]
        )
    }

    @Test(.enabled(if: authModesUnprivilegedFileTestsSupported, "Root can read mode-000 files"))
    func `A01 preserves an unreadable local identity`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/cli/commands",
            ["TestAuthModesA01UnreadableIdentity"]
        )
    }

    @Test
    func `A02 reports Open and refuses key management without creating access state`() async throws
    {
        try await AuthModesFixture.isolated("./internal/cli/commands", ["TestAuthModesA02Open"])
    }

    @Test
    func `A03 makes the closing computer its sole owner across fixture restart`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/cli/commands",
            ["TestAuthModesA03FirstOwner"]
        )
    }

    @Test
    func `A04 requires nonterminal consent from each valid starting state`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/cli/commands",
            ["TestAuthModesA04Consent", "TestAuthModesA04TerminalDecline"]
        )
    }

    @Test
    func `A05 refuses an untrusted key even with a stored Cloud certificate`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/cli/commands",
            ["TestAuthModesA05Untrusted"]
        )
    }

    @Test
    func `A06 gives each trusted key its role and refuses nonowner key management`() async throws {
        try await AuthModesFixture.isolated("./internal/cli/commands", ["TestAuthModesA06Roles"])
    }

    @Test
    func `A07 changes a role by removing and adding the same public key`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/cli/commands",
            ["TestAuthModesA07ChangeRole"]
        )
    }

    @Test
    func `A08 retains a usable owner after refusing removal of the last owner`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/cli/commands",
            ["TestAuthModesA08LastOwner"]
        )
    }

    @Test
    func `A09 ends an actual stream when the isolated fixture honors restart`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/agent/services",
            ["TestAuthModesA09RemovedStream"]
        )
    }

    @Test
    func `A10 refuses unsupported algorithms and malformed input without adding keys`() async throws
    {
        try await AuthModesFixture.isolated(
            "./internal/cli/commands",
            [
                "TestAuthModesA10Inputs", "TestDeviceKeysListEscapesLabels",
                "TestLocalKeyShowOutputIsAddableAsIs",
                "TestReadPublicKeyArgRefusesPrivateKeysAndJunk",
            ]
        )
    }

    @Test
    func `A11 admits every frozen method for exactly its assigned callers`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/agent/interceptor",
            ["TestAuthModesA11FrozenMatrix"]
        )
    }

    @Test
    func `A14 reserves privileged method admission and root hooks for owners`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/agent/interceptor",
            ["TestClosedAuthorizer"]
        )
    }

    @Test
    func `A15 checks deployment and exec privileges beyond method admission`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/agent/containerd",
            [
                "TestDeployEntitlementsByCallerRole",
                "TestExecAndAttachNeedTheRoleTheAppsEntitlementsNeed",
            ]
        )
    }

    @Test
    func `A16 limits admin socket admission to operator authority`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/agent/interceptor",
            ["TestAuthModesA11FrozenMatrix"]
        )
    }

    @Test
    func `A17 refuses nonowner forced recovery before the isolated scheduler`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/agent/services",
            [
                "TestForcedRecoveryNeedsTheOwnerOnAClosedDevice",
                "TestRebootIntoForcedRecoveryReachesTheHost",
                "TestPlainRebootRequestIsANormalReboot",
            ]
        )
    }

    @Test
    func `A18 and A19 restore roles and keyless off across fixture restarts`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/cli/commands",
            ["TestAuthModesA18A19Keyless", "TestRegistryOverLocalKey"]
        )
    }

    @Test
    func `A20 treats discovery shape as a display hint`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/cli/commands",
            [
                "TestDiscoverShowsClosedShape", "TestDiscoverRowOfClosedDeviceReachedWithLocalKey",
                "TestLadderIgnoresLocalPinUnderAliases",
            ]
        )
    }

    @Test
    func `A21 retains identity pins and refuses a replacement device key`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/cli/commands",
            ["TestLadderRefusesChangedLocalDeviceKey", "TestLadderIgnoresLocalPinUnderAliases"]
        )
    }

    @Test
    func `A22 preserves existing defensive no plaintext guarantees`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/cli/commands",
            [
                "TestLadderNeverReachesLocallyPinnedDeviceInTheClear",
                "TestLadderNeverPlaintextAfterVerifiedLocalHandshake",
                "TestLadderNeverReadsCloseAfterVerifiedLocalHandshakeAsKeyRefused",
                "TestLadderRefusesCloudPinnedHostAnsweringAsLocalDevice",
                "TestLadderRefusesCloudPinnedHostRefusingLocalKey",
            ]
        )
    }

    @Test
    func `A23 reopens and creates a new sole owner without reviving keys`() async throws {
        try await AuthModesFixture.isolated("./internal/cli/commands", ["TestAuthModesA23Reopen"])
    }

    @Test
    func `A24 checks Closed ownership before public enrollment submits to Cloud`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/cli/commands",
            ["TestAuthModesA24EnrollNormalEntry"]
        )
    }

    @Test
    func `A25 refuses access mutations while the isolated service is Enrolled`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/agent/services",
            ["TestAccessShapeTransitions", "TestOpenAndEnrolledUnchanged"]
        )
    }

    @Test
    func `A26 preserves dormant Closed state through defensive cleanup`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/agent/services",
            ["TestUnenrollKeepsClosedAccess"]
        )
    }

    @Test
    func `A28 verifies seed writing and protective first boot refusal paths`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/cli/commands",
            [
                "TestAuthModesA28PublicCLIRefusals",
                "TestAuthModesA28DesktopRefusalBeforeReadiness", "TestAuthModesA28FailedFirstBoot",
                "TestAuthModesA28SeedWriteFailure",
                "TestInstallCloseSeedsThisComputerAsOwner",
                "TestInstallCloseRefusedWhereNoConfigPartitionIsWritten",
                "TestInstallCloseNeverLeavesTheDeviceOpenSilently",
                "TestInstallCloseSurfacesAPinMismatchAtOnce", "TestInstallCloseRefusesPreEnroll",
            ]
        )
    }

    @Test
    func `A29 fails closed for unusable access files`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/agent/trustedkeys",
            ["TestOpenUnusableFileFailsClosed"]
        )
    }

    @Test
    func `A30 never replaces existing owners or reapplies a consumed seed`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/agent/configpartition",
            [
                "TestApplyTrustedKeysSeed_NeverReplacesExistingKeys",
                "TestApplyTrustedKeysSeed_LeavesAnUnusableSeedInPlace",
                "TestApplyTrustedKeysSeed_NotReappliedAfterReopen",
                "TestApplyTrustedKeysSeed_FailsWhenTheSeedCannotBeInstalled",
            ]
        )
    }

    @Test
    func `A31 refuses registered unmapped unary and streaming handlers for every caller`()
        async throws
    {
        try await AuthModesFixture.isolated(
            "./internal/agent/interceptor",
            ["TestAuthModesA31RegisteredUnmappedCalls"]
        )
    }

    @Test
    func `A24 MCP refuses nonowners before any Cloud request`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/cli/mcp",
            ["TestAuthModesA24MCPBeforeCloud"]
        )
    }

    @Test
    func `A27 sudo keeps the invoking users key and refuses a root replacement`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/cli/localkey",
            ["TestPathUnderSudoIsTheInvokingUsersKey", "TestLoadOrCreateRefusesToCreateUnderSudo"]
        )
    }

    @Test
    func `A29 enrollment takes precedence over an unusable dormant local store`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/agent/services",
            ["TestAuthModesA29EnrolledPrecedence"]
        )
    }

    @Test
    func `A20 accepts TXT only when the selected host and shape agree`() async throws {
        try await CLIAndAgentScenario().run(authenticated: false) { cli, _ in
            try await cli.sh(
                "cd \(AuthModesFixture.quote(AuthModesFixture.root + "/.github/ci-tests/auth-modes")) && python3 -m unittest -v test_observe_txt"
            )
        }
    }

    @Test
    func `A18 A19 retries only transient read-only app failures`() async throws {
        try await CLIAndAgentScenario().run(authenticated: false) { cli, _ in
            try await cli.sh(
                "cd \(AuthModesFixture.quote(AuthModesFixture.root + "/.github/ci-tests/auth-modes")) && python3 -m unittest -v test_readiness"
            )
        }
    }

    /** WDY-4393. A status read on the outgoing listener must not leave the
     owner unable to use the settled Open device. The assertion stays positive. */
    @Test
    func `A23 reconnects Open after polling during restart WDY-4393`() async throws {
        try await AuthModesFixture.isolated(
            "./internal/cli/commands",
            ["TestAuthModesA23ReopenDuringRestart"],
            knownReopen: true
        )
    }
}
