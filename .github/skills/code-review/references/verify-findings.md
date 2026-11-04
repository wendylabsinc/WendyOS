# Verify a finding before publishing it

## Prove the execution path

Resolve imports in the actual module, image, platform build tags and generated tree.
Two examples can have different ros_app.py files with different function signatures.
Look for transformations outside the diff: MDX preparation may rewrite URLs, server
interceptors may authorize a handler, or a validated flag may never reach its provider.

For language or library claims, check the declared version, local API documentation
or a minimal reproduction. Past false positives included:

- Go permits append(byteSlice, stringValue...) without an explicit []byte conversion.
- os.CopyFS can create its destination or use an existing directory. Existing files
  are a different case; do not confuse the two conditions.
- Python array.array("I", bytesValue) groups bytes into words. Check item size and
  byte order separately when they matter to the protocol.
- Go range-variable address semantics depend on the language version and whether
  the variable is declared in the loop or reused by assignment. Do not apply an old
  shared-variable warning without checking that distinction.

These are counterexamples to blanket rules, not exemptions from examining surrounding
code. The [case studies](case-studies.md) link the review evidence.

## Compare the behavior that matters

Choose checks that could disprove the implementation's claim:

| Claim | Useful countercheck |
| --- | --- |
| A CLI option selects a backend | Trace affected providers, caches and fast paths. |
| Enrollment uses a new identity | Follow it through pins, connection, renewal and removal on affected transports. |
| An operation is transactional | Fail after a persistent write and inspect authoritative state and recovery. |
| A worker stops | Block I/O or cancel during startup, then verify resource release. |
| Data is ready | Observe the durable record or expected app response, not an earlier status. |
| A regression is fixed | Where practical, show the same focused case fails on base and passes on head. |

Tests that assert rewritten messages can endorse lost information. Identity tests
must assert the identity round-trip, not only call success. Mocked provider selection
does not prove GPU inference; a device node does not prove an entire hardware workflow.

Hardware evidence needs the board/firmware variant, tested path, setup latency and a
sampling window that overlaps the event. Distinguish absent observations from physical
limitations. Do not deploy or alter hardware unless the task already authorizes it.

## Separate defects from evidence gaps and policy choices

Use the causal chain: input or state, executed branch, violated contract, consequence.
If a missing fact breaks that chain, describe the unknown instead of inventing it.

For security, name the attacker-controlled input, bypassed check and authority or
resource reached. Read middleware before claiming missing auth. For concurrency,
name the interleaving and synchronization that permits it; a goroutine or unlocked
read in isolation is insufficient.

For compatibility, identify a supported version combination or saved artifact that
fails. A policy change can deliberately refuse an old path. Review acceptance,
migration and operator guidance instead of insisting every fallback survive forever.

An architecture preference needs a current decision or concrete operational cost to
become a blocker. An enhancement outside accepted scope stays optional.

## Re-review the evidence, not the labels

Review submissions, inline threads, top-level comments and CI checks are different
records. Formal CHANGES_REQUESTED differs from an advisory bot COMMENTED verdict.
Compare submission and merge times before claiming a review blocked a merge.

A fix commit title, green check, resolved thread or merge does not prove a finding
was corrected. An old objection does not stay valid after the relevant code changes.
Keep refutations and explicitly accepted tradeoffs visible.

Separate a reproduced baseline CI failure from a regression. Do not recommend
suppressing a check merely because it is unrelated. When automation decides failure
from one set of findings but renders a filtered set, verify that each failure still
has an actionable explanation.
