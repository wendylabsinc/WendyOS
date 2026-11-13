import Testing
import WendyE2ETesting

@Suite
struct `'wendy auth default'` {
    let scenario = CLIAndAgentScenario()

    /**
     Clearing the current context removes the preference, preserving both named
     accounts, all credentials and unrelated settings. Fresh status reports both
     accounts with neither marked current; a later explicit use still works offline.
     */
    @Test
    func `clears only the selection while retaining both saved accounts`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await ContextCLIAssertions.run(
                cli,
                body: #"""
                    result = f.run('auth', 'default', '--clear', '--json=false')
                    assert result.stdout == '✓ Current context cleared.\n'
                    expected = copy.deepcopy(f.original)
                    expected.pop('currentContext')
                    f.expect_config(expected)
                    before = f.snapshot()
                    assert f.run('auth', 'default', '--json=false').stdout == 'No current context set.\n'
                    f.expect_status(f.run('cloud', 'status', '--json'), current=None)
                    assert f.snapshot() == before
                    assert f.run('auth', 'use', 'beta', '--json=false').stdout == '✓ Switched to context "beta" (%s).\n' % f.label('beta')
                    expected['currentContext'] = 'beta'
                    f.expect_config(expected)
                    """#
            )
        }
    }

    /**
     A current pointer whose context is missing is cleared with a warning.
     Only that stale preference changes; credentials survive and a second process
     reports no current context without changing the file again.
     */
    @Test
    func `warns and clears a stale current pointer while preserving accounts`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await ContextCLIAssertions.run(
                cli,
                body: #"""
                    stale = copy.deepcopy(f.original)
                    stale['currentContext'] = 'removed-account'
                    f.write(stale)
                    result = f.run('auth', 'default', '--json=false')
                    assert result.stdout == '⚠ Current context "removed-account" no longer exists; clearing it.\n'
                    expected = copy.deepcopy(f.original)
                    expected.pop('currentContext')
                    f.expect_config(expected)
                    before = f.snapshot()
                    assert f.run('auth', 'default', '--json=false').stdout == 'No current context set.\n'
                    f.expect_status(f.run('cloud', 'status', '--json'), current=None)
                    assert f.snapshot() == before
                    """#
            )
        }
    }
}
