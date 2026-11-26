import Foundation
import Testing
import WendyE2ETesting

@Suite(.serialized)
struct AuthModesLiveAcceptance {}

let authModesDiscoverySupported =
    WendyE2EMachine.cli.os == .macOS
    && ProcessInfo.processInfo.environment["WENDY_E2E_AUTH_MODES_DISCOVERY"] == "true"

let authModesLiveEnabled =
    ProcessInfo.processInfo.environment["WENDY_E2E_AUTH_MODES_LIVE"] == "true"

/// Resource-specific, externally booked Open board. This fixture never reserves,
/// installs, flashes or updates a board. A private recovery directory retains the
/// generated keys and journal until cleanup is verified, including after a kill.
struct AuthModesLiveFixture: Sendable, Decodable {
    let device: String
    let resource: String
    let holder: String
    let recoveryDirectory: String
    let cloudGRPC: String?
    let tenant: String?
    let mdnsInstance: String?
    let disposableEnrollment: Bool

    var owner: String { recoveryDirectory + "/owner" }
    func config(_ role: String) -> String { recoveryDirectory + "/" + role }
    func command(_ role: String = "owner", json: Bool = false) -> String {
        "env WENDY_RUN_NO_CLOUD_FALLBACK=1 WENDY_CONFIG_DIR=\(AuthModesFixture.quote(config(role))) wendy --json=\(json) --device \(AuthModesFixture.quote(device))"
    }

    static func load() throws -> Self {
        let env = ProcessInfo.processInfo.environment
        let path = try #require(
            env["WENDY_E2E_AUTH_MODES_TARGET"],
            "Select a private target JSON file"
        )
        let fixture = try JSONDecoder().decode(
            Self.self,
            from: Data(contentsOf: URL(fileURLWithPath: path))
        )
        try #require(authModesLocalTestsSupported && WendyE2EEnvironment.isolation != .none)
        try #require((env["WENDY_AGENT_SOCKET"] ?? "").isEmpty)
        try #require((env["WENDY_CLOUD_INSECURE"] ?? "").isEmpty)
        try #require(
            try HTTPAppFixture.requireLANDevice(WendyE2EEnvironment.deviceAddress) == fixture.device
        )
        try #require(!fixture.resource.contains("/") && !fixture.resource.isEmpty)
        let org = try #require(
            env["RANCH_ORGANIZATION_ROOT"],
            "Provide the canonical reservations root"
        )
        let claim = try String(
            contentsOfFile: org + "/reservations/" + fixture.resource,
            encoding: .utf8
        )
        try #require(claim.split(separator: "\n").contains(Substring("holder: " + fixture.holder)))
        let until = try #require(claim.split(separator: "\n").first { $0.hasPrefix("until: ") })
        let dateText = String(until.dropFirst(7))
        let parser = ISO8601DateFormatter()
        parser.formatOptions = [.withInternetDateTime]
        let expiry =
            parser.date(from: dateText)
            ?? parser.date(from: dateText.replacingOccurrences(of: "Z", with: ":00Z"))
        try #require(try #require(expiry, "Reservation needs an ISO8601 until time") > Date())
        try #require(
            fixture.recoveryDirectory.hasPrefix("/")
                && URL(fileURLWithPath: fixture.recoveryDirectory).lastPathComponent.hasPrefix(
                    "auth-modes-"
                )
        )
        return fixture
    }

    func expect(
        _ cli: WendyE2ESession,
        role: String = "owner",
        contains: String,
        _ args: String
    ) async throws {
        try await cli.sh("\(command(role)) \(args)") { result in
            try #require(result.status.isSuccess)
            #expect(result.stdout.contains(contains))
        }
    }

    func refuse(
        _ cli: WendyE2ESession,
        role: String,
        diagnostic: String,
        _ args: String
    ) async throws {
        try await cli.sh("\(command(role)) \(args)") { result in
            #expect(result.status.isFailure)
            #expect(
                result.stderr.contains(diagnostic),
                "Transport errors do not establish authorization"
            )
        }
    }

    func wait(
        _ cli: WendyE2ESession,
        role: String = "owner",
        shape: String,
        keyless: String? = nil
    ) async throws {
        let expectedRole = shape == "Closed" && role == "owner" ? "owner" : nil
        for attempt in 0..<30 {
            let result = try await cli.sh("\(command(role)) device access") { $0 }
            if result.status.isSuccess && result.stdout.contains("Access: " + shape)
                && (keyless == nil || result.stdout.contains("Keyless apps: " + keyless!))
                && (expectedRole == nil || result.stdout.contains("Your role: " + expectedRole!))
            {
                return
            }
            if attempt < 29 { try await cli.sh("sleep 1") }
        }
        Issue.record("Target did not settle to \(shape) for \(role)")
        throw ReadinessDeadline()
    }
    struct ReadinessDeadline: Error {}

    func add(_ cli: WendyE2ESession, role: String, computer: String? = nil) async throws {
        let computer = computer ?? role
        try await cli.sh(
            "\(command(computer)) auth local-key show > \(AuthModesFixture.quote(config(computer) + ".public"))"
        )
        try await expect(
            cli,
            contains: "as \(role)",
            "device keys add \(AuthModesFixture.quote(config(computer) + ".public")) --role \(role) --label \(role)-fixture"
        )
        try await expect(cli, role: computer, contains: "Your role: " + role, "device access")
    }

    func waitForApps(_ cli: WendyE2ESession, role: String, allowed: Bool) async throws {
        let observer = AuthModesFixture.root + "/.github/ci-tests/auth-modes/readiness.py"
        let expected = allowed ? "allowed" : "denied"
        try await cli.sh(
            "python3 \(AuthModesFixture.quote(observer)) --device \(AuthModesFixture.quote(device)) --config \(AuthModesFixture.quote(config(role))) --expect \(expected)"
        )
    }

    func observeTXT(_ cli: WendyE2ESession, shape: String) async throws {
        try #require(authModesDiscoverySupported)
        let instance = try #require(
            mdnsInstance,
            "Provide the selected target's exact DNS-SD instance"
        )
        try #require(device.hasSuffix(".local"))
        let observer = AuthModesFixture.root + "/.github/ci-tests/auth-modes/observe_txt.py"
        try await cli.sh(
            "python3 \(AuthModesFixture.quote(observer)) --instance \(AuthModesFixture.quote(instance)) --host \(AuthModesFixture.quote(device)) --shape \(AuthModesFixture.quote(shape))"
        )
    }

    func recordApp(_ cli: WendyE2ESession, name: String) async throws {
        let script = """
            import json, os
            from pathlib import Path
            p=Path(\(String(reflecting: recoveryDirectory))) / 'journal.json'
            j=json.loads(p.read_text()); j['apps'].append(\(String(reflecting: name)))
            q=p.with_suffix('.next'); q.write_text(json.dumps(j)); os.replace(q,p)
            """
        try await cli.sh("python3 -c \(AuthModesFixture.quote(script))")
    }

    func run(
        authenticated: Bool = false,
        observeDiscovery: Bool = false,
        filePath: String = #filePath,
        function: String = #function,
        line: Int = #line,
        _ body: @Sendable (Self, WendyE2ESession, WendyE2ESession) async throws -> Void
    ) async throws {
        let fixture = self
        let scenario = CLIAndAgentScenario(after: { cli, _ in
            try await fixture.cleanup(cli)
            try await cli.sh(
                #"rm -f "$WENDY_CONFIG_DIR/local-operator-key.pem" "$WENDY_CONFIG_DIR/config.json""#
            )
        })
        try await scenario.run(
            authenticated: authenticated,
            filePath: filePath,
            function: function,
            line: line
        ) { cli, agent in
            if authenticated {
                try #require(
                    WendyE2EEnvironment.cliAuthConfigPath != nil,
                    "Use the dedicated --cli-auth-config fixture"
                )
                let endpoint = try #require(cloudGRPC)
                let tenantID = try #require(tenant)
                try #require(disposableEnrollment && endpoint.hasSuffix(":443"))
                let validate = """
                    import json, os
                    from pathlib import Path
                    c=json.loads((Path(os.environ['WENDY_CONFIG_DIR'])/'config.json').read_text())
                    assert len(c['auth'])==1
                    a=c['auth'][0]
                    assert a['cloudGRPC']==\(String(reflecting: endpoint))
                    assert len(a['certificates'])==1
                    assert a['certificates'][0]['principalUri'].startswith('spiffe://wendy.sh/tenant/'+\(String(reflecting: tenantID))+'/operator/')
                    print('Dedicated Cloud endpoint and operator tenant verified; credentials omitted')
                    """
                try await cli.sh("python3 -c \(AuthModesFixture.quote(validate))")
            }
            // Reconcile a preceding interrupted booking before creating fresh keys.
            try await cleanup(cli)
            try await cli.sh("\(command("observer")) device access") { result in
                try #require(
                    result.status.isSuccess && result.stdout.contains("Access: Open"),
                    "Pool baseline must be independently Open"
                )
            }
            if observeDiscovery { try await observeTXT(cli, shape: "open") }
            try await cli.sh("wendy auth local-key show > owner.public")
            let journal: [String: String] = ["device": device, "resource": resource]
            let data = try JSONSerialization.data(withJSONObject: journal)
            let setup = """
                set -eu
                mkdir -p \(AuthModesFixture.quote(owner))
                chmod 700 \(AuthModesFixture.quote(recoveryDirectory)) \(AuthModesFixture.quote(owner))
                cp "$WENDY_CONFIG_DIR/local-operator-key.pem" \(AuthModesFixture.quote(owner + "/local-operator-key.pem"))
                if [ -f "$WENDY_CONFIG_DIR/config.json" ]; then
                  cp "$WENDY_CONFIG_DIR/config.json" \(AuthModesFixture.quote(owner + "/config.json"))
                fi
                chmod 600 \(AuthModesFixture.quote(owner))/*
                printf '%s' \(AuthModesFixture.quote(String(decoding: data, as: UTF8.self))) > \(AuthModesFixture.quote(recoveryDirectory + "/initial.json"))
                """
            try await cli.sh(setup)
            let initialize = """
                import json
                from pathlib import Path
                p=Path(\(String(reflecting: recoveryDirectory)))
                j=json.loads((p/'initial.json').read_text()); j['apps']=[]; j['name']='auth-modes-'+__import__('uuid').uuid4().hex[:12]
                (p/'journal.json').write_text(json.dumps(j))
                """
            try await cli.sh(
                "\(command(json: true)) device apps list > \(AuthModesFixture.quote(recoveryDirectory + "/baseline-apps.json"))"
            )
            try await cli.sh("python3 -c \(AuthModesFixture.quote(initialize))")
            try await expect(cli, contains: "Closed.", "device access close --yes")
            try await wait(cli, shape: "Closed", keyless: "off")
            try await expect(cli, contains: "Your role: owner", "device access")
            if observeDiscovery { try await observeTXT(cli, shape: "closed") }
            try await add(cli, role: "operator")
            try await add(cli, role: "viewer")
            try await add(cli, role: "owner", computer: "recovery-owner")
            try await body(self, cli, agent)
        }
    }

    /// Runs after thrown assertions and on the next acquisition after cancellation.
    /// Uses only the journal's exact resources and retains credentials on failure.
    func cleanup(_ cli: WendyE2ESession) async throws {
        let script = """
            import json, os, subprocess, time
            from pathlib import Path
            root=Path(\(String(reflecting: recoveryDirectory)))
            p=root/'journal.json'
            if not p.exists(): raise SystemExit(0)
            j=json.loads(p.read_text())
            assert j['device']==\(String(reflecting: device)) and j['resource']==\(String(reflecting: resource))
            env=dict(os.environ,WENDY_CONFIG_DIR=str(root/'owner'),WENDY_RUN_NO_CLOUD_FALLBACK='1')
            def call(args,check=True,observer=False,attempt=0):
                e=env.copy()
                if observer: e['WENDY_CONFIG_DIR']=str(root/('cleanup-observer-'+str(attempt)))
                r=subprocess.run(['wendy','--json=false','--device',j['device'],*args],env=e,text=True,stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=30)
                if check and r.returncode: raise RuntimeError('cleanup failed: '+str(args)+' (exit '+str(r.returncode)+')')
                return r
            cloud=\(cloudGRPC.map { String(reflecting: $0) } ?? "None")
            if j.get('cloudMayExist'):
                assert cloud
                c=json.loads((root/'owner'/'config.json').read_text())
                assert len(c['auth'])==1 and c['auth'][0]['cloudGRPC']==cloud
                a=c['auth'][0]; assert len(a['certificates'])==1
                assert a['certificates'][0]['principalUri'].startswith('spiffe://wendy.sh/tenant/'+\(tenant.map { String(reflecting: $0) } ?? "''")+'/operator/')
                catalog=json.loads(call(['--json','cloud','discover','--all','--cloud-grpc',cloud]).stdout)
                assets=[x for x in catalog if x['name']==j['name']]
                assert len(assets)<=1
                if assets:
                    import uuid
                    exact=assets[0]['id']; uuid.UUID(exact)
                    assert not j.get('asset') or j['asset']==exact, 'exact asset identity changed'
                    # The public read-only command binds the authenticated device
                    # principal, installed leaf, tenant, endpoint and exact asset.
                    checked=json.loads(call(['--json','device','unenroll','--check','--asset-id',exact,'--cloud-grpc',cloud]).stdout)
                    assert checked['cloudBindingVerified'] is True and checked['assetDeleted'] is False
                    assert checked['principal'].startswith('spiffe://wendy.sh/tenant/'+\(tenant.map { String(reflecting: $0) } ?? "''")+'/device/')
                    j['asset']=exact
                    q=p.with_suffix('.next'); q.write_text(json.dumps(j)); os.replace(q,p)
                    call(['device','unenroll','--yes','--asset-id',assets[0]['id'],'--cloud-grpc',cloud])
                    call(['device','unpin',j['device']])
                elif j.get('asset'):
                    # Exact retained deletion evidence reconciles an interrupted reset.
                    call(['device','unenroll','--yes','--asset-id',j['asset'],'--cloud-grpc',cloud])
                    call(['device','unpin',j['device']])
                else:
                    assert not j.get('enrollSucceeded'), 'enrollment succeeded without a catalog identity'
            # An independently observed Open transition can outlive a stale
            # owner pin (WDY-4393). Only that observation permits exact unpinning.
            observed=call(['device','access'],False,True,attempt='initial-'+__import__('uuid').uuid4().hex)
            if observed.returncode==0 and 'Access: Open' in observed.stdout:
                call(['device','unpin',j['device']])
            state=None
            for _ in range(30):
                r=call(['device','access'],check=False)
                if r.returncode==0: state=r.stdout; break
                time.sleep(1)
            assert state is not None, 'owner did not recover; retain journal and keys'
            for app in j['apps']:
                import uuid
                assert app.startswith('e2e-'); uuid.UUID(app[4:])
                apps=json.loads(call(['--json','device','apps','list']).stdout)
                if any(x['name']==app for x in apps): call(['device','apps','remove',app,'--force'])
            if 'Access: Closed' in state: call(['device','access','open','--yes'])
            elif 'Access: Open' not in state: raise RuntimeError('cleanup requires Cloud-owned unenrollment')
            for _ in range(30):
                r=call(['device','access'],False,True,attempt=_)
                if r.returncode==0 and 'Access: Open' in r.stdout: break
                time.sleep(1)
            else: raise RuntimeError('Open baseline not independently observed')
            call(['device','unpin',j['device']])
            assert 'Access: Open' in call(['device','access']).stdout
            after=json.loads(call(['--json','device','apps','list']).stdout)
            before=json.loads((root/'baseline-apps.json').read_text())
            def inventory(xs): return sorted((x['name'],x.get('runningState'),x.get('version'),x.get('httpPort')) for x in xs)
            assert inventory(after)==inventory(before), 'baseline app inventory changed'
            if j.get('cloudMayExist'):
                assert not any(x['name']==j['name'] for x in json.loads(call(['--json','cloud','discover','--all','--cloud-grpc',cloud]).stdout))
            # Only credentials generated/copied by this fixture are removed.
            for role in ('owner','operator','viewer','stranger','observer','recovery-owner','local-owner-only','open-origin-observer',*[d.name for d in root.glob('cleanup-observer-*') if d.is_dir()]):
                for name in ('local-operator-key.pem','config.json'):
                    (root/role/name).unlink(missing_ok=True)
            p.unlink()
            print('Auth Modes cleanup verified: Open, owned apps absent, baseline inventory restored, exact Cloud name absent')
            """
        try await cli.sh("python3 -c \(AuthModesFixture.quote(script))")
    }
}
