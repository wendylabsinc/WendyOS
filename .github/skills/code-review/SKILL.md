---
name: code-review
description: Review WendyOS pull requests and proposed changes for concrete defects, architectural fit, compatibility, and trustworthy validation. Use when reviewing code, reviewing a design, or assessing an existing review finding. Includes lessons from rejected PRs, corrected reviews, and recurring bugs and fixes.
---

# Wendy code review

Find changes that would break a supported workflow or undermine an established
contract. Verify the mechanism before recommending a fix. Historical review
decisions are evidence to investigate, not permanent prohibitions.

## Establish the change being reviewed

- Read applicable repository instructions and identify the base and head revisions.
  Preserve unrelated local edits. Distinguish a PR's own diff from inherited changes
  in a stack and compare findings with the latest head.
- State the intended user-visible behavior and the existing paths it affects.
  Trace the entry point through dispatch, configuration, transport, handler and
  persistence. New helpers and tests do not prove production callers use them.
- Check current source, relevant design decisions and linked dependencies. A PR
  description can be stale. When a dependency is unavailable, identify the missing
  contract rather than assuming its behavior.

## Review architectural fit

Read [architecture.md](references/architecture.md) when a change adds or alters a
service, RPC, entitlement, identity, transport, hardware owner, persistent state,
deployment flow or public contract. Select the relevant checks; do not manufacture
findings for every category.

For an architectural objection, establish:

1. Which component currently owns the behavior or resource.
2. What the proposed change duplicates, bypasses or makes inconsistent.
3. A supported workflow that would fail, or an accepted contract it contradicts.
4. The smallest viable correction, including migration or rollout dependencies.

If only a preference remains, label it as a design question. A new RPC, service or
dependency can be justified. Do not block a requested feature because an older PR
chose a different design, or demand unrelated features as a condition of approval.

## Check recurring failure patterns

For runtime changes, read the relevant sections of
[bug-patterns.md](references/bug-patterns.md). It connects past failures to their
causes, fixes and useful regression checks. Focus on lifecycle transitions,
persisted versus transient state, stream boundaries, identity across fallbacks,
partial failure and compatibility with older peers.

Use the historical case to form a hypothesis, then verify it against the current
change. Trace sibling paths that share the mechanism, such as native and container
restart, registry and chunked delivery, or live video and recorded export. A fix
in one path does not prove the others are correct. Check the stated limits of a
fix before recommending it elsewhere. Select checks that can expose the suspected
failure; do not demand every historical test for every PR.

## Verify candidate findings

Read [verify-findings.md](references/verify-findings.md) before finalizing findings.
For each candidate:

- Identify the trigger, exact code path and user impact. Check callers, middleware,
  generated inputs and build transforms that could invalidate the claim.
- Compare base and head. Separate introduced defects, pre-existing defects exposed
  by the new path, and unrelated baseline failures.
- Reproduce the smallest uncertain claim with the repository's language version,
  platform or focused test when feasible. A precise source trace can suffice;
  physical testing is needed only for claims that depend on hardware behavior.
- Look for a counterexample to your explanation. Read earlier replies before
  repeating a dismissed or corrected finding. Reopen it only with new evidence.

Use [case-studies.md](references/case-studies.md) for architectural rejections,
useful change requests, false positives and unresolved merge-time concerns. Follow
source links only when their details matter to the current review.

## Return actionable findings

Follow the caller's review format or machine-readable schema. Otherwise report
findings in severity order, with a narrow file/line or commit permalink, the trigger,
the resulting failure, supporting evidence and a minimal correction. Do not prescribe
a large rewrite when a smaller change preserves the intended behavior.

Distinguish these outcomes:

- **Blocking defect:** a demonstrated or strongly supported correctness, security,
  data-loss or required-contract problem. State the applicable conditions.
- **Design question:** ownership, scope or rollout intent needs a decision and the
  existing evidence does not establish a defect.
- **Validation gap:** a material claim lacks the evidence needed to assess it.
  Name the missing check and what result would settle the issue.
- **Optional improvement:** useful work that is not required for this change.

Use existing severity definitions when supplied. Do not turn an uncertain claim
into a blocker by assigning it a high severity. If no defensible defect remains,
say so and name material unverified behavior without inventing a finding.

For a re-review, give each previous finding an explicit disposition: fixed with
evidence, refuted with evidence, an explicitly accepted tradeoff, or still open.
A resolved thread or merged PR alone proves none of those. Note what checks you
actually performed and which revision they cover.

Review locally by default. This skill does not authorize posting GitHub reviews,
resolving threads, editing code, deploying to devices or changing repository policy.
