import Foundation
import Testing
import WendyE2ETesting

/// Physical proof is opt-in and requires a currently reserved disposable board.
/// Admission-only privileged operations remain in the isolated suite.
extension AuthModesLiveAcceptance {
    @Suite(.serialized, .enabled(if: authModesLiveEnabled && authModesLocalTestsSupported))
    struct `'Auth Modes device acceptance'` {
        @Test(.enabled(if: authModesDiscoverySupported))
        func `A20 target-only TXT reports Open then Closed`() async throws {
            try await AuthModesLiveFixture.load().run(observeDiscovery: true) { fixture, cli, _ in
                try await fixture.expect(cli, contains: "Access: Closed", "device access")
            }
        }

        /** A09. A real CLI follows the owned app's logs before revocation.
         Removal must end that process and refuse fresh operator connections. */
        @Test
        func `A09 removing an operator ends its live stream and new access`() async throws {
            try await AuthModesLiveFixture.load().run { fixture, cli, _ in
                try await HTTPAppFixture.withComposeApp(
                    cli: cli,
                    configDirectory: fixture.owner,
                    recordApp: { try await fixture.recordApp(cli, name: $0) }
                ) { app in
                    let script = """
                        import json, os, subprocess, time
                        from pathlib import Path
                        root=Path(\(String(reflecting: fixture.recoveryDirectory)))
                        device=\(String(reflecting: fixture.device)); app=\(String(reflecting: app.app))
                        def env(role): return dict(os.environ,WENDY_CONFIG_DIR=str(root/role),WENDY_RUN_NO_CLOUD_FALLBACK='1')
                        def run(role,args):
                            return subprocess.run(['wendy','--json=false','--device',device,*args],env=env(role),text=True,stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=30)
                        fp=json.loads(run('operator',['--json','auth','local-key','show']).stdout)['fingerprint']
                        log=root/'removed-operator-stream.log'
                        with log.open('w') as output:
                            stream=subprocess.Popen(['wendy','--json=false','--device',device,'device','logs','--app',app,'--tail','20'],env=env('operator'),text=True,stdout=output,stderr=subprocess.STDOUT)
                            try:
                                for _ in range(100):
                                    if app in log.read_text(): break
                                    assert stream.poll() is None, 'stream ended before revocation'
                                    time.sleep(0.1)
                                else: raise RuntimeError('stream never produced owned-app evidence')
                                removed=run('owner',['device','keys','remove',fp,'--yes'])
                                assert removed.returncode==0 and 'Removed' in removed.stdout
                                try: stream.wait(timeout=15)
                                except subprocess.TimeoutExpired: raise RuntimeError('removed key stream remained active')
                                assert 'context deadline exceeded' not in log.read_text()
                                for _ in range(30):
                                    owner=run('owner',['device','access'])
                                    if owner.returncode==0 and 'Your role: owner' in owner.stdout: break
                                    time.sleep(1)
                                else: raise RuntimeError('independent owner never reconnected')
                                denied=run('operator',['device','access'])
                                assert denied.returncode and fp in denied.stderr and 'does not trust' in denied.stderr
                                print('A09: observed owned stream before removal, stream ended within 15 seconds, fresh key refused, independent owner recovered')
                            finally:
                                if stream.poll() is None:
                                    stream.terminate()
                                    try: stream.wait(timeout=5)
                                    except subprocess.TimeoutExpired: stream.kill(); stream.wait()
                        """
                    try await cli.sh("python3 -c \(AuthModesFixture.quote(script))")
                }
            }
        }

        /** A12/A13. Operator manages an ordinary app. Independent HTTP observations
         prove effects and preserve the running instance after each viewer refusal. */
        @Test
        func `A12 A13 viewer reads and operator manages a real HTTP app`() async throws {
            try await AuthModesLiveFixture.load().run { fixture, cli, _ in
                try await HTTPAppFixture.withComposeApp(
                    cli: cli,
                    configDirectory: fixture.config("operator"),
                    recordApp: { try await fixture.recordApp(cli, name: $0) }
                ) { app in
                    let first = try await app.response(cli)
                    try await cli.sh("\(fixture.command("viewer", json: true)) device apps list") {
                        result in
                        try #require(result.status.isSuccess)
                        let apps = try JSONDecoder().decode(
                            [HTTPAppFixture.App].self,
                            from: Data(result.stdout.utf8)
                        )
                        #expect(
                            apps.contains { $0.name == app.app && $0.runningState == "RUNNING" }
                        )
                    }
                    try await cli.sh(
                        "\(fixture.command("viewer", json: true)) device info --read-only"
                    )
                    try await cli.sh(
                        "\(fixture.command("viewer")) device logs --app \(app.app) --no-follow"
                    ) { result in
                        try #require(result.status.isSuccess)
                        #expect(result.stdout.contains(app.app))
                    }
                    let beforeKeys = try await cli.sh(
                        "\(fixture.command(json: true)) device keys list"
                    ) { $0.stdout }
                    for action in ["stop", "start", "remove"] {
                        let flags =
                            action == "remove" ? "--force" : action == "start" ? "--detach" : ""
                        try await fixture.refuse(
                            cli,
                            role: "viewer",
                            diagnostic: "operator",
                            "device apps \(action) \(app.app) \(flags)"
                        )
                        let after = try await app.response(cli)
                        #expect(after.instance == first.instance)
                        try await app.requireState(cli, "RUNNING", expectedVersion: nil)
                        let afterKeys = try await cli.sh(
                            "\(fixture.command(json: true)) device keys list"
                        ) { $0.stdout }
                        #expect(beforeKeys == afterKeys)
                    }
                    // A viewer receives the same valid ordinary deployment input.
                    // Owner/operator reads independently verify the running app survives.
                    try await HTTPAppFixture.withProject(
                        cli: cli,
                        imageMayBeImported: false,
                        mode: .prebuiltCompose,
                        configDirectory: fixture.config("viewer"),
                        recordApp: { try await fixture.recordApp(cli, name: $0) }
                    ) { refusedApp in
                        let inventory = try await app.inventory(cli)
                        try await cli.sh(
                            "\(refusedApp.command) run --prefix \(AuthModesFixture.quote(refusedApp.app)) --skip-cloud-registration --detach --no-restart"
                        ) { result in
                            #expect(result.status.isFailure)
                            #expect(result.stderr.contains("operator"))
                        }
                        let after = try await app.inventory(cli)
                        #expect(
                            inventory.sorted { $0.name < $1.name }
                                == after.sorted { $0.name < $1.name }
                        )
                        #expect(!after.contains { $0.name == refusedApp.app })
                        let response = try await app.response(cli)
                        #expect(response.instance == first.instance)
                    }
                    try await app.stop(cli, expectedVersion: nil)
                    try await app.start(cli)
                    let restarted = try await app.response(cli)
                    #expect(restarted.instance != first.instance)
                    try await app.requireState(cli, "RUNNING", expectedVersion: nil)
                    // withComposeApp removes the exact app and verifies inventory.
                }
            }
        }

        /** A18/A19. Keyless app operations have real effects. Switching off preserves
         the serving app and all four keys, then refuses the stranger again. */
        @Test
        func `A18 A19 opens and closes the ordinary app set without losing keys`() async throws {
            try await AuthModesLiveFixture.load().run { fixture, cli, _ in
                let keys = try await cli.sh("\(fixture.command(json: true)) device keys list") {
                    $0.stdout
                }
                try await fixture.expect(
                    cli,
                    contains: "Keyless apps on",
                    "device access keyless on"
                )
                try await fixture.wait(cli, role: "stranger", shape: "Closed", keyless: "on")
                try await fixture.waitForApps(cli, role: "stranger", allowed: true)
                try await fixture.expect(
                    cli,
                    role: "stranger",
                    contains: "Your role: none",
                    "device access"
                )
                try await HTTPAppFixture.withComposeApp(
                    cli: cli,
                    configDirectory: fixture.config("stranger"),
                    allowKeylessInventoryWarning: true,
                    recordApp: { try await fixture.recordApp(cli, name: $0) }
                ) { app in
                    let first = try await app.response(cli)
                    _ = try await app.requireReplay(cli, containing: first)
                    try await app.stop(cli, expectedVersion: nil)
                    try await app.start(cli)
                    let restarted = try await app.response(cli)
                    #expect(restarted.instance != first.instance)
                    try await fixture.refuse(
                        cli,
                        role: "stranger",
                        diagnostic: "owner",
                        "device keys list"
                    )
                    try await fixture.expect(
                        cli,
                        contains: "Keyless apps off",
                        "device access keyless off"
                    )
                    try await fixture.wait(cli, shape: "Closed", keyless: "off")
                    try await fixture.waitForApps(cli, role: "stranger", allowed: false)
                    try await fixture.refuse(
                        cli,
                        role: "stranger",
                        diagnostic: "does not trust",
                        "device apps list"
                    )
                    let after = try await app.response(cli)
                    #expect(after.instance == restarted.instance)
                    let afterKeys = try await cli.sh(
                        "\(fixture.command(json: true)) device keys list"
                    ) { $0.stdout }
                    #expect(keys == afterKeys)
                    for role in ["owner", "operator", "viewer"] {
                        try await fixture.expect(
                            cli,
                            role: role,
                            contains: "Your role: " + role,
                            "device access"
                        )
                    }
                    // Restore keyless just for this app's exact-resource cleanup.
                    try await fixture.expect(
                        cli,
                        contains: "Keyless apps on",
                        "device access keyless on"
                    )
                    try await fixture.wait(cli, role: "stranger", shape: "Closed", keyless: "on")
                    try await fixture.waitForApps(cli, role: "stranger", allowed: true)
                }
                try await fixture.expect(
                    cli,
                    contains: "Keyless apps off",
                    "device access keyless off"
                )
                try await fixture.wait(cli, shape: "Closed", keyless: "off")
                try await fixture.waitForApps(cli, role: "stranger", allowed: false)
            }
        }

        /** A17. Only a separately selected reboot run requests a normal reboot.
         The owner reads only the Linux boot ID through the normal CLI shell. */
        @Test(
            .enabled(
                if: ProcessInfo.processInfo.environment["WENDY_E2E_AUTH_MODES_REBOOT"] == "true"
            )
        )
        func `A17 operator reboots and all roles survive a changed Linux boot ID`() async throws {
            try await AuthModesLiveFixture.load().run { fixture, cli, _ in
                let bootIDCommand =
                    "\(fixture.command()) device shell -- cat /proc/sys/kernel/random/boot_id"
                let before = try await cli.sh(bootIDCommand) { result in
                    try #require(result.status.isSuccess)
                    let id = result.normalizedStdout.trimmingCharacters(in: .whitespacesAndNewlines)
                    try #require(
                        UUID(uuidString: id) != nil,
                        "Owner shell must return a valid Linux boot ID"
                    )
                    return id
                }
                try await fixture.refuse(
                    cli,
                    role: "viewer",
                    diagnostic: "operator",
                    "device reboot --yes --no-wait"
                )
                try await cli.sh("\(fixture.command("operator")) device reboot --yes --no-wait")
                var changed = false
                for _ in 0..<60 {
                    let after = try await cli.sh(bootIDCommand) { $0 }
                    let observed = after.normalizedStdout.trimmingCharacters(
                        in: .whitespacesAndNewlines
                    )
                    if after.status.isSuccess && UUID(uuidString: observed) != nil
                        && observed != before
                    {
                        changed = true
                        break
                    }
                    try await cli.sh("sleep 2")
                }
                try #require(changed, "A successful request is not proof of reboot")
                try await fixture.wait(cli, shape: "Closed", keyless: "off")
                for role in ["owner", "operator", "viewer"] {
                    try await fixture.expect(
                        cli,
                        role: role,
                        contains: "Your role: " + role,
                        "device access"
                    )
                }
            }
        }
    }

}
