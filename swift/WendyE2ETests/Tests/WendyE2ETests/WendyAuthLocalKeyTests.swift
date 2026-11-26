import Foundation
import Testing
import WendyE2ETesting

@Suite(.serialized, .enabled(if: authModesLocalTestsSupported, "Requires a local POSIX CLI"))
struct `'wendy auth local-key show'` {
    /** A01. Fresh signed-out CLI processes keep one public identity per config.
     Private files remain 0600 and an unsafe mode never silently replaces a key.
     Cloud sign-in preservation is recorded separately in the live lifecycle. */
    @Test
    func `A01 keeps one local key independently of Cloud sign-in`() async throws {
        try await CLIAndAgentScenario().run(authenticated: false) { cli, _ in
            try await cli.sh("wendy auth local-key show --json > first.json")
            try await cli.sh("wendy auth local-key show --json > second.json")
            try await cli.sh(
                "WENDY_CONFIG_DIR=\"$HOME/other-config\" wendy auth local-key show --json > other.json"
            )
            let script = #"""
                import base64, hashlib, json, os, stat
                a,b,c=[json.load(open(p)) for p in ('first.json','second.json','other.json')]
                assert a == b and a['fingerprint'] != c['fingerprint']
                der=base64.b64decode(''.join(a['publicKey'].splitlines()[1:-1]))
                assert bytes.fromhex('0609608648016503040312') in der, 'ML-DSA-65 SPKI OID required'
                assert a['fingerprint'] == 'sha256:'+hashlib.sha256(der).hexdigest()
                assert stat.S_IMODE(os.stat(a['path']).st_mode) == 0o600
                assert 'PRIVATE KEY' not in json.dumps(a)
                # Only a public fingerprint is recorded. No private file is printed.
                print(a['fingerprint'])
                os.chmod(a['path'],0o644)
                """#
            try await cli.sh("python3 -c \(AuthModesFixture.quote(script))")
            try await cli.sh("wendy auth local-key show --json") { result in
                #expect(result.status.isFailure)
                #expect(result.stderr.contains("chmod 600"))
                #expect(!result.stdout.contains("PRIVATE KEY"))
            }
            try await cli.sh(
                #"chmod 600 "$WENDY_CONFIG_DIR/local-operator-key.pem"; wendy auth local-key show --json > recovered.json"#
            )
            try await cli.sh("cmp first.json recovered.json")
        }
    }
}
