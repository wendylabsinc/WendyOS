import Foundation
import Testing
import WendyE2ETesting

extension AuthModesLiveAcceptance {
    @Suite(
        .serialized,
        .enabled(
            if: authModesLiveEnabled && authModesLocalTestsSupported
                && ProcessInfo.processInfo.environment["WENDY_E2E_AUTH_MODES_CLOUD"] == "true"
        )
    )
    struct `'Auth Modes Cloud acceptance'` {
        /** A25/A26. The real CLI enrolls and performs Cloud-owned removal. Local
         keys, saved keyless state and server identity must return unchanged. */
        @Test
        func `A25 A26 restores Closed keyless off after Cloud removal`() async throws {
            try await closedCycle(keyless: false)
        }

        @Test
        func `A25 A26 restores Closed keyless on after Cloud removal`() async throws {
            try await closedCycle(keyless: true)
        }

        private func closedCycle(
            keyless: Bool,
            filePath: String = #filePath,
            function: String = #function,
            line: Int = #line
        ) async throws {
            try await AuthModesLiveFixture.load().run(
                authenticated: true,
                filePath: filePath,
                function: function,
                line: line
            ) { fixture, cli, _ in
                try await fixture.expect(
                    cli,
                    contains: "Keyless apps " + (keyless ? "on" : "off"),
                    "device access keyless " + (keyless ? "on" : "off")
                )
                try await fixture.wait(cli, shape: "Closed", keyless: keyless ? "on" : "off")
                try await fixture.cloudCycle(cli, closedOrigin: true, keyless: keyless)
            }
        }

        @Test(.enabled(if: authModesDiscoverySupported))
        func `A20 target-only TXT follows Open Closed Enrolled and returned Closed`() async throws {
            try await AuthModesLiveFixture.load().run(authenticated: true, observeDiscovery: true) {
                fixture,
                cli,
                _ in
                try await fixture.cloudCycle(
                    cli,
                    closedOrigin: true,
                    keyless: false,
                    observeDiscovery: true
                )
            }
        }

        @Test
        func `A26 Open origin returns Open without inventing trusted keys`() async throws {
            try await AuthModesLiveFixture.load().run(authenticated: true) { fixture, cli, _ in
                try await fixture.expect(cli, contains: "Open.", "device access open --yes")
                // Every attempt uses a fresh config, so the observer cannot repin
                // the outgoing Closed listener and mistake it for the settled state.
                try await cli.sh("sleep 3")
                try await fixture.wait(cli, role: "open-origin-observer", shape: "Open")
                try await fixture.cloudCycle(cli, closedOrigin: false, keyless: false)
            }
        }
    }

}

extension AuthModesLiveFixture {
    func cloudCycle(
        _ cli: WendyE2ESession,
        closedOrigin: Bool,
        keyless: Bool,
        observeDiscovery: Bool = false
    ) async throws {
        let cloud = try #require(cloudGRPC)
        let tenantID = try #require(tenant)
        let instance = observeDiscovery ? try #require(mdnsInstance) : ""
        let script = """
            import json, os, subprocess, time, uuid
            from pathlib import Path
            root=Path(\(String(reflecting: recoveryDirectory)))
            p=root/'journal.json'; j=json.loads(p.read_text())
            env=dict(os.environ,WENDY_CONFIG_DIR=str(root/'owner'),WENDY_RUN_NO_CLOUD_FALLBACK='1')
            def call(args,ok=True,role='owner'):
                e=env.copy(); e['WENDY_CONFIG_DIR']=str(root/role)
                r=subprocess.run(['wendy','--json=false','--device',j['device'],*args],env=e,text=True,stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=90)
                print('wendy '+' '.join(args)+' -> exit '+str(r.returncode),flush=True)
                if ok and r.returncode: raise RuntimeError('CLI operation failed; credentials and journal retained')
                return r
            def read(args,role='owner'): return json.loads(call(['--json',*args],role=role).stdout)
            cloud=\(String(reflecting: cloud)); tenant=\(String(reflecting: tenantID))
            closed=\(closedOrigin ? "True" : "False"); keyless=\(keyless ? "True" : "False")
            catalog=lambda: read(['cloud','discover','--all','--cloud-grpc',cloud])
            assert not any(x['name']==j['name'] for x in catalog())
            local_before=read(['auth','local-key','show'])['fingerprint']
            keys_before=read(['device','keys','list'])['keys'] if closed else []
            pins_before=json.loads((root/'owner'/'known_devices.json').read_text()) if closed else {}
            local_key='local:'+j['device'].lower().removesuffix('.').removesuffix('.local')
            if closed: assert local_key in pins_before
            j['cloudMayExist']=True
            q=p.with_suffix('.next'); q.write_text(json.dumps(j)); os.replace(q,p)
            call(['device','enroll','--name',j['name'],'--cloud-grpc',cloud])
            assets=[x for x in catalog() if x['name']==j['name']]
            assert len(assets)==1
            asset=assets[0]['id']; uuid.UUID(asset)
            checked=read(['device','unenroll','--check','--asset-id',asset,'--cloud-grpc',cloud])
            assert checked['cloudBindingVerified'] is True and checked['assetDeleted'] is False
            assert checked['principal'].startswith('spiffe://wendy.sh/tenant/'+tenant+'/device/')
            j['asset']=asset; j['principal']=checked['principal']; j['enrollSucceeded']=True
            q.write_text(json.dumps(j)); os.replace(q,p)
            assert read(['device','access'])['shape']=='enrolled'
            def txt(shape):
                if \(observeDiscovery ? "True" : "False"):
                    subprocess.run(['python3',\(String(reflecting: AuthModesFixture.root + "/.github/ci-tests/auth-modes/observe_txt.py")),'--instance',\(String(reflecting: instance)),'--host',j['device'],'--shape',shape],check=True,timeout=30)
            txt('enrolled')
            assert read(['auth','local-key','show'])['fingerprint']==local_before
            # Same local owner key, no Cloud certificate or sign-in session.
            import shutil
            local_only=root/'local-owner-only'; local_only.mkdir(exist_ok=True,mode=0o700)
            shutil.copyfile(root/'owner'/'local-operator-key.pem',local_only/'local-operator-key.pem')
            os.chmod(local_only/'local-operator-key.pem',0o600)
            refusal=call(['device','access'],ok=False,role='local-owner-only')
            assert refusal.returncode and 'wendy auth login' in refusal.stderr
            for args in (
                ['device','access','close','--yes'],['device','access','open','--yes'],
                ['device','access','keyless','on'],['device','keys','list'],
                ['device','keys','add',str(root/'viewer.public'),'--role','viewer'],
                ['device','keys','remove',local_before,'--yes']):
                r=call(args,ok=False)
                assert r.returncode and 'Cloud manages' in r.stderr
                assert read(['device','access'])['shape']=='enrolled'
                assert any(x['id']==asset and x['name']==j['name'] for x in catalog())
            # Independently recheck the binding immediately before deletion.
            checked=read(['device','unenroll','--check','--asset-id',asset,'--cloud-grpc',cloud])
            assert checked['cloudBindingVerified'] and checked['principal']==j['principal']
            call(['device','unenroll','--yes','--asset-id',asset,'--cloud-grpc',cloud])
            assert not any(x['id']==asset or x['name']==j['name'] for x in catalog())
            # Successful signed reset plus catalog deletion authorizes deliberate
            # exact-name pin cleanup; no network failure triggers an unpin.
            call(['device','unpin',j['device']])
            wanted='closed' if closed else 'open'
            for _ in range(30):
                r=call(['--json','device','access'],ok=False)
                if not r.returncode:
                    state=json.loads(r.stdout)
                    # A Closed-origin reset must never be accepted as Open.
                    assert not closed or state['shape']!='open'
                    if state['shape']==wanted: break
                time.sleep(1)
            else: raise RuntimeError('post-unenrollment shape did not settle')
            if closed:
                assert state['keylessApps']==keyless and state['yourRole']=='owner'
                assert read(['device','keys','list'])['keys']==keys_before
                assert len(keys_before)==4
                for role in ('operator','viewer','recovery-owner'):
                    # These configs legitimately retain a local pin from before
                    # enrollment; normal reconnect must recover the original role.
                    r=read(['device','access'],role=role)
                    assert r['shape']=='closed' and r['yourRole']==('owner' if role=='recovery-owner' else role)
                pins_after=json.loads((root/'owner'/'known_devices.json').read_text())
                assert pins_before[local_key]['spkiFingerprint']==pins_after[local_key]['spkiFingerprint']
            else:
                r=call(['device','keys','list'],ok=False)
                assert r.returncode and 'close it first' in r.stderr
            txt(wanted)
            j['cloudMayExist']=False; j.pop('asset',None); j.pop('enrollSucceeded',None)
            q.write_text(json.dumps(j)); os.replace(q,p)
            print(json.dumps({'scenario':'A25/A26','origin':wanted,'keyless':keyless,'asset':asset,'catalogDeleted':True,'localFingerprintPreserved':True}),flush=True)
            """
        try await cli.sh("python3 -c \(AuthModesFixture.quote(script))")
    }
}
