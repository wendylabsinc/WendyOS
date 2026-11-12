# CI security boundaries

Build and test jobs use read-only repository tokens. Only release and reporting
jobs receive the write permissions they need. Checkout does not persist tokens
in the changed build/test workflows. The release job retains authenticated Git
because it pushes release tags and Homebrew changes. Its Homebrew App token is
limited to that repository and contents/pull-request writes.

Swift lint runs PR code without a write token. A separate job posts its bounded
output as data, without checking out the PR. The E2E analyzer builds its review
scripts from the PR base commit and fetches the proposed commit only for a diff.
Artifact identifiers must be a single bounded path component.

Actions use full commit SHAs. Dependabot proposes updates. The Arch Linux release
container is pinned by digest. Version tags were resolved through their upstream
repositories; this change does not upgrade action major versions.

The required `Local: Ubuntu 24` check audits all workflows and composite actions
with zizmor 1.32.0, blocking high-severity, high-confidence findings. The Go test
aggregate also fails when this audit fails or is skipped. The CLI is used directly
because the vendor action does not expose the `--no-config` and `--no-ignores`
flags needed to prevent inline exclusions from suppressing this gate. Lower
confidence findings still require manual review. A passing scanner is not proof
that a workflow is safe.

Run the local checks with:

```sh
python3 .github/scripts/ci_security_test.py
python3 .github/scripts/swift_e2e_gate_test.py
uvx --from zizmor==1.32.0 zizmor --offline --no-config --no-ignores --min-severity high --min-confidence high .github
```

## Controls outside this repository

On 2026-11-12, all 85 non-archived public repositories in `wendylabsinc` were
verified to require approval for every external contributor's fork workflows.
The default-branch ruleset requires an independent review, stale-review dismissal,
code-owner approval, and the latest push to be approved. Its required check is
bound to the GitHub Actions App. These settings must remain enabled.

A same-repository writer can change a workflow on another branch and request
repository secrets. Main-branch review rules and read-only token defaults do not
prevent that. Signing, release, and cloud credentials need protected environments
or cloud-side trust conditions bound to approved refs and workflows. Never accept
an arbitrary branch's OIDC token merely because its repository name matches.

Persistent runners and caches require separate review. Restrict runner groups to
approved workflows and refs; do not share a writable cache or machine between
unreviewed PR code and releases. Workflow `if` conditions alone cannot enforce
this because a contributor can propose changes to those conditions.

Organization runner-group policy and audit-log access were unavailable to the
CLI token during this review. Cloud IAM, runner host isolation, and every
repository secret's scope have not been certified by these workflow checks.
