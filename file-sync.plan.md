# Wendy File Sync

Adds a dedicated rsync-style file sync gRPC protocol for deploying files to
native macOS devices. Replaces the OCI image packaging used for the darwin
deploy path with a simpler, purpose-built mechanism. Scoped to darwin targets
only; Linux support is deferred to future work.

## Background

The macOS prototype (PR #399) repurposed the `WriteLayer` RPC to transfer native
app binaries by packaging them as OCI images (tar + gzip + manifest + config
blobs), then parsing and extracting them on the agent side. This was a pragmatic
shortcut but is the wrong abstraction: there is no container runtime involved,
and OCI's layered image model adds complexity (tar extraction, content-addressed
blob store, manifest parsing) that buys nothing for the darwin native path.

A dedicated file sync protocol is simpler, transfers only what changed, and
also serves as the natural vehicle for syncing large supplementary files
(model weights, datasets, config bundles) to a darwin device alongside the
app binary — something the Linux path currently handles by baking assets into
the container image.

## Key Design Decisions

- **One-way only**: CLI → agent. No reverse sync.
- **Per-app working directory**: each app gets its own isolated directory on
  the device. All synced paths are relative to it.
- **Content-addressed by SHA256**: only files that are missing or have changed
  are transferred. First deploy is a full upload; subsequent deploys send only
  what changed — typically just the rebuilt binary.
- **`remotePath` defaults to `localPath`**: the common case requires only
  `localPath`. An explicit `remotePath` overrides the destination.
- **Manifest-driven convergence**: files present on the device but absent from
  the CLI's declared set are deleted after each sync. The working directory
  always converges to exactly what the CLI declared.
- **Temp-file + atomic rename**: the live file is never in a partially-written
  state. A corrupt or interrupted transfer leaves no debris.

## Development Approach

Each iteration starts with failing tests that pin down a specific behaviour,
then adds the minimum implementation to make them pass. The sequence is ordered
so every iteration leaves the system demonstrably working and the arc reads as
a story of capability accumulating.

Test style throughout: plain `testing.T`, behaviour-descriptive names
(`TestSyncFiles_CorruptCommit_NoPartialFile` not `TestSyncFilesError`),
table-driven subtests for exhaustive validation cases, bufconn in-process gRPC
servers for service-level tests. No BDD framework.

---

### Iteration 1 — `wendy.json` can declare files

*Touches: `appconfig.go`, `appconfig_test.go`*

Write tests for round-trip JSON of the `files` field: an entry with both
`localPath` and `remotePath`; an entry with only `localPath` (remote defaults
to local); all six validation error cases (empty / absolute / dotdot for each
of `localPath` and `remotePath` when given); existing `wendy.json` files
without a `files` key. All fail. Add `FileSync` and extend `Validate()`. Tests
pass. Nothing acts on files yet — they are declared but inert.

---

### Iteration 2 — The agent can inventory what it already has

*Touches: `swift/Sources/WendyAgent/Services/FileSyncService.swift` (new)*

Write tests for `buildManifest`: empty directory returns empty array; a single
file produces the correct relative path, size, SHA256, and mode; nested
directories produce correct relative paths; `*.tmp` files are excluded. All
fail. Implement `buildManifest` as a standalone function. Tests pass. Nothing
calls it yet.

---

### Iteration 3 — The agent answers "what do you have?"

*Touches: `swift/Sources/WendyAgent/Services/FileSyncService.swift`*

Write tests for the `SyncStart` → `SyncManifest` handshake: an empty working
directory returns an empty `SyncManifest`; a pre-seeded directory returns the
correct entries. All fail. Stand up `FileSyncService` with this first
exchange. Tests pass. The CLI can now ask the device what it has.

---

### Iteration 4 — The agent accepts and stores a new file

*Touches: `swift/Sources/WendyAgent/Services/FileSyncService.swift`*

Write tests for the full happy path: `SyncStart` + `FileChunk`(s) +
`FileCommit` → `FileAck`, file present at the correct path with correct
content and mode; nested paths work (parent dirs created automatically). All
fail. Implement `FileChunk` and `FileCommit` handling with temp-file + atomic
rename. Tests pass.

---

### Iteration 5 — The agent rejects corrupt transfers

*Touches: `swift/Sources/WendyAgent/Services/FileSyncService.swift`*

Write tests: a `FileCommit` with a wrong SHA256 returns an error, no file
appears at the destination, no `.tmp` remains. Fail. Add SHA256 verification
on commit with temp-file cleanup on mismatch. Pass.

---

### Iteration 6 — The agent removes files no longer declared

*Touches: `swift/Sources/WendyAgent/Services/FileSyncService.swift`*

Write a test: pre-seed the working directory with `old.bin`; send a
`SyncStart` whose manifest omits it; after stream EOF the file is gone and
`SyncComplete` is received. Fail. Implement the deletion pass on stream close.
Pass.

---

### Iteration 7 — The CLI knows what it has and what needs transferring

*Touches: `filesync.go`, `filesync_test.go` (both new, in `cli/commands`)*

Write tests for `buildLocalManifest` (empty dir, single file, nested files,
correct SHA256/size/mode) and `diffManifests` (identical → empty; missing from
remote → included; SHA256 differs → included; extra remote-only file → not
included in transfer list). All fail. Implement both functions. Pass.

---

### Iteration 8 — The CLI drives a complete sync session

*Touches: `filesync.go`, `filesync_test.go`*

Write tests for `syncFiles` using a fake `WendyFileSyncServiceClient`:
all diffed files are transferred; an unchanged file is not re-sent; a file
that changes mid-transfer (streaming hash ≠ manifest hash) causes an
immediate error with no `FileCommit` sent. All fail. Implement `syncFiles`.
Pass.

---

### Iteration 9 — Progress is visible during transfer

*Touches: `filesync.go`, `filesync_test.go`*

Write tests: progress callback receives the correct total byte count, per-file
sizes, and a file-counter increment on each `FileAck`; no output when the diff
is empty. Fail. Add progress tracking. Pass.

---

### Iteration 10 — `wendy run` on macOS uses file sync instead of OCI

*Touches: `run.go`, `commands_test.go`*

Write tests: the macOS deploy path calls `syncFiles` with the binary always
included plus any entries from `appCfg.Files`; `CreateContainer` receives the
binary name via `cmd` rather than a manifest digest; the old OCI packaging
code path is unreachable. Fail. Replace `runMacOSWithAgent` to use
`syncFiles`; simplify `CreateContainer` for darwin (no OCI parsing, no blob
store — just verify the binary is present in the working directory). Remove
`oci.go`. Pass.

---

## Parts

---

## Part 1 — Add `files` to `AppConfig`

**Goal:** Let `wendy.json` declare a list of local paths to sync to the
device's app working directory and where they should land within it.

### Schema

**File:** `go/internal/shared/appconfig/appconfig.go`

Add:

```go
// FileSync describes a file or directory to sync to the device's app working
// directory before the app starts. LocalPath is relative to wendy.json.
// RemotePath is the destination relative to the app working directory; it
// defaults to LocalPath when omitted.
type FileSync struct {
    LocalPath  string `json:"localPath"`
    RemotePath string `json:"remotePath,omitempty"`
}
```

Add `Files []FileSync `json:"files,omitempty"`` to `AppConfig`.

### Effective remote path

When `RemotePath` is empty the effective destination equals `LocalPath` with
any leading `./` stripped. No additional normalisation — the value is used
verbatim as a relative path under the app working directory.

### Validation

Add to `Validate()`:

- `localPath` must not be empty.
- `localPath` must not be absolute (must not start with `/`).
- `localPath` must not contain `..` path components.
- `remotePath`, if non-empty, must not be absolute.
- `remotePath`, if non-empty, must not contain `..` path components.

### Example `wendy.json`

```json
{
  "appId": "sh.wendy.examples.MyApp",
  "version": "1.0.0",
  "language": "swift",
  "files": [
    { "localPath": "models/gemma-3-27b" },
    { "localPath": "config/prod.json", "remotePath": "config/app.json" }
  ]
}
```

### Acceptance criteria

- `appconfig_test.go` covers: valid entry with both paths; valid entry with
  `localPath` only; each validation error case; round-trip JSON serialisation;
  existing `wendy.json` files without `files` parse without error.

---

## Part 2 — `SyncFiles` RPC: proto + agent implementation

**Goal:** Define the wire protocol for file sync and implement the agent-side
handler. No CLI integration yet.

### Proto

**File:** `Proto/wendy/agent/services/v1/wendy_agent_v1_file_sync_service.proto` (new)

File sync is its own domain — file transfer and reconciliation, independent of
container lifecycle — so it gets a dedicated service.

```protobuf
syntax = "proto3";

package wendy.agent.services.v1;

service WendyFileSyncService {
  rpc SyncFiles(stream SyncFilesRequest) returns (stream SyncFilesResponse);
}

// FileEntry describes a single file in a manifest.
message FileEntry {
  string path   = 1; // relative to app working directory
  int64  size   = 2;
  string sha256 = 3;
  uint32 mode   = 4; // unix file permissions (e.g. 0755)
}

message SyncFilesRequest {
  oneof request_type {
    SyncStart  start  = 1;
    FileChunk  chunk  = 2;
    FileCommit commit = 3;
  }
}

// SyncStart opens a sync session for the given app. The CLI sends its
// local manifest so the agent can respond with what it already has.
message SyncStart {
  string             app_id   = 1;
  repeated FileEntry manifest = 2;
}

// FileChunk carries a slice of a file being transferred.
message FileChunk {
  string path = 1; // relative to app working directory
  bytes  data = 2;
}

// FileCommit signals the end of a single file transfer.
message FileCommit {
  string path   = 1;
  string sha256 = 2;
  int64  size   = 3;
}

message SyncFilesResponse {
  oneof response_type {
    SyncManifest manifest  = 1;
    FileAck      ack       = 2;
    SyncComplete complete  = 3;
  }
}

// SyncManifest is the agent's reply to SyncStart.
message SyncManifest {
  repeated FileEntry files = 1;
}

// FileAck confirms a file was written successfully.
message FileAck {
  string path = 1;
}

message SyncComplete {}
```

Regenerate Go and Swift bindings after adding the file.

### Wiring

**`go/internal/cli/grpcclient/client.go`** — add to `AgentConnection` and
`newAgentConnection`:

```go
FileSyncService agentpb.WendyFileSyncServiceClient
// ...
FileSyncService: agentpb.NewWendyFileSyncServiceClient(conn),
```

### Agent implementation

**File:** `swift/Sources/WendyAgent/Services/FileSyncService.swift` (new)

The server-side implementation lives entirely in the Swift macOS agent.
`FileSyncService` is an `actor` conforming to
`Wendy_Agent_Services_V1_WendyFileSyncService.ServiceProtocol`, constructed
with a `filesBase` path (default:
`~/Library/Application Support/wendy-agent/files`).

Working directory root: `<filesBase>/<appId>`

**`SyncFiles`** handler:

**`SyncStart`** — resolve `workDir = <filesBase>/<appId>`. Walk it (if it
exists) with `FileManager`, skipping `*.tmp` files, and produce a
`SyncManifest`. Compute SHA256 by streaming each file in 64 KiB reads — no
full-file buffering. Send the `SyncManifest` response.

**`FileChunk`** — write the chunk to `<workDir>/<path>.tmp`, creating parent
directories as needed.

**`FileCommit`** — flush the temp file, verify SHA256 and size, set file
permissions from the manifest entry's `mode`, atomic rename to
`<workDir>/<path>`. Send `FileAck`. On error, remove the temp file before
returning.

**Stream EOF** — collect `SyncStart` manifest paths into a set; delete any
files present in the agent's opening manifest but absent from that set. Send
`SyncComplete`.

Extract `buildManifest(at:) -> [FileEntry]` as a standalone helper — it is
also exercised directly in unit tests.

### Acceptance criteria

- `SyncStart` against empty dir → empty `SyncManifest`.
- `SyncStart` + chunks + commit for a small binary → file at correct path with
  correct content and mode `0755`.
- `SyncStart` manifest omitting a pre-existing file → file deleted after EOF.
- `FileCommit` with wrong SHA256 → error, no file at destination, no `.tmp`.
- Interrupted stream (closed mid-chunks) → live file unchanged, no `.tmp`
  after next successful sync.

---

## Part 3 — CLI integration and macOS deploy path replacement

**Goal:** Drive `SyncFiles` from the CLI, wire it into `wendy run`, and replace
the OCI image packaging used by the macOS native deploy path.

### Manifest building — CLI side

**File:** `go/internal/cli/commands/filesync.go` (new)

`buildLocalManifest(root string) ([]agentpb.FileEntry, error)`:

- `fs.WalkDir` over `root`.
- For each regular file: stream through `sha256.New()` in 64 KiB reads, record
  path (relative to root), size, SHA256 hex, and `os.FileMode` cast to
  `uint32`.
- Return the slice.

### Diff computation

`diffManifests(local, remote []agentpb.FileEntry) []string`:

- Build a map of remote entries by path.
- Return paths of local files missing from remote or whose SHA256 differs.
  The agent handles deletions; the CLI logs them at the end.

### `syncFiles` function

```go
func syncFiles(
    ctx context.Context,
    conn *grpcclient.AgentConnection,
    appID string,
    entries []fileSyncEntry, // {localRoot, remotePath}
) error
```

Where `fileSyncEntry` is a CLI-internal type pairing a resolved absolute local
root with the effective remote path. Constructed from `appCfg.Files` by
`run.go`.

Protocol:

1. Build the local manifest by walking each `localRoot`, prefixing each file's
   path with its `remotePath` to produce agent-relative paths.
2. Open a `SyncFiles` bidi stream.
3. Send `SyncStart{AppId: appID, Manifest: localManifest}`.
4. Receive `SyncManifest` from the agent.
5. Compute diff. If empty, close stream and return — print "Files up to date."
6. For each file to transfer: open it, stream in 256 KiB `FileChunk` messages,
   computing SHA256 on-the-fly. After the last chunk, compare the on-the-fly
   hash against the manifest entry hash. If they differ, the file changed
   during transfer — return an error without sending `FileCommit`. If they
   match, send `FileCommit` and wait for `FileAck`.
7. Send stream EOF. Agent prunes stale files and sends `SyncComplete`.

Two verification layers: the CLI catches mid-transfer mutations (manifest hash
vs. streaming hash); the agent catches transit corruption (committed hash vs.
received data hash).

### Progress

Total bytes and file count are known before the first byte is sent (from the
diff). Display:

- Current file: name, bytes sent / file size, percentage.
- Overall: aggregate bytes sent / total bytes, percentage.
- File counter: "file N of M" updated on each `FileAck`.

Show nothing when the diff is empty.

### macOS deploy path replacement

**File:** `go/internal/cli/commands/run.go`

Replace `runMacOSWithAgent` with a version that:

1. Builds the Swift binary (unchanged).
2. Assembles `entries` for `syncFiles`:
   - Always includes the binary itself (implicit: `localRoot` = build output
     dir, `remotePath` = product name).
   - Appends entries from `appCfg.Files`, resolving `localRoot` relative to
     `cwd` and applying the remote-defaults-to-local rule.
3. Calls `syncFiles`.
4. Calls `CreateContainer` with `Cmd` set to the product name (binary name)
   and `AppName` set to the app ID. No manifest digest. No OCI.

**Remove `go/internal/cli/commands/oci.go`** — it exists solely for the OCI
packaging path that this part replaces.

On the agent side (Swift macOS agent, implemented in `kb.wendy-for-mac`),
`CreateContainer` for darwin verifies the binary exists in
`<filesBase>/<appId>/` and registers the app. No OCI parsing, no tar
extraction, no blob store.

### Acceptance criteria

- macOS deploy: `wendy run` builds the binary, calls `syncFiles`, calls
  `CreateContainer` with `cmd = product`. No OCI code path reached.
- Second `wendy run` with no source changes: binary already on device with
  matching SHA256 → nothing transferred, "Files up to date" printed.
- A declared file removed locally: deleted from device on next run.
- A file mutated during transfer: `wendy run` fails with a clear error naming
  the file. No partial file on device.
- Project with no `files`: behaviour identical to today.
- Progress shown during transfer; absent when nothing to sync.

---

## Future Work

### Linux support

The `files` field in `wendy.json` and the `SyncFiles` RPC are intentionally
platform-agnostic. Extending support to Linux requires two additions:

**Go agent implementation** — a `FileSyncService` in
`go/internal/agent/services/file_sync_service.go` mirroring the Swift agent
behaviour, registered in `go/cmd/wendy-agent/main.go`. Storage root:
`/var/lib/wendy/files`.

**Bind-mount into containers** — an `ApplyFiles` helper in
`go/internal/agent/oci/spec.go` that, for each `files` entry, adds a
read-only bind mount:

```
host:  /var/lib/wendy/files/<appId>/<effectiveRemotePath>
guest: /wendy/files/<effectiveRemotePath>
```

and injects `WENDY_FILES_DIR=/wendy/files` into the container environment.
Called from `CreateContainerWithProgress` after `ApplyEntitlements`.

**CLI wiring** — `syncFiles` called from `runWithAgent` (the Linux path)
before container creation, guarded by `len(appCfg.Files) > 0`.

### Content-addressed blob store

The current implementation follows rsync semantics: files are identified by
path and SHA256, transferred one at a time via temp + rename. A content-
addressed blob store (similar to OCI layers or git objects) would allow blobs
to be indexed by SHA256 and shared across all apps on the device.

Real-world model shards are typically 5+ GB. At that scale:

- **Deduplication**: a shard shared across model versions or apps is stored
  and transferred once.
- **Move detection**: moving a file between subdirectories costs no transfer.
- **Resumability**: an interrupted transfer resumes from the last completed
  blob rather than restarting the file.
- **GC via nlink**: hard links from the blob store to app working directories
  let the OS track reference counts for free; blobs with `nlink == 1` are safe
  to delete without walking manifests.

This requires all file storage to reside on a single filesystem (hard-link
constraint). exFAT external drives — common for large model storage — do not
support hard links, so a fallback path would be needed.
