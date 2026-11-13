# Contributing to WendyOS

Thank you for contributing to WendyOS! This document covers the development workflows, CI pipelines, and guidelines for contributing to the project.

## CLI design

Use the [Command Line Interface Guidelines](https://clig.dev/) as the baseline
standard for Wendy CLI design. Apply them with judgment and preserve
established behavior unless a change is intentional. Document Wendy-specific
decisions and exceptions in the [executable E2E reference](../../../../../../swift/WendyE2ETests/README.md#reference-directory),
a behavioral reference backed by executable assertions. Full-screen TUI design
requires separate guidance.

For questions about initialization, [generators](https://guides.rubyonrails.org/generators.html),
and file conflicts, use concrete references such as the [Ruby on Rails CLI](https://guides.rubyonrails.org/command_line.html)
for inspiration, applying judgment while Wendy's executable specs remain authoritative.

The following agreed design conventions apply to new work and intentional
redesigns. Existing commands may not yet comply; preserve established interfaces
until deliberate compatibility changes.

- A TUI is optional convenience. All capabilities must be available without
  Bubble Tea through the ordinary CLI, arguments/options, and plain prompts
  where appropriate.
- Use one Rails-style file-generation policy: create missing files, leave
  identical files untouched, explicitly resolve differing files, and preserve
  unrelated files. Interactive conflicts must offer overwrite, skip, diff, and
  quit.
- Explicit `--force` overwrites conflicts; `--skip` preserves conflicts while
  still creating missing files. Reject `--force` together with `--skip`.
- Outside `--dry-run`, non-interactive unresolved conflicts must fail with
  actionable paths. Absent input or EOF must never grant overwrite permission.
- Use `--dry-run`, not `--dry` or `--pretend`, to preview actions and conflicts
  without prompting or writing.
- Preflight unresolved conflicts before writing. This does not promise rollback
  of later I/O failures.

## CI / GitHub Actions

### Source-only AI review

The security, API, docs and integration-test reviews validate the complete diff before excluding generated protocol output. The fixed policy in `.github/scripts/review_sources.py` covers Go protobuf output, the three Swift core Proto output directories, WendyNotify's generated notification stubs and ThermalNotify's four named outputs: the `.pb.swift` and `.grpc.swift` files for `wendy_system_v1_notifications` and `wendy_agent_services_v1_wendy_agent_v1_video_service` directly under `Examples/ThermalNotify/Sources/ThermalNotify/Generated/`. This is a path and suffix policy. It does not confirm that a file came from a generator, and most excluded outputs have no regeneration check, so a hand-written file at one of these paths is not AI-reviewed. Unknown source files, cross-boundary renames, source definitions, generators, external source pins, configuration and generation/build checks remain reviewed. Generated headers and PR attributes do not define exclusions.

Coverage distinguishes source files and bytes from excluded output. Upstream source bodies are not fetched: a pin change alone does not establish upstream API or behavior compatibility. Generation checks, where they exist, are independent and do not substitute for source review.

Generated-only changes make no model call. Security preserves prior findings and enforces earlier blockers. API review records `skipped` and preserves prior decisions, acceptance and risk labels without publishing a replacement checklist. Docs and integration-test review preserve prior comments and still fail for an existing blocker. A blocker kept this way clears when a later push with a non-generated change gets a normal review, or when a maintainer deletes the bot comment after triage. API and docs review run this policy from current `main`. Security and integration-test review run it from the PR's base revision, so a PR whose base predates the policy keeps the earlier behavior until it merges `main`. Swift E2E analysis has a separate review path without this source filter; its repository and tool reads remain unrestricted.

GitHub-managed Copilot uses `.github/copilot-instructions.md` as prompt guidance, not an enforced input filter. [Administrator content exclusions](https://docs.github.com/en/copilot/concepts/security-governance-and-network-settings/content-exclusion) can cover these output paths on Copilot Business/Enterprise; their configuration and instruction adoption are unverified. [Provider defaults](https://docs.github.com/en/copilot/reference/review-excluded-files) also exclude some dependency/configuration files. No Copilot settings or review requests are changed here.

### Integration Test Review (`.github/workflows/integration-test-review.yml`)

The diff is read from immutable base/head Git commits after checking PR metadata. Existing test context is read as data from the event head. The model retains its existing 30,000-character diff excerpt budget. Larger source diffs are explicitly reported as partial input; omitted source remains unreviewed, and an excerpt cannot clear earlier blockers. A partial run publishes its new findings beneath an earlier blocker, and the check stays red until the source diff shrinks under the 30,000-character limit so a full review runs, or a maintainer deletes the bot comment after triage. Generated output is filtered before the diff, file lists or context are sent to the model.

### Docs Review Workflow (`.github/workflows/docs-review.yml`)

The docs-review workflow runs on every pull request targeting `main` and posts a structured docs coverage review as a PR issue comment when Claude finds documentation gaps.

**Key behaviours:**

- **Structured output:** Claude returns either `NO_CHANGES_NEEDED` or JSON with up to 8 findings. Each finding includes severity, title, docs path, one-line overview, and detailed rationale or suggested content.
- **Severity sorting:** Findings are sorted blockers-first, then concerns, then info, with 🛑 Error, ⚠️ Concern, and 💡 Info labels in the comment.
- **Inline path summaries:** Each visible finding paragraph starts with the affected docs path, followed by a colon and the overview prose.
- **Collapsible details:** Longer rationale and suggested diffs are placed behind a workflow-owned `<details>` disclosure block.
- **Output safety:** LLM-provided text is length-limited, angle brackets are rendered as safe Unicode characters, detail text is placed in fenced code blocks, unsafe URI schemes are neutralised, and docs paths are accepted only when they match the strict existing `docs/.../*.md` allowlist.
- **Edit-in-place comment:** Existing docs-review comments are identified by the `<!-- ai-docs-review:v1 -->` marker and edited instead of posting duplicates. Duplicate marker comments are deleted.
- **Stale comment cleanup:** A completed source review with no findings removes the previous comment. A generated-only skip preserves it and enforces any prior blocker.
- **Body truncation:** The prepared comment is capped at 65,000 characters, with space reserved so the identifying marker remains present after truncation.
- **GitHub API retries:** Comment list, create, update, and delete requests retry transient `429` and `5xx` responses before failing the step.

### Security Review Workflow (`.github/workflows/security-review.yml`)

A dedicated **AI Security Review** workflow runs on every pull request targeting `main` and posts a structured security and compliance report as a PR comment.

#### Trigger

```yaml
on:
  pull_request:
    types: [opened, synchronize, reopened]
    branches: [main]
```

Concurrent runs for the same PR are cancelled automatically (`cancel-in-progress: true`).

#### What it does

1. **Reads the complete PR diff** from immutable Git commits, validates it against PR metadata and prepares bounded source-only batches.
2. **Fetches prior review state** from the existing security-review comment identified by the `<!-- ai-security-review:v1 -->` marker.
3. **Invokes Claude for source changes** (`CLAUDE_MODEL`, currently `claude-opus-4-8`, `max_tokens=16000`) with a detailed system prompt covering security, compliance, prior review state, and explicit `SECURITY:` silencing comments.
4. **Renders a structured Markdown comment** from Claude's JSON response: one glanceable line per finding, with full details behind per-finding `<details>` disclosures.
5. **Posts or updates a PR comment** headed `# AI Security Review`. Existing comments are edited in place via the stable marker; duplicate security-review comments are deleted.
6. **Blocks merging only on open high-severity findings** — a final step exits non-zero only when an unsilenced `open` finding is `HIGH` or `CRITICAL`.

#### Security analysis scope

The model analyses the diff for:

- Injection vulnerabilities (SQL, command, LDAP, XPath, template, etc.)
- Authentication and authorisation flaws
- Sensitive data exposure (secrets, PII, tokens hardcoded or logged)
- Cryptographic weaknesses
- Input validation and output encoding issues
- Security misconfiguration (overly broad permissions, debug flags, unsafe defaults)
- Insecure dependencies or version pins
- Race conditions and TOCTOU vulnerabilities
- Path traversal and SSRF
- Denial-of-service vectors (unbounded loops, large allocations, missing rate limits)

#### Compliance frameworks checked

| Framework | Controls evaluated |
|---|---|
| **SOC 2** | CC6, CC7, CC8, CC9, A1, C1, P-series |
| **ISO/IEC 27001:2022** | A.8 (technological controls), A.9 (access control), A.12 (logging & monitoring) |
| **PCI DSS v4.0** | Req 3, 4, 6, 10 — flagged only when the diff touches payment/card data |
| **GDPR / Privacy** | Lawful basis, PII logging, data-subject rights |
| **HIPAA** | PHI encryption, access controls — flagged only when the diff touches health data |
| **NIST SP 800-53 / CSF 2.0** | AC, AU, IA, SC, SI control families |

Each finding is rated **CRITICAL**, **HIGH**, **MEDIUM**, **LOW**, or **INFORMATIONAL** and tagged with the relevant standard(s) (e.g. `[SOC2-CC6] [ISO27001-A.8]`).

#### Report structure

The comment is rendered from a JSON payload with:

| Field | Description |
|---|---|
| `summary` | One-line overview of the review |
| `findings[]` | Up to 10 security findings |
| `compliance_summary` | Short Markdown compliance summary |

Each finding includes severity, status, standards, title, path, line(s), visible overview, and detailed Markdown remediation. Valid statuses are `open`, `addressed`, `cancelled`, and `silenced`.

#### Stateful review and silencing

The previous AI Security Review comment is included as untrusted prior review state. Claude preserves still-relevant findings, marks fixed findings as `addressed`, marks no-longer-applicable findings as `cancelled`, and appends genuinely new findings as `open`.

A nearby developer comment containing `SECURITY: <reason>` explicitly accepts the risk. The renderer also checks the PR diff for nearby `SECURITY:` comments and marks matching open findings as `silenced`, keeping them visible but non-blocking.

#### Severity gate

The gate counts only findings whose status remains `open` and whose severity is `HIGH` or `CRITICAL`. Silenced, addressed, and cancelled findings do not fail the check. `MEDIUM`, `LOW`, and `INFORMATIONAL` findings are reported in the comment but do not fail the check.

#### Permissions

The job requests only `contents: read` and `pull-requests: write`. It only runs on PRs from within the same repository (`github.event.pull_request.head.repo.full_name == github.repository`), preventing untrusted forks from triggering the workflow.

#### Prompt injection mitigation

PR title, body, and diff are wrapped in `<untrusted_pr_content>` tags. Prior review state is wrapped in `<untrusted_previous_review>` tags, and malformed model output sent through the repair pass is wrapped in `<untrusted_model_response>` tags. The prompts explicitly instruct the model to ignore instructions embedded within those untrusted sections.

#### Required secrets

| Secret | Purpose |
|---|---|
| `ANTHROPIC_API_KEY` | Authenticates calls to the Anthropic API |

No additional secrets are required; the `GH_TOKEN` is provided automatically by GitHub Actions.
