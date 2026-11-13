# Audit 2022 native Mac source-cache publication

Original `44be244bc42ffa1f8a1f55349d5dc6c9da4d4d54` kept the materialization root writable until rename, protected the final root, and retained cleanup ownership and the cache lock through that transition. Its report attributed the first-start failure to macOS rename returning EACCES.

`test-g1-cache-publication.py` runs in the existing macOS 15 CLI/device job as an ordinary user. It adds no runner, mounted volume, app or system service. The Darwin-only test is behind `g1_cache_lifecycle` and the runner supplies a disposable Go overlay with two observation calls around the real rename and failed-publication cleanup. No production source changes or filesystem-call mocks are involved. All production source writes, chmods, flock operations, rename, verification and cleanup run against fresh private directories.

The successful journey materializes the full actual embedded source bundle. An actual create/unlink operation proves the staging root is writable immediately before publication; opening each payload for writing must fail. After the real rename, the parent process must fail to acquire the child's kernel flock while the root is still 0700. After protection and child exit, it acquires the lock, requires root 0555, and verifies reuse preserves the same inode.

The failure journey uses a small flat source tree and a native per-process `sandbox-exec` policy that denies only `file-write-mode` on its future owned published directory. The real final chmod must return a permission error after rename. A second barrier observes the still-present failed tree and requires the lock to remain held during cleanup. Production must remove its failed publication, retain a foreign staging tree and its exact marker bytes, then recover through an unsandboxed retry and reuse without replacement. The flat tree isolates cleanup ownership from a separate persistent policy that would also deny every later root chmod; it is not presented as a full embedded-payload failure under that policy.

Every child is held by its original process handle and joined. On unknown drain, its private directory is retained and the test fails; the outer process guardian owns final descendant termination. The sandbox policy affects only the child and changes no host sandbox profile, TCC setting, installed application or user cache.

Four source controls must execute the real test and fail its named assertion:

- Removing staging-root chmod makes the actual publication write probe fail.
- Dropping the post-rename cleanup assignment leaks the real failed publication.
- Omitting final protection leaves a writable published root.
- Releasing the cache lock early lets the parent process acquire it before protection.

Compilation errors, skips, races, unrelated failures and timeouts cannot satisfy a control. The required gate runs with the race detector. Portable evidence-admission tests do not substitute for these native operations.

## Historical rename sensitivity

On the observed macOS 27.0.1 host, renaming a 0555 directory within the same parent succeeds; moving it between parents returns permission denied. Production stages and publishes under the same `g1` parent. Using the cross-parent failure as the original startup reproduction would therefore be misleading.

The gate separately records native same-parent/cross-parent behavior and runs the actual original publication sequence, without observers, through `TestMaterializeIdentity`. That observation is classified as either a real same-parent rename permission failure or compatibility on the tested OS. The local original-sequence run passes, so `same_parent_rename_EACCES_reproduced` is false. Its success is not a passing negative control. The writable-root control proves the intended publication contract, not that the local kernel requires it for this rename.

The native current lifecycle and all four controls passed locally; the existing macOS 15 job will provide independent native evidence and its own historical-sensitivity classification. No simulator boot, model download, application deployment or physical robot accuracy is inferred from source-cache acceptance.

```sh
python3 .github/scripts/test-g1-cache-publication.py --output /absolute/new/evidence
```
