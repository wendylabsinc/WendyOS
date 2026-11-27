import Foundation
import Testing
import WendyE2ETesting

/// Tiny, private-to-the-specs cache trees; no downloads, builders or global cache state.
enum CacheCommandFixture {
    static let knownDefectsEnabled =
        ProcessInfo.processInfo.environment["WENDY_E2E_KNOWN_CACHE_DEFECTS"] == "1"

    static func restoringPermissions(for directory: String) -> CLIAndAgentScenario {
        CLIAndAgentScenario(after: { cli, _ in
            try await cli.sh(
                """
                \(try environment(cli))
                locked="$cache/\(directory)"
                if [ -d "$locked" ] && [ ! -L "$locked" ]; then
                  chmod 700 "$locked"
                fi
                """
            )
        })
    }

    static func environment(_ cli: WendyE2ESession) throws -> String {
        try #require(
            WendyE2EEnvironment.isolation == .perTest,
            "Cache fixtures require per-test isolation"
        )
        return """
            set -eu
            export XDG_CACHE_HOME="$HOME/.cache"
            cache="\(cli.wendyCacheDirectory)"
            """
    }

    static func populate(_ cli: WendyE2ESession) async throws {
        try await cli.sh(
            """
            \(try environment(cli))
            mkdir -p "$cache/os-images/jetson.flashpack/boot" \
              "$cache/buildx/com.example.demo-linux_arm64/blobs/sha256" \
              "$cache/ocilayout/com.example.demo-linux_arm64/blobs/sha256" \
              "$cache/ocilayout/blobs" "$cache/misc/nested"
            printf '12345678901' > "$cache/os-images/pi.img"
            printf '1234567' > "$cache/os-images/jetson.flashpack/boot/kernel"
            printf '12345' > "$cache/os-images/jetson.flashpack/rootfs"
            printf '1234567890123' > "$cache/buildx/com.example.demo-linux_arm64/blobs/sha256/bca2b41a2b25e137c83fee346af7bd1e0f52bd560583ca07a1b42f9944c5c50b"
            printf '12345678901234567' > "$cache/ocilayout/com.example.demo-linux_arm64/blobs/sha256/97f40b8ae3e4d3118bb4afb623d3e768f04d9bc2f913bd64296ebcd53386c1ac"
            printf '123' > "$cache/ocilayout/blobs/shared"
            printf '1234567890123456789' > "$cache/misc/nested/payload"
            """
        )
    }
}
