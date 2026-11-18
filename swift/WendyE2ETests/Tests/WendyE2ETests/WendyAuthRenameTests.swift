import Testing
import WendyE2ETesting

@Suite
struct `'wendy auth rename'` {
    let scenario = CLIAndAgentScenario()

    /**
     Renaming the current context changes its name and current pointer together.
     Credentials and unrelated preferences remain intact, and fresh status and default
     processes identify the renamed account without contacting saved endpoints.
     */
    @Test
    func `renames the current context and keeps its identity selected`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await ContextCLIAssertions.run(
                cli,
                body: #"""
                    for args in (('alpha', 'primary'), ('primary',)):
                        f.write(f.original)
                        result = f.run('auth', 'rename', *args, '--json=false')
                        assert result.stdout == '✓ Renamed context "alpha" to "primary".\n'
                        expected = copy.deepcopy(f.original)
                        expected['auth'][0]['name'] = 'primary'
                        expected['currentContext'] = 'primary'
                        f.expect_config(expected)
                        before = f.snapshot()
                        f.expect_status(f.run('cloud', 'status', '--json'), current='primary', names=('primary', 'beta'))
                        result = f.run('auth', 'default', '--json=false')
                        assert result.stdout == 'Current context: primary (%s)\n' % f.label('alpha')
                        assert f.snapshot() == before
                    """#
            )
        }
    }

    /**
     An explicit rename of another context changes that name only. The current
     account, both credential records and unrelated preferences are preserved.
     */
    @Test
    func `renames a noncurrent context without switching accounts`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await ContextCLIAssertions.run(
                cli,
                body: #"""
                    result = f.run('auth', 'rename', 'beta', 'secondary', '--json=false')
                    assert result.stdout == '✓ Renamed context "beta" to "secondary".\n'
                    expected = copy.deepcopy(f.original)
                    expected['auth'][1]['name'] = 'secondary'
                    f.expect_config(expected)
                    before = f.snapshot()
                    f.expect_status(f.run('cloud', 'status', '--json'), names=('alpha', 'secondary'))
                    assert f.run('auth', 'default', '--json=false').stdout == 'Current context: alpha (%s)\n' % f.label('alpha')
                    assert f.snapshot() == before
                    """#
            )
        }
    }

    /**
     A context cannot take an existing name. The error identifies the collision
     and preserves the configuration bytes and current selection.
     */
    @Test
    func `rejects a name collision without modifying either account`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await ContextCLIAssertions.run(
                cli,
                body: #"""
                    before = f.snapshot()
                    result = f.run('auth', 'rename', 'alpha', 'beta', '--json=false', exit=1)
                    assert 'a context named "beta" already exists' in result.stderr
                    assert f.snapshot() == before
                    """#
            )
        }
    }
}
