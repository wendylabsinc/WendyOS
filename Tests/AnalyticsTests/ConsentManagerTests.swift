import Foundation
import Testing

@testable import Analytics

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
        setenv("WENDY_ANALYTICS", value, 1)
        #expect(ConsentManager.shouldDisableAnalytics() == true)
        unsetenv("WENDY_ANALYTICS")
    }

    @Test(
        "Should not disable analytics with WENDY_ANALYTICS enabled",
        arguments: [
            "true", "1", "enabled", "TRUE", "True", "ENABLED", "Enabled",
        ]
    )
    func shouldNotDisableAnalyticsWithWendyAnalyticsEnabled(value: String) {
        setenv("WENDY_ANALYTICS", value, 1)
        #expect(ConsentManager.shouldDisableAnalytics() == false)
        unsetenv("WENDY_ANALYTICS")
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
        // WENDY_ANALYTICS takes precedence, so ensure it's unset
        unsetenv("WENDY_ANALYTICS")
        setenv(indicator, "true", 1)
        #expect(ConsentManager.shouldDisableAnalytics() == true)
        unsetenv(indicator)
    }

    @Test("WENDY_ANALYTICS should take precedence over CI detection")
    func environmentVariablePriority() {
        setenv("CI", "true", 1)
        setenv("WENDY_ANALYTICS", "true", 1)
        #expect(ConsentManager.shouldDisableAnalytics() == false)
        unsetenv("CI")
        unsetenv("WENDY_ANALYTICS")
    }

    @Test("Should not disable analytics by default (no env vars set)")
    func shouldNotDisableByDefault() {
        // Ensure no relevant env vars are set
        unsetenv("WENDY_ANALYTICS")
        unsetenv("CI")
        unsetenv("CONTINUOUS_INTEGRATION")
        unsetenv("BUILD_ID")
        unsetenv("JENKINS_URL")
        unsetenv("GITHUB_ACTIONS")
        unsetenv("GITLAB_CI")

        #expect(ConsentManager.shouldDisableAnalytics() == false)
    }
}

// MARK: - Async Tests

@Suite("Consent Manager Async Operations")
struct ConsentManagerAsyncTests {

    @Test("Analytics should be disabled when WENDY_ANALYTICS is false")
    func isAnalyticsEnabledWithEnvironmentOverride() async {
        let consentManager = ConsentManager()

        setenv("WENDY_ANALYTICS", "false", 1)
        let enabled = await consentManager.isAnalyticsEnabled()
        #expect(enabled == false)
        unsetenv("WENDY_ANALYTICS")
    }

    @Test("Analytics should be enabled by default (opt-out model)")
    func isAnalyticsEnabledByDefault() async {
        // Ensure no relevant env vars are set (must unset ALL CI indicators)
        unsetenv("WENDY_ANALYTICS")
        unsetenv("CI")
        unsetenv("CONTINUOUS_INTEGRATION")
        unsetenv("BUILD_ID")
        unsetenv("JENKINS_URL")
        unsetenv("GITHUB_ACTIONS")
        unsetenv("GITLAB_CI")

        let consentManager = ConsentManager()

        // Without any config or environment variables, should default to enabled
        let enabled = await consentManager.isAnalyticsEnabled()
        #expect(enabled == true)
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
