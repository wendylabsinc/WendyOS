import AsyncHTTPClient
import Foundation
import NIOCore
import Testing

@testable import wendy

@Suite("FetchReleases")
struct FetchReleasesTests {

    /// Mock HTTPExecutor that captures the request for verification
    /// Note: Throws an error since we only test that headers are set correctly (the fix for WDY-554)
    final class MockHTTPExecutor: HTTPExecutor {
        var capturedRequest: HTTPClientRequest?

        func execute(
            _ request: HTTPClientRequest,
            deadline: NIODeadline
        ) async throws -> HTTPClientResponse {
            capturedRequest = request

            // Throw an error immediately - we only care about capturing the request headers
            // The test verifies the headers are set correctly, which fixes the 403 errors
            struct MockError: Error {}
            throw MockError()
        }
    }

    @Test("GitHub API request includes all required headers")
    func testGitHubAPIHeaders() async throws {
        // Given: A mock HTTP client
        let mock = MockHTTPExecutor()

        // When: fetchReleases is called (it will fail, but that's OK)
        _ = try? await fetchReleases(httpClient: mock)

        // Then: All required headers must be present to avoid 403 errors
        // See: https://docs.github.com/en/rest/using-the-rest-api/getting-started-with-the-rest-api
        let request = mock.capturedRequest
        #expect(request != nil, "Request should have been captured")

        // User-Agent is REQUIRED by GitHub API (will get 403 without it)
        #expect(
            request?.headers["User-Agent"].first == "wendy-agent",
            "User-Agent header is required by GitHub API and must be set to 'wendy-agent'"
        )

        // Accept header specifies API response format
        #expect(
            request?.headers["Accept"].first == "application/vnd.github+json",
            "Accept header should be 'application/vnd.github+json'"
        )

        // X-GitHub-Api-Version specifies the API version to use
        #expect(
            request?.headers["X-GitHub-Api-Version"].first == "2022-11-28",
            "X-GitHub-Api-Version header should be '2022-11-28' (latest version)"
        )
    }
}
