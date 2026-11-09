# Four English E2E specs

This experiment compiles four English behavior descriptions into stored Swift
tests in the existing Wendy E2E harness. Test execution runs the real CLI and
never calls a model. Regenerate a changed spec, or regenerate after investigating
test breakage. The three unchanged outputs remain byte-identical during the
cache maintenance experiment.

| English input | Existing comparison case | Invariant |
| --- | --- | --- |
| `cache-clear.md` | `WendyCacheClearTests.clears cached items and prints a summary` | Clear cache, report success, safely repeat. |
| `entitlement-add.md` | `WendyProjectEntitlementsAddTests.adds an entitlement to wendy.json` | Persist network entitlement and preserve app ID. |
| `entitlement-invalid.md` | `WendyProjectEntitlementsAddTests.validates the entitlement type before writing` | Reject the type without changing the project. |
| `device-default.md` | `WendyDeviceSetDefaultTests.preserves unrelated known configuration keys` | Persist the selection and retain three unrelated config values. |

## Reproduce generation

From `swift/WendyE2ETests`, the original v1 input revision
(`e569ae737`) uses this initial compilation command:

```sh
python3 EnglishSpecs/compile.py --codex /path/to/codex-gated \
  --run-dir .build/english-specs-initial-new
```

Compile only the maintained cache case with:

```sh
python3 EnglishSpecs/compile.py --cache-only --codex /path/to/codex-gated \
  --run-dir .build/english-specs-cache-new
```

Use your authorized Codex executable or gated wrapper. Each command requires a
new artifact directory and records the exact prompt, response, session, timing,
token usage and file hashes. Only the named English inputs and harness API
description enter the prompt. The compiler receives no original Swift test
implementations or product implementation code. A compiler tool call or reported
failure rejects the generation. Neither measured compilation called tools.

The driver stages the split response and runs `swift format` with the repository's
`.swift-format` before replacing stored Swift. Successful output includes the raw-response hash in
both provenance files, so it can be replayed immediately. Run the focused offline
driver checks with `python3 EnglishSpecs/test_compile.py -v`; they use a fake
compiler transport and the real Swift formatter, without model calls.

Generated Swift must never be hand-edited. Correct a spec or API prompt and regenerate when
validation exposes a problem. A fresh model invocation is stochastic; it does
not promise identical output bytes. An archived raw response can be replayed
without invoking a model:

```sh
python3 EnglishSpecs/compile.py --cache-only \
  --import-raw /path/to/recorded/raw-output.swift \
  --run-dir .build/english-specs-import-new
```

Replay requires the adjacent `measurement.json`, matching input/raw hashes and
explicit acceptance: completed, no tools, no failures, successful process. For
archives predating that state, adjacent `events.jsonl` must establish acceptance.
Omit `--cache-only` when replaying the original four-file
response at the corresponding input revision. Commit handwritten inputs and
generated output separately, and put the reproducing command in the generated
commit. `.gitattributes` marks the stored Swift as generated.

## Real execution and comparison

The measured CLI is pinned to
`e9be120f82140172e6ec5ac5477bc9bf2f8cf000`, built with Go 1.27.3. Tests run
with Apple Swift 6.4.2 on macOS arm64, using the same real CLI binary for the
original and generated groups. Each case receives an isolated home, temporary
directory and working directory. Authentication is disabled for these scenarios;
no agent, hardware, deployed device or personal credential is required.

The first four generated tests compiled without a correction round. Ten separate
executions of the original and v1 generated groups selected exactly four tests
each time:

| Version | Pinned CLI | Cache removal mutation |
| --- | --- | --- |
| Four original cases | 40/40 passed | All four passed. |
| Four generated v1 cases | 40/40 passed | All four passed. |
| Manually maintained cache comparator | 10/10 passed | Rejected with CLI permission-denied and cache-left-behind failures. |
| Generated maintained cache case | 10/10 passed | Rejected with the same permission-denied and cache-left-behind failures. |

A final run of all four generated cases also passed after removing the temporary
manual comparator from the product test tree.

The explicit mutation changes only `cache clear` from `removeCacheTree(cacheDir)`
to `os.RemoveAll(cacheDir)`, using a Go overlay. Product source remains untouched.
It recreates the removal behavior addressed by
[WendyOS #2449 — fix(cli): remove read-only extracted cache trees safely](https://github.com/wendylabsinc/WendyOS/pull/2449).
It is a mutation of the pinned CLI, not a complete historical build.

There are two assertion differences in v1. The English cache case checks actual
seeded-file deletion while the original checks exit/output only. The English
invalid-entitlement case compares exact file bytes while the original checks
parsed state. These stronger requirements cannot establish a benefit from the
English format. Both v1 groups miss the read-only regression because neither
creates a read-only descendant.

For maintenance, the cache spec adds a 0555 directory containing a 0444 file and
a cached symlink to an external 0555 directory with a 0444 file. Deletion must
remove the cache while leaving the target's bytes and modes unchanged after
both invocations. An equally strong AI-assisted Swift comparator includes the
same fixtures and checks. That comparator is retained only as local evidence.
The maintained generated case compiled without correction. Both maintained
cases verified fixture cleanup after passing and failing runs. The generated output replaces
only the cache file; the three other output hashes remain identical to v1.
Windows retains the original writable-cache behavior; the added permission and
symlink requirement is POSIX-specific.

## Assertions and failure explanations

Cache checks cover exit status, exact success output, empty stderr, on-disk
deletion and repeatability. The maintained case additionally checks fixture
preconditions and external-target preservation. The known-bad binary's real
permission-denied diagnostic and remaining cache explain its failure. Cleanup
restores only the known case-owned directories and propagates errors.

Entitlement addition checks the command's saved JSON boolean and independently
reads the saved project to verify its app ID and entitlement. Rejection checks
failure status, empty stdout, a diagnostic naming the invalid and valid types,
exact unchanged file bytes and parsed project state. Default-device selection
independently reads saved JSON and checks the selected address, analytics
preference, update timestamp and completion preference. Required JSON shapes
fail loudly before dependent assertions run. Scenario cleanup runs after failure;
command recordings remain available.

## Effort, cost and limits

Both blind compilations used `gpt-6.1-sol` at high reasoning:

| Compilation | Seconds | Input tokens | Cached input tokens | Output tokens | Reasoning output tokens |
| --- | ---: | ---: | ---: | ---: | ---: |
| Initial four cases | 95.518 | 22,100 | 12,288 | 4,560 | 1,552 |
| Maintained cache only | 76.494 | 21,366 | 12,288 | 3,288 | 1,552 |

Cached and reasoning counts are subsets of input and output respectively. These
are CLI-reported counts and include native harness context beyond the supplied
documents. The ephemeral session IDs and exact prompt/response hashes are in the
generated commits and retained experiment artifacts. There were two model
invocations, each successful on its first attempt.

Observed work, with physical line counts including comments and blank lines:

| Work | Observed steps and size |
| --- | --- |
| Initial compiler setup | Reused the existing harness unchanged; wrote 83 lines of harness/API instructions and a 108-line fixed-case driver. Formatting, provenance and generated Swift still required technical review. |
| Initial behavior authoring | Wrote four English specs totaling 112 lines / 630 whitespace-separated words. One blind compilation produced 345 lines of Swift, which were reviewed and run. |
| Cache maintenance setup | Added a 64-line cache-specific API/instruction input; extended the driver from 108 to 135 lines at the pre-review checkpoint for cache-only generation and validation. This setup work is additional to editing the behavior prose. |
| English cache maintenance | One behavior edit grew the spec from 26 lines / 160 words to 50 lines / 398 words. One cache-only compilation, zero corrective generation rounds; reviewed and validated the resulting 209-line Swift file. No generated source edits. |
| AI-assisted Swift maintenance | Adapted the original behavior into an 84-line local Swift comparator, then made one parity edit to include the original writable-file fixture. Formatted and validated it; zero compilation/test-failure repair rounds. No blind compiler invocation; interactive AI usage was not separately metered. |
| Compiler review repairs | Independent review reproduced three defects: failed formatting overwrote stored output, successful output lacked replay provenance, and replay bypassed tool-call rejection. The final driver is 161 lines, with 106 lines of offline regression checks. The initial two checks fail against the reviewed driver; all three pass after repair. No further model invocation or generated Swift edit. |

Both paths shared CLI builds, isolated execution, mutation construction and
recording checks. Compiler setup is distinct from per-spec authoring; these
counts and edit steps measure work products, not equivalent effort or time.
A non-developer can read the English setup, action, expected saved values and
cleanup without decoding Swift macros or callbacks. The permission modes,
symlink fixture and preservation requirements still need technical judgment.
Swift/API knowledge remained necessary to supply the harness guidance, quote
shell commands safely, review assertions and cleanup, and investigate failures.
Readability remains provisional: no non-developer reading study was performed.

Manual comparator maintenance is AI-assisted Swift work. Total authoring effort
and per-edit interactive-session tokens were not separately measured, so this
experiment supplies no human-minutes or authoring-speed comparison. No billing
data was measured; subscription marginal cost is unknown. Source prose, generated
Swift and actual failure logs are available for readability assessment, with no
claimed format-quality advantage.

Only macOS was executed. Linux and Windows runtime behavior remains unverified.
The initial zero-test filter attempts and a wrong-working-directory invocation
are retained as setup failures and excluded from every execution/pass count.
This four-case sample does not establish full-suite coverage, hardware behavior,
cross-platform reliability or a cost advantage. Detailed command recordings,
compiler evidence, the mutation patch/binary, comparison scripts and the manual
comparator live in the lane's `english-specs-20261109` evidence directory.

## Recommendation

Stored Swift compiled from English is viable for these four filesystem cases.
The maintenance result demonstrates the value of a read-only fixture and stronger
assertions equally in both formats. Keep this as a bounded authoring experiment
with reviewed generated output. This evidence does not justify a broader suite
migration or a claim of better correctness, authoring speed or cost.
