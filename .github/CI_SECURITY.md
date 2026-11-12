# CI security boundaries

Build and test jobs use read-only repository tokens. Only release and reporting
jobs receive the write permissions they need. Checkout does not persist tokens
in the changed build/test workflows. The release job retains authenticated Git
because it pushes release tags and Homebrew changes. Its Homebrew App token is
limited to that repository and contents/pull-request writes.

Swift lint runs PR code without a write token. A separate job posts its bounded
output as data, without checking out the PR. The E2E analyzer builds and runs its
tools from the PR base commit. It fetches the proposed commit only as data: the
review diff and the PR's E2E test sources, copied in as regular files, which the
report and AI review parse but never compile. A PR's script, Swift, or
dependency changes therefore never run beside the review credentials. This does
not stop a same-repository PR that edits the workflow itself (see below). The
base aggregator reads the PR's attempt artifacts, so a change to their format
must stay readable by the previous aggregator, or that PR's analysis fails until
it merges. Artifact identifiers must be a single bounded path component.

Actions use full commit SHAs, and Dependabot proposes their updates. The Arch
Linux release container is pinned by digest. Dependabot does not update
`docker://` references, so refresh that digest by hand. The AUR job runs a full
`pacman -Syu` so the aging image cannot fail as a partial upgrade. Version tags
were resolved through their upstream repositories; this change does not upgrade
action major versions.

The required `Local: Ubuntu 24` check audits all workflows and composite actions
with zizmor 1.32.0, blocking high-severity, high-confidence findings. It runs the
E2E suite even when the audit fails, then fails the job. The Go test aggregate
also fails when this audit fails or is skipped. Both gates run
`.github/scripts/workflow-audit.sh`, which installs zizmor by hash from
`workflow-audit-requirements.txt`; update the version and hashes together. The
CLI is used directly because the vendor action does not expose the `--no-config`
and `--no-ignores` flags needed to prevent inline exclusions from suppressing
this gate. Lower confidence findings still require manual review. A passing
scanner is not proof that a workflow is safe.

Run the local checks (Python 3.11 or later) with:

```sh
bash .github/scripts/workflow-audit.sh
python3 .github/scripts/swift_e2e_gate_test.py
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
