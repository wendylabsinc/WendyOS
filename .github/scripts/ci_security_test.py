#!/usr/bin/env python3
"""Exercise workflow trust boundaries and reject unsafe artifact identifiers."""
from pathlib import Path
import os
import re
import subprocess
import unittest

ROOT = Path(__file__).resolve().parents[2]


def job(filename, name):
    text = (ROOT / '.github/workflows' / filename).read_text()
    match = re.search(rf'^  {re.escape(name)}:\n(.*?)(?=^  [a-zA-Z0-9_-]+:\n|\Z)', text, re.M | re.S)
    if not match:
        raise AssertionError(f'Missing job {name}')
    return match[1]


def permissions_values(text, indent):
    """Return each `permissions:` value at this indent, inline or as a block."""
    lines = text.splitlines()
    prefix = ' ' * indent + 'permissions:'
    values = []
    for index, line in enumerate(lines):
        if not line.startswith(prefix):
            continue
        value = [line[len(prefix):].split('#', 1)[0]]
        for nested in lines[index + 1:]:
            content = nested.split('#', 1)[0]
            if not content.strip():
                continue
            if len(nested) - len(nested.lstrip()) <= indent:
                break
            value.append(content)
        values.append(' '.join(value))
    return values


def grants_write(text, indent):
    # Matches block and flow-style `scope: write` as well as `write-all`.
    return any(re.search(r'\bwrite\b', value) for value in permissions_values(text, indent))


class WorkflowSecurityTests(unittest.TestCase):
    def test_permission_parser_catches_write_forms(self):
        for snippet, expected in [
            ('permissions:\n  contents: read\n  pull-requests: write\nfoo: 1\n', True),
            ('permissions: write-all\n', True),
            ('permissions: { contents: read, pull-requests: write }\n', True),
            ('permissions:\n  # never write here\n  contents: read\njobs:\n  x: write\n', False),
            ('permissions: read-all\n', False),
            ('permissions: {}\n', False),
        ]:
            with self.subTest(snippet=snippet):
                self.assertEqual(grants_write(snippet, 0), expected)
        self.assertTrue(grants_write('    permissions: write-all\n', 4))
        self.assertFalse(grants_write('      permissions: write-all\n', 4))

    def test_code_execution_jobs_have_no_write_tokens(self):
        for filename, names in [('swift-tests.yml', ['test', 'build', 'lint']),
                                ('swift-e2e-tests.yml', ['swift-e2e-local-macos', 'swift-e2e-local-ubuntu'])]:
            text = (ROOT / '.github/workflows' / filename).read_text()
            defaults = text.split('\njobs:', 1)[0]
            self.assertFalse(grants_write(defaults, 0), filename)
            for name in names:
                self.assertFalse(grants_write(job(filename, name), 4), name)

    def test_lint_comment_job_does_not_execute_pr_code(self):
        comment = job('swift-tests.yml', 'lint-comment')
        self.assertNotIn('actions/checkout@', comment)
        self.assertNotRegex(comment, r'(?m)^        run:')
        self.assertIn('github.event.pull_request.head.repo.full_name == github.repository', comment)
        self.assertIn('needs.lint.outputs.lint_output', comment)
        self.assertIn('pull-requests: write', comment)

    def test_e2e_analysis_executes_base_but_reviews_head(self):
        analyze = job('swift-e2e-tests.yml', 'swift-e2e-analyze')
        self.assertIn('ref: ${{ github.event.pull_request.base.sha || github.sha }}', analyze)
        self.assertIn('persist-credentials: false', analyze)
        self.assertIn('git fetch --no-tags origin "$PR_HEAD_SHA"', analyze)
        self.assertIn('--diff "${PR_BASE_SHA}...${PR_HEAD_SHA}"', analyze)
        self.assertNotIn('git checkout', analyze)

    def test_artifact_ids_cannot_escape_or_inject_paths(self):
        analyze = job('swift-e2e-tests.yml', 'swift-e2e-analyze')
        condition = re.search(r'if (\[\[ ! "\$\{attempt_id\}" =~ .*? \]\]); then', analyze)[1]
        for identifier, expected in [('swift-e2e-tests.gh123.linux.0001', 1), ('..', 0),
                                     ('.', 0), ('../outside', 0), ('a/b', 0),
                                     ('x\ny', 0), ('', 0), ('a' * 201, 0), ('x;echo bad', 0)]:
            with self.subTest(identifier=identifier):
                result = subprocess.run(['bash', '-c', condition], env={**os.environ, 'attempt_id': identifier})
                self.assertEqual(result.returncode, expected)

    def test_audit_gates_share_one_script(self):
        for filename, name in [('go-tests.yml', 'workflow-security'),
                               ('swift-e2e-tests.yml', 'swift-e2e-local-ubuntu')]:
            with self.subTest(job=name):
                block = job(filename, name)
                self.assertIn('run: bash .github/scripts/workflow-audit.sh', block)
                self.assertNotIn('zizmor', block)
        script = (ROOT / '.github/scripts/workflow-audit.sh').read_text()
        self.assertIn('--require-hashes', script)
        self.assertIn('--no-config --no-ignores', script)
        requirements = (ROOT / '.github/scripts/workflow-audit-requirements.txt').read_text()
        self.assertRegex(requirements, r'(?m)^zizmor==\d+\.\d+\.\d+ \\$')
        self.assertRegex(requirements, r'--hash=sha256:[0-9a-f]{64}')

    def test_e2e_audit_failure_fails_job_after_suite(self):
        ubuntu = job('swift-e2e-tests.yml', 'swift-e2e-local-ubuntu')
        audit = ubuntu.index('id: workflow-audit')
        suite = ubuntu.index('uses: ./.github/actions/swift-e2e-run')
        fail = ubuntu.index("steps.workflow-audit.outcome == 'failure'")
        self.assertLess(audit, suite)
        self.assertLess(suite, fail)
        self.assertIn('continue-on-error: true', ubuntu[audit:suite])
        self.assertIn('exit 1', ubuntu[fail:])

    def test_security_failure_cannot_pass_go_aggregate(self):
        aggregate = job('go-tests.yml', 'test')
        self.assertIn('workflow-security', aggregate)
        commands = re.search(r'(?m)^        run: \|\n((?:          .*\n)+)', aggregate)[1]
        commands = '\n'.join(line[10:] for line in commands.splitlines())
        names = re.findall(r'^          (\w+_RESULT):', aggregate, re.M)
        for outcome in ['success', 'failure', 'cancelled', 'skipped']:
            env = {**os.environ, **{name: 'success' for name in names}, 'WORKFLOW_SECURITY_RESULT': outcome}
            result = subprocess.run(['bash', '-e', '-c', commands], env=env)
            self.assertEqual(result.returncode == 0, outcome == 'success')


if __name__ == '__main__':
    unittest.main()
