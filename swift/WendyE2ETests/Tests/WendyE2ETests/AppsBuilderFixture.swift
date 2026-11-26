import Foundation
import Testing
import WendyE2ETesting

enum AppsBuilderFixture {
    /// Bake the in-tree HTTP server into a scratch Dockerfile context. This
    /// uses the local standard-library toolchain, with no base-image download.
    static func writeSmallContext(_ cli: WendyE2ESession, directory: String) async throws {
        let source = URL(fileURLWithPath: AppsDeploymentFixture.observerPath)
            .deletingLastPathComponent().deletingLastPathComponent()
            .appendingPathComponent(".github/ci-tests/apps-deployments-small").path
        let quote = AppsDeploymentFixture.quote
        try await cli.sh(
            "cp \(quote(source + "/main.go")) \(quote(source + "/Dockerfile")) \(quote(directory))/"
        )
        try await cli.sh(
            "cd \(quote(directory)) && printf '1.0.0\\n' > version.txt && env CGO_ENABLED=0 GOOS=linux GOARCH=arm64 GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local go build -trimpath -buildvcs=false -ldflags '-s -w -X main.imageVersion=1.0.0' -o server main.go"
        )
    }

    static func withBuilder(
        _ cli: WendyE2ESession,
        _ body: @Sendable () async throws -> Void
    ) async throws {
        let plugins = try AppsDeploymentFixture.setting("WENDY_E2E_APPS_DOCKER_PLUGIN_DIR")
        let data = try JSONSerialization.data(withJSONObject: ["cliPluginsExtraDirs": [plugins]])
        let json = String(decoding: data, as: UTF8.self)
        try await cli.sh(
            "mkdir -p \"$DOCKER_CONFIG/cli-plugins\" && printf '%s' \(AppsDeploymentFixture.quote(json)) > \"$DOCKER_CONFIG/config.json\""
        )
        // The CLI's credential-free build config preserves this standard
        // plugin directory, not cliPluginsExtraDirs from config.json.
        let plugin = AppsDeploymentFixture.quote(plugins + "/docker-buildx")
        try await cli.sh(
            "if test -L \"$DOCKER_CONFIG/cli-plugins/docker-buildx\"; then test \"$(readlink \"$DOCKER_CONFIG/cli-plugins/docker-buildx\")\" = \(plugin); else ln -s \(plugin) \"$DOCKER_CONFIG/cli-plugins/docker-buildx\"; fi"
        )
        try await cli.sh("docker buildx version && docker info --format '{{.ServerVersion}}'")
        do {
            try await body()
        } catch {
            do { try await removeBuilder(cli) } catch {
                Issue.record("Owned Buildx builder cleanup failed; retain its command evidence")
            }
            throw error
        }
        try await removeBuilder(cli)
    }

    private static func removeBuilder(_ cli: WendyE2ESession) async throws {
        // The CLI appends -oci to this per-test generated name. Never remove
        // another builder or prune the shared Docker engine.
        try await cli.sh(
            "if docker buildx inspect \"$WENDY_BUILDX_BUILDER-oci\" >/dev/null 2>&1; then docker buildx rm \"$WENDY_BUILDX_BUILDER-oci\"; fi"
        )
    }
}
