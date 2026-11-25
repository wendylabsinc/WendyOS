import Testing
import WendyE2ETesting

@Suite
struct `'wendy auth use'` {
    let scenario = CLIAndAgentScenario()

    /**
     Selects the named account even when another account uses the same Cloud endpoint.
     Each fresh CLI process sees the selection; switching changes only the current
     context pointer and preserves every credential, build-host preference and analytics setting.
     No configured Cloud, Auth or PKI endpoint is contacted.
     */
    @Test
    func `persists a named selection between accounts at the same endpoint`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await ContextCLIAssertions.run(
                cli,
                body: #"""
                    for name in ('beta', 'alpha', 'beta'):
                        before = copy.deepcopy(f.original)
                        before['currentContext'] = name
                        result = f.run('auth', 'use', name, '--json=false')
                        assert result.stdout == '✓ Switched to context "%s" (%s).\n' % (name, f.label(name))
                        f.expect_config(before)
                        saved = f.snapshot()
                        f.expect_status(f.run('cloud', 'status', '--json'), current=name)
                        result = f.run('auth', 'default', '--json=false')
                        assert result.stdout == 'Current context: %s (%s)\n' % (name, f.label(name))
                        assert f.snapshot() == saved, 'read-only follow-up mutated config'
                    """#
            )
        }
    }

    /**
     An unknown name reports the locally stored choices and the login command for
     adding another account. It fails without changing configuration or querying Cloud.
     */
    @Test
    func `rejects an unknown selector without changing saved contexts`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await ContextCLIAssertions.run(
                cli,
                body: #"""
                    before = f.snapshot()
                    result = f.run('auth', 'use', 'missing-account', '--json=false', exit=1)
                    for text in ('no auth session matches "missing-account"', 'Sessions stored here:', f.label('alpha'), f.label('beta'), 'wendy auth login', 'does not query the cloud'):
                        assert text in result.stderr, 'missing actionable selector guidance'
                    assert f.snapshot() == before
                    """#
            )
        }
    }

    /**
     An endpoint shared by two saved accounts is ambiguous. The error identifies
     both candidates and leaves all saved state byte-for-byte unchanged.
     */
    @Test
    func `rejects an ambiguous shared endpoint without choosing an account`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await ContextCLIAssertions.run(
                cli,
                body: #"""
                    before = f.snapshot()
                    result = f.run('auth', 'use', f.endpoint, '--json=false', exit=1)
                    for text in ('matches multiple sessions', f.label('alpha'), f.label('beta')):
                        assert text in result.stderr, 'missing ambiguity detail'
                    assert f.snapshot() == before
                    """#
            )
        }
    }

    /**
     An incomplete context cannot become current. The error names it and tells
     the user to log in again, without contacting any saved endpoint or changing state.
     */
    @Test
    func `rejects a named context without a certificate`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await ContextCLIAssertions.run(
                cli,
                body: #"""
                    incomplete = copy.deepcopy(f.original)
                    incomplete['auth'][1].pop('certificates')
                    f.write(incomplete)
                    before = f.snapshot()
                    result = f.run('auth', 'use', 'beta', '--json=false', exit=1)
                    assert 'auth context "beta" has no certificates' in result.stderr
                    assert 'wendy auth login' in result.stderr
                    assert f.snapshot() == before
                    """#
            )
        }
    }

    /**
     Without an argument, scripted use fails promptly with a request for a
     context name. No picker opens, no saved endpoint is contacted and no state changes.
     */
    @Test
    func `requires a context name without an interactive terminal`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await ContextCLIAssertions.run(
                cli,
                body: #"""
                    before = f.snapshot()
                    result = f.run('auth', 'use', '--json=false', exit=1)
                    assert 'provide a context name when not running interactively' in result.stderr
                    assert f.snapshot() == before
                    """#
            )
        }
    }
}
