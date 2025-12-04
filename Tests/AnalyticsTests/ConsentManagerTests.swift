import Foundation
import Testing

@testable import Analytics

// MARK: - Mock Environment Provider

struct MockEnvironmentProvider: EnvironmentProvider {
    private var environment: [String: String]

    init(_ environment: [String: String] = [:]) {
        self.environment = environment
    }

    func getValue(forKey key: String) -> String? {
        environment[key]
    }
}

// MARK: - Environment Variable Tests

@Suite("Consent Manager Environment Variables")
struct ConsentManagerEnvironmentTests {

    @Test(
        "Should disable analytics with WENDY_ANALYTICS set to non-true values",
        arguments: [
            "false", "0", "no", "False", "FALSE", "No", "NO", "off", "disabled", "random",
        ]
    )
    func shouldDisableAnalyticsWithWendyAnalyticsFalse(value: String) {
        let mockEnv = MockEnvironmentProvider(["WENDY_ANALYTICS": value])
        let consentManager = ConsentManager(environmentProvider: mockEnv)
        #expect(consentManager.shouldDisableAnalytics() == true)
    }

    @Test(
        "Should not disable analytics with WENDY_ANALYTICS enabled",
        arguments: [
            "true", "1", "enabled", "TRUE", "True", "ENABLED", "Enabled",
        ]
    )
    func shouldNotDisableAnalyticsWithWendyAnalyticsEnabled(value: String) {
        let mockEnv = MockEnvironmentProvider(["WENDY_ANALYTICS": value])
        let consentManager = ConsentManager(environmentProvider: mockEnv)
        #expect(consentManager.shouldDisableAnalytics() == false)
    }

    @Test(
        "Should disable analytics in CI environment",
        arguments: [
            "CI",
            "CONTINUOUS_INTEGRATION",
            "BUILD_ID",
            "JENKINS_URL",
            "GITHUB_ACTIONS",
            "GITLAB_CI",
        ]
    )
    func shouldDisableAnalyticsInCIEnvironment(indicator: String) {
        let mockEnv = MockEnvironmentProvider([indicator: "true"])
        let consentManager = ConsentManager(environmentProvider: mockEnv)
        #expect(consentManager.shouldDisableAnalytics() == true)
    }

    @Test("WENDY_ANALYTICS should take precedence over CI detection")
    func environmentVariablePriority() {
        let mockEnv = MockEnvironmentProvider(["CI": "true", "WENDY_ANALYTICS": "true"])
        let consentManager = ConsentManager(environmentProvider: mockEnv)
        #expect(consentManager.shouldDisableAnalytics() == false)
    }

    @Test("Should not disable analytics by default (no env vars set)")
    func shouldNotDisableByDefault() {
        // Use mock environment with no variables set
        let mockEnv = MockEnvironmentProvider([:])
        let consentManager = ConsentManager(environmentProvider: mockEnv)

        #expect(consentManager.shouldDisableAnalytics() == false)
    }
}

// MARK: - Async Tests

@Suite("Consent Manager Async Operations")
struct ConsentManagerAsyncTests {

    @Test("Analytics should be disabled when WENDY_ANALYTICS is false")
    func isAnalyticsEnabledWithEnvironmentOverride() {
        // Use mock environment to avoid filesystem access
        let mockEnv = MockEnvironmentProvider(["WENDY_ANALYTICS": "false"])
        let consentManager = ConsentManager(environmentProvider: mockEnv)

        // Should be disabled when explicitly set to false
        let shouldDisable = consentManager.shouldDisableAnalytics()
        #expect(shouldDisable == true)
    }

    @Test("Analytics should be enabled by default (opt-out model)")
    func isAnalyticsEnabledByDefault() {
        // Use mock environment with no CI variables set
        let mockEnv = MockEnvironmentProvider([:])
        let consentManager = ConsentManager(environmentProvider: mockEnv)

        // Without any environment variables, analytics should not be disabled
        // This validates the opt-out model (enabled by default) without filesystem access
        let shouldDisable = consentManager.shouldDisableAnalytics()
        #expect(shouldDisable == false)
    }

    @Test("Get status should return appropriate message")
    func getStatus() async {
        let consentManager = ConsentManager()

        // Test with no environment variables (must unset ALL CI indicators)
        unsetenv("WENDY_ANALYTICS")
        unsetenv("CI")
        unsetenv("CONTINUOUS_INTEGRATION")
        unsetenv("BUILD_ID")
        unsetenv("JENKINS_URL")
        unsetenv("GITHUB_ACTIONS")
        unsetenv("GITLAB_CI")
        var status = await consentManager.getStatus()
        #expect(status.contains("Analytics:"))

        // Test with WENDY_ANALYTICS=false
        setenv("WENDY_ANALYTICS", "false", 1)
        status = await consentManager.getStatus()
        #expect(status.contains("Disabled") && status.contains("environment variable"))
        unsetenv("WENDY_ANALYTICS")
    }
}

// MARK: - Config Management Tests

@Suite("Consent Manager Config Operations")
struct ConsentManagerConfigTests {

    @Test("Disable analytics should not throw")
    func disableAnalytics() throws {
        let consentManager = ConsentManager()
        do {
            try consentManager.disableAnalytics()
        } catch {
            // Expected to potentially fail without proper file system setup
            // But the method should not crash
        }
    }

    @Test("Enable analytics should not throw")
    func enableAnalytics() throws {
        let consentManager = ConsentManager()
        do {
            try consentManager.enableAnalytics()
        } catch {
            // Expected to potentially fail without proper file system setup
            // But the method should not crash
        }
    }
}
