import Foundation
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

    /**
     **CURRENT OBSERVED FAILURE:** With saved `defaultOrgId: 73`, clearing reports
     success but a fresh process selects beta again. With saved `defaultCloudGRPC`,
     it selects alpha again. This spec is disabled by default. Set
     `WENDY_E2E_KNOWN_CONTEXT_DEFECTS=1` and filter `wendy auth default` to replay
     both retained failing cases.

     Required behavior: clearing leaves neither saved account current in fresh
     default/status processes, even when legacy selection preferences were saved.
     Both accounts, credentials, device preference, analytics and unrelated settings
     survive. This specifies public selection behavior without requiring a particular
     representation of cleared or legacy preferences on disk.

     Tracked by [WDY-4299 — wendy auth default --clear silently reselects an account from legacy defaults](https://linear.app/wendylabsinc/issue/WDY-4299/wendy-auth-default-clear-silently-reselects-an-account-from-legacy).
     */
    @Test(
        .disabled(
            if: ProcessInfo.processInfo.environment["WENDY_E2E_KNOWN_CONTEXT_DEFECTS"] != "1",
            "[WDY-4299 — wendy auth default --clear silently reselects an account from legacy defaults](https://linear.app/wendylabsinc/issue/WDY-4299/wendy-auth-default-clear-silently-reselects-an-account-from-legacy). Replay both cases with WENDY_E2E_KNOWN_CONTEXT_DEFECTS=1."
        )
    )
    func `keeps a cleared selection unset when legacy preferences are saved`() async throws {
        try await self.scenario.run(authenticated: false) { cli, _ in
            try await ContextCLIAssertions.run(
                cli,
                body: #"""
                    failures = []
                    for field, value in (('defaultOrgId', 73), ('defaultCloudGRPC', f.endpoint)):
                        seeded = copy.deepcopy(f.original)
                        seeded[field] = value
                        f.write(seeded)
                        before = f.snapshot()
                        result = f.run('auth', 'default', '--clear', '--json=false')
                        assert result.stdout == '✓ Current context cleared.\n'
                        cleared = json.loads(f.path.read_text())
                        # Preserve account material and unrelated preferences; permit any
                        # representation of the cleared selection, including new fields.
                        for key in f.original:
                            if key != 'currentContext':
                                assert cleared[key] == seeded[key], 'clear changed preserved field: ' + key
                        for path, contents in before.items():
                            if path != 'config.json':
                                assert f.snapshot()[path] == contents, 'clear changed unrelated file'
                        saved = f.snapshot()
                        default = f.run('auth', 'default', '--json=false')
                        status = f.run('cloud', 'status', '--json')
                        current = [s['context'] for s in json.loads(status.stdout)['sessions'] if s.get('current')]
                        print(json.dumps({'legacyField': field, 'freshDefault': default.stdout.strip(), 'freshCurrent': current}))
                        if default.stdout != 'No current context set.\n':
                            failures.append(field + ': fresh default silently reselected an account')
                        try:
                            f.expect_status(status, current=None)
                        except AssertionError:
                            failures.append(field + ': fresh status incorrectly marks an account current')
                        assert f.snapshot() == saved, 'inspection changed saved configuration'
                    assert not failures, '; '.join(failures)
                    """#
            )
        }
    }
}
