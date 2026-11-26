import Foundation
import Testing
import WendyE2ETesting

extension AppsDeploymentFixture {
    private struct OutageState: Decodable {
        let unit: String
        let active: Bool
        let binarySHA256: String
        let execStart: String
        let tasks: [String]
        let restartArmed: Bool?
        let stopObserved: Bool?
        let restoreObserved: Bool?
        let temporaryStateRemoved: Bool?
        let outageSeconds: Double?
    }

    /// Ordinary service stop with an independent, pre-armed systemd restart.
    /// Never use the stopped Agent as the means of starting it again. No
    /// binary, service override, trust or enrollment configuration is changed.
    func withIndependentAgentRestart(
        _ cli: WendyE2ESession,
        _ body: @Sendable () async throws -> Void
    ) async throws {
        try #require(appsDedicatedDeviceTestsEnabled)
        try #require(ProcessInfo.processInfo.environment["WENDY_E2E_APPS_SYSTEMD_OUTAGE"] == "true")
        // The approved native Linux Agent exposes mTLS at its normal port.
        // Prove this port is reachable before relying on its closure as outage
        // evidence. A mismatched/nondefault listener fails before service writes.
        try await waitForAgentPort(cli, open: true, seconds: 3)
        let restoreBoundary = ContinuousClock.now.advanced(by: .seconds(65))
        var baseline: OutageState?
        var primaryError: (any Error)?
        do {
            let armed = try await succeed(cli, systemdOutageCommand(mode: "arm"))
            baseline = try JSONDecoder().decode(
                OutageState.self,
                from: Data(armed.normalizedStdout.utf8)
            )
            try #require(baseline?.active == true && baseline?.restartArmed == true)
            try await waitForAgentPort(cli, open: false, seconds: 12)
            try await body()
        } catch { primaryError = error }

        do {
            // Give the independent restart its entire bounded window, even if
            // the body failed before observing the stop. An interrupted runner
            // still leaves the on-device timer responsible for recovery.
            if ContinuousClock.now < restoreBoundary {
                try await Task.sleep(until: restoreBoundary, clock: .continuous)
            }
            try await waitForAgentPort(cli, open: true, seconds: 45)
            try await verifyIdentity(cli)
            let finished = try await succeed(cli, systemdOutageCommand(mode: "finish"))
            let after = try JSONDecoder().decode(
                OutageState.self,
                from: Data(finished.normalizedStdout.utf8)
            )
            try #require(after.active)
            #expect(after.unit == baseline?.unit)
            #expect(after.binarySHA256 == baseline?.binarySHA256)
            #expect(after.execStart == baseline?.execStart)
            #expect(
                after.tasks == baseline?.tasks,
                "Unrelated containerd task identities and PIDs survive the Agent outage"
            )
            #expect(after.stopObserved == true)
            #expect(after.restoreObserved == true)
            #expect(after.temporaryStateRemoved == true)
            let duration = try #require(after.outageSeconds)
            #expect(duration > 0 && duration <= 65, "The observed outage must remain bounded")
        } catch {
            Issue.record(
                "Independent Agent restart audit failed for \(device); inspect the owned systemd outage receipt before releasing the device"
            )
            primaryError = primaryError ?? error
        }
        if let primaryError { throw primaryError }
    }

    private func waitForAgentPort(_ cli: WendyE2ESession, open: Bool, seconds: Int) async throws {
        let code = #"""
            import datetime,json,socket,sys,time
            host,want,seconds=sys.argv[1],sys.argv[2]=='open',int(sys.argv[3])
            deadline=time.monotonic()+seconds
            while True:
                try:
                    with socket.create_connection((host,50052),timeout=1): current=True
                except OSError: current=False
                if current==want:
                    print(json.dumps({'utc':datetime.datetime.now(datetime.timezone.utc).isoformat(),'host':host,'port':50052,'open':current}));break
                if time.monotonic()>=deadline: raise RuntimeError('Expected bounded Agent port state was not observed')
                time.sleep(.25)
            """#
        _ = try await succeed(
            cli,
            "python3 -c \(Self.quote(code)) \(Self.quote(device)) \(open ? "open" : "closed") \(seconds)"
        )
    }

    private func systemdOutageCommand(mode: String) -> String {
        "wendy --device \(Self.quote(device)) device shell -- python3 -c \(Self.quote(Self.systemdOutageControl)) \(Self.quote(attempt)) \(Self.quote(mode)) \(Self.quote(app))"
    }

    private static let systemdOutageControl = #"""
        import datetime,hashlib,json,pathlib,re,shutil,subprocess,sys
        attempt,mode,app=sys.argv[1:]
        assert re.fullmatch(r'sh\.wendy\.e2e\.apps-[0-9a-f-]{36}',app), 'Owned app required'
        assert re.fullmatch(r'[0-9a-f-]{36}',attempt), 'Owned attempt required'
        root=pathlib.Path('/run/wendy-e2e-outage-'+attempt)
        prefix='wendy-e2e-outage-'+attempt
        restart=prefix+'-restart'
        stop=prefix+'-stop'
        allowed=['wendyos-agent.service','wendy-agent.service']
        def now(): return datetime.datetime.now(datetime.timezone.utc).isoformat()
        def call(args): return subprocess.run(args,check=True,capture_output=True,text=True).stdout.strip()
        def active(unit): return subprocess.run(['systemctl','is-active','--quiet',unit]).returncode==0
        def state(unit):
            props=dict(line.split('=',1) for line in call(['systemctl','show',unit,'--property=ActiveState,MainPID,ExecStart']).splitlines())
            pid=int(props['MainPID']);assert pid>0 and props['ActiveState']=='active', 'Healthy Agent required'
            binary=pathlib.Path('/proc')/str(pid)/'exe'
            tasks=sorted(line for line in call(['ctr','--address','/run/containerd/containerd.sock','--namespace','default','tasks','list']).splitlines()[1:] if line.split() and line.split()[0]!=app and not line.split()[0].startswith(app+'_'))
            return {'unit':unit,'active':True,'pid':pid,'utc':now(),'binarySHA256':hashlib.sha256(binary.read_bytes()).hexdigest(),'execStart':props['ExecStart'].split('; start_time=')[0].strip(),'tasks':tasks}
        if mode=='arm':
            units=[unit for unit in allowed if active(unit)]
            assert len(units)==1, 'Exactly one supported active Agent service required'
            unit=units[0];before=state(unit)
            root.mkdir(mode=0o700)
            (root/'baseline.json').write_text(json.dumps(dict(before,attempt=attempt,app=app)))
            # The manager owns both jobs. Restart is installed and verified
            # before a delayed stop is even scheduled. The worker writes proof
            # after systemctl returns; it never depends on the Agent transport.
            restart_code="import datetime,json,pathlib,subprocess; p=pathlib.Path("+repr(str(root))+ "); u="+repr(unit)+"; r=subprocess.run(['systemctl','start',u]); p.joinpath('restart.json').write_text(json.dumps({'utc':datetime.datetime.now(datetime.timezone.utc).isoformat(),'ok':r.returncode==0,'active':subprocess.run(['systemctl','is-active','--quiet',u]).returncode==0}))"
            call(['systemd-run','--unit',restart,'--on-active=60s','--timer-property=AccuracySec=1s','--collect','/usr/bin/python3','-c',restart_code])
            assert active(restart+'.timer'), 'Independent restart must be armed first'
            stop_code="import datetime,json,pathlib,subprocess; p=pathlib.Path("+repr(str(root))+ "); u="+repr(unit)+"; r=subprocess.run(['systemctl','stop',u]); p.joinpath('stop.json').write_text(json.dumps({'utc':datetime.datetime.now(datetime.timezone.utc).isoformat(),'ok':r.returncode==0,'inactive':subprocess.run(['systemctl','is-active','--quiet',u]).returncode!=0}))"
            call(['systemd-run','--unit',stop,'--on-active=2s','--timer-property=AccuracySec=1s','--collect','/usr/bin/python3','-c',stop_code])
            print(json.dumps(dict(before,restartArmed=True,restartUnit=restart+'.timer',stopUnit=stop+'.timer',scheduledRestartAfterSeconds=60)))
        elif mode=='finish':
            assert root.is_dir() and not root.is_symlink(), 'Owned outage receipt required'
            before=json.loads((root/'baseline.json').read_text());assert before['attempt']==attempt and before['app']==app and before['unit'] in allowed
            unit=before['unit'];after=state(unit)
            stopped=json.loads((root/'stop.json').read_text()) if (root/'stop.json').exists() else {}
            restored=json.loads((root/'restart.json').read_text()) if (root/'restart.json').exists() else {}
            subprocess.run(['systemctl','stop',stop+'.timer',restart+'.timer'],capture_output=True)
            assert not active(stop+'.timer') and not active(restart+'.timer'), 'Owned timers must be inactive'
            duration=(datetime.datetime.fromisoformat(restored['utc'])-datetime.datetime.fromisoformat(stopped['utc'])).total_seconds() if 'utc' in stopped and 'utc' in restored else None
            shutil.rmtree(root)
            print(json.dumps(dict(after,stopObserved=stopped.get('ok',False) and stopped.get('inactive',False),restoreObserved=restored.get('ok',False) and restored.get('active',False),stoppedUTC=stopped.get('utc'),restoredUTC=restored.get('utc'),outageSeconds=duration,temporaryStateRemoved=not root.exists())))
        else: raise RuntimeError('Unknown fixture operation')
        """#
}
