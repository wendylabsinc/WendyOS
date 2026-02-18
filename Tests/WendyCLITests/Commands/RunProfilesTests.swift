import AppConfig
import Foundation
import Testing

@testable import wendy

@Suite("Run Profile Resolution Tests")
struct RunProfilesTests {

    @Test("Resolves best matching local profile by OS and arch")
    func resolvesBestLocalProfile() throws {
        let config = AppConfig(
            appId: "com.example.test",
            version: "1.0.0",
            profiles: [
                .init(
                    id: "local-generic",
                    when: .init(target: .local),
                    run: .init(type: .host, command: "echo generic")
                ),
                .init(
                    id: "local-macos-arm64",
                    when: .init(target: .local, os: "macos", arch: "arm64"),
                    run: .init(type: .host, command: "echo mac")
                ),
            ]
        )

        let resolved = try config.resolveProfile(
            context: .init(target: .local, os: "macos", arch: "arm64")
        )

        #expect(resolved.id == "local-macos-arm64")
    }

    @Test("Resolves device profile by hostname regex and traits")
    func resolvesDeviceProfileByHostnameAndTraits() throws {
        let config = AppConfig(
            appId: "com.example.test",
            version: "1.0.0",
            profiles: [
                .init(
                    id: "device-cpu",
                    when: .init(target: .device, traits: ["cpu"]),
                    build: .init(type: .docker),
                    run: .init(type: .container)
                ),
                .init(
                    id: "device-jetson-cuda",
                    when: .init(
                        target: .device,
                        traits: ["cuda"],
                        device: .init(platform: "jetson", hostnameRegex: ".*jetson.*")
                    ),
                    build: .init(type: .docker),
                    run: .init(type: .container)
                ),
            ]
        )

        let resolved = try config.resolveProfile(
            context: .init(
                target: .device,
                traits: ["cuda"],
                devicePlatform: "jetson",
                deviceHostname: "my-jetson.local"
            )
        )

        #expect(resolved.id == "device-jetson-cuda")
    }

    @Test("Requested profile ID bypasses matching")
    func requestedProfileBypassesMatching() throws {
        let config = AppConfig(
            appId: "com.example.test",
            version: "1.0.0",
            profiles: [
                .init(
                    id: "local-a",
                    when: .init(target: .local),
                    run: .init(type: .host, command: "echo a")
                ),
                .init(
                    id: "local-b",
                    when: .init(target: .local, os: "linux"),
                    run: .init(type: .host, command: "echo b")
                ),
            ]
        )

        let resolved = try config.resolveProfile(
            context: .init(target: .local, os: "macos"),
            requestedProfileID: "local-b"
        )

        #expect(resolved.id == "local-b")
    }

    @Test("defaultProfile wins when top candidates are tied")
    func defaultProfileBreaksTopCandidateTie() throws {
        let config = AppConfig(
            appId: "com.example.test",
            version: "1.0.0",
            defaultProfile: "local-b",
            profiles: [
                .init(
                    id: "local-a",
                    when: .init(target: .local),
                    run: .init(type: .host, command: "echo a")
                ),
                .init(
                    id: "local-b",
                    when: .init(target: .local),
                    run: .init(type: .host, command: "echo b")
                ),
            ]
        )

        let resolved = try config.resolveProfile(
            context: .init(target: .local, os: "macos", arch: "arm64")
        )

        #expect(resolved.id == "local-b")
    }

    @Test("defaultProfile does not override stronger match")
    func defaultProfileDoesNotOverrideHigherRankedMatch() throws {
        let config = AppConfig(
            appId: "com.example.test",
            version: "1.0.0",
            defaultProfile: "local-generic",
            profiles: [
                .init(
                    id: "local-generic",
                    when: .init(target: .local),
                    run: .init(type: .host, command: "echo generic")
                ),
                .init(
                    id: "local-macos-arm64",
                    when: .init(target: .local, os: "macos", arch: "arm64"),
                    run: .init(type: .host, command: "echo specific")
                ),
            ]
        )

        let resolved = try config.resolveProfile(
            context: .init(target: .local, os: "macos", arch: "arm64")
        )

        #expect(resolved.id == "local-macos-arm64")
    }

    @Test("Higher priority wins when scores tie")
    func higherPriorityWinsWhenScoresTie() throws {
        let config = AppConfig(
            appId: "com.example.test",
            version: "1.0.0",
            profiles: [
                .init(
                    id: "local-low-priority",
                    when: .init(target: .local),
                    priority: 10,
                    run: .init(type: .host, command: "echo low")
                ),
                .init(
                    id: "local-high-priority",
                    when: .init(target: .local),
                    priority: 50,
                    run: .init(type: .host, command: "echo high")
                ),
            ]
        )

        let resolved = try config.resolveProfile(
            context: .init(target: .local, os: "macos", arch: "arm64")
        )

        #expect(resolved.id == "local-high-priority")
    }

    @Test("Rejects invalid profile hostname regex")
    func rejectsInvalidHostnameRegex() throws {
        let config = AppConfig(
            appId: "com.example.test",
            version: "1.0.0",
            profiles: [
                .init(
                    id: "device-invalid-regex",
                    when: .init(
                        target: .device,
                        device: .init(hostnameRegex: "[")
                    ),
                    build: .init(type: .docker),
                    run: .init(type: .container)
                )
            ]
        )

        #expect(throws: ProfileResolutionError.self) {
            _ = try config.resolveProfile(
                context: .init(target: .device, deviceHostname: "edge.local")
            )
        }
    }

    @Test("Profile decoding requires build or run")
    func profileRequiresBuildOrRun() throws {
        let json = """
            {
              "appId": "com.example.test",
              "version": "1.0.0",
              "profiles": [
                {
                  "id": "invalid",
                  "when": { "target": "local" }
                }
              ]
            }
            """

        #expect(throws: DecodingError.self) {
            _ = try JSONDecoder().decode(AppConfig.self, from: Data(json.utf8))
        }
    }

    @Test("Profile decoding rejects duplicate profile IDs")
    func profileRejectsDuplicateIDs() throws {
        let json = """
            {
              "appId": "com.example.test",
              "version": "1.0.0",
              "profiles": [
                {
                  "id": "local-dev",
                  "when": { "target": "local" },
                  "run": { "type": "host", "command": "echo one" }
                },
                {
                  "id": "local-dev",
                  "when": { "target": "local", "os": "linux" },
                  "run": { "type": "host", "command": "echo two" }
                }
              ]
            }
            """

        #expect(throws: DecodingError.self) {
            _ = try JSONDecoder().decode(AppConfig.self, from: Data(json.utf8))
        }
    }

    @Test("Profile decoding supports build env and lifecycle hooks")
    func profileDecodesBuildEnvAndHooks() throws {
        let json = """
            {
              "appId": "com.example.test",
              "version": "1.0.0",
              "profiles": [
                {
                  "id": "local-dev",
                  "when": { "target": "local" },
                  "build": {
                    "type": "command",
                    "command": "make build",
                    "env": {
                      "DOCKER_BUILDKIT": "1"
                    }
                  },
                  "run": {
                    "type": "host",
                    "command": "python app.py"
                  },
                  "hooks": {
                    "preBuild": [
                      {
                        "name": "prepare",
                        "command": "echo {{target.platform}}"
                      }
                    ],
                    "preStop": [
                      {
                        "args": ["echo", "cleanup"],
                        "continueOnError": true
                      }
                    ]
                  }
                }
              ]
            }
            """

        let config = try JSONDecoder().decode(AppConfig.self, from: Data(json.utf8))
        let profile = try #require(config.profile(withID: "local-dev"))

        #expect(profile.build?.env?["DOCKER_BUILDKIT"] == "1")
        #expect(profile.hooks?.preBuild?.count == 1)
        #expect(profile.hooks?.preBuild?.first?.command == "echo {{target.platform}}")
        #expect(profile.hooks?.preStop?.first?.continueOnError == true)
    }

    @Test("Profile decoding supports all lifecycle hook phases")
    func profileDecodesAllLifecycleHookPhases() throws {
        let json = """
            {
              "appId": "com.example.test",
              "version": "1.0.0",
              "profiles": [
                {
                  "id": "local-dev",
                  "when": { "target": "local" },
                  "build": {
                    "type": "command",
                    "command": "echo build"
                  },
                  "run": {
                    "type": "host",
                    "command": "echo run"
                  },
                  "hooks": {
                    "preBuild": [
                      { "name": "pre-build", "command": "echo pre-build" }
                    ],
                    "postBuild": [
                      { "name": "post-build", "command": "echo post-build" }
                    ],
                    "preRun": [
                      { "name": "pre-run", "args": ["echo", "pre-run"] }
                    ],
                    "postRun": [
                      { "name": "post-run", "command": "echo post-run" }
                    ],
                    "preStop": [
                      { "name": "pre-stop", "command": "echo pre-stop", "continueOnError": true }
                    ]
                  }
                }
              ]
            }
            """

        let config = try JSONDecoder().decode(AppConfig.self, from: Data(json.utf8))
        let profile = try #require(config.profile(withID: "local-dev"))
        let hooks = try #require(profile.hooks)

        #expect(hooks.preBuild?.first?.name == "pre-build")
        #expect(hooks.postBuild?.first?.name == "post-build")
        #expect(hooks.preRun?.first?.args == ["echo", "pre-run"])
        #expect(hooks.postRun?.first?.name == "post-run")
        #expect(hooks.preStop?.first?.continueOnError == true)
    }

    @Test("RunCommand rejects conflicting --local and --device")
    func runCommandRejectsLocalAndDeviceConflict() throws {
        #expect(throws: (any Error).self) {
            try RunCommand.parse(["--local", "--device", "edge.local"]).validate()
        }
    }
}
