# Swift PR Review — File Sync

## FileSyncService.swift

**#1 — Buffering entire stream before responding** ✅ resolved
`syncFiles` collects all messages into `allMessages` before doing anything. The agent can't send its manifest until it has received all chunks. The whole point of bidirectional streaming is to let the client see the agent's manifest early and avoid sending unchanged files. This defeats that.

**#2 — Loading entire file into memory for verification** ✅ resolved
The commit handler uses `FileManager.default.contents(atPath:)` to load the whole file into RAM to compute SHA256. For large binaries this is a problem. The streaming hasher is already used correctly in `buildManifest`; keep the `SHA256` context open alongside the `FileHandle` while writing chunks instead.

**#3 — `FileHandle.write` errors silently dropped** ✅ resolved
The older `FileHandle.write(_:)` doesn't declare throws, but on failure it silently drops data. On macOS 10.15.4+ use `try fh.write(contentsOf:)` and propagate the error.

**#4 — `.tmp` suffix collision** ✅ resolved
Using `destURL.path + ".tmp"` is fragile — if the relative path already ends in `.tmp` (e.g. `foo.tmp`), it collides with the exclusion rule in `buildManifest`, and a future upload of that file will be silently excluded from manifests. Use a separate temp directory or a UUID-named scratch file instead.

**#5 — Path traversal not validated** ✅ resolved
`chunk.path` and `commit.path` from the client are appended to `workDir` without checking for `..` components. A malicious or buggy client could escape the app directory.

**#6 — Pruning uses the pre-session agent manifest** ✅ resolved
Stale-file deletion is based on `agentManifest` built before the session ran, not the actual post-session directory state. If a file was newly created by a bad chunk/commit sequence that was then cleaned up, or if there are pre-existing temp files that slipped through, the pruning set is wrong. Recompute or track what was actually written.

---

## ContainerService.swift

**#7 — `try! process.run()`** ✅ resolved
Force-try in a production gRPC handler. A missing binary or bad `executableURL` will crash the agent process entirely. Replace with `try process.run()` and propagate as an `RPCError`.

**#8 — `WriteLayer` accumulates the whole layer in memory**
Same pattern as #2 — large OCI layers will OOM the agent.

**#9 — `process.waitUntilExit()` blocks a Swift concurrency thread**
Synchronous call inside a structured concurrency task group. It parks a thread for the entire lifetime of the child process, starving the cooperative thread pool. Use `withCheckedContinuation` + `terminationHandler`, as is done correctly in `extractTarGz`.

---

## AgentService.swift

**#10 — Reachable RPCs use `fatalError` instead of `.unimplemented`**
`runContainer` and `updateAgent` are reachable RPCs — a client calling either will crash the agent. Stubs should throw `RPCError(code: .unimplemented, ...)` like the unimplemented methods in `ContainerService` do.

---

## WendyAgent.swift

**#11 — Duplicated apps base path**
`FileSyncService` and `ContainerService` each independently default to `~/Library/Application Support/wendy-agent/apps`. A single path constant should be shared between them; otherwise a misconfiguration silently splits the data.
