# Plan: in-flight file sync hashing

## Goal

Remove the Swift agent’s post-transfer disk reread/rehash in `swift/Sources/WendyAgent/Services/FileSyncService.swift` by verifying file integrity incrementally during chunk receipt.

## Protocol changes

File:
- `Proto/wendy/agent/services/v1/wendy_agent_v1_file_sync_service.proto`

### 1) Change all SHA256 fields from `string` to `bytes`
Keep the field name `sha256`, but change the type everywhere it appears:
- `FileSyncEntry.sha256`
- `FileSyncCommit.sha256`
- new mode-only request `sha256`
- new chunk checkpoint `sha256`

Rule:
- every SHA256 field must be exactly 32 bytes
- both CLI and agent reject any other length

### 2) Extend `FileSyncChunk`
Each chunk will carry cumulative post-chunk state:
- `path`
- `data`
- `sequence`
- `cumulative_size`
- `sha256`

Semantics:
- `sequence` starts at `0` and must be contiguous
- `cumulative_size` is total file bytes after applying this chunk
- `sha256` is the cumulative SHA256 after applying this chunk
- for empty files, send exactly one empty chunk:
  - `sequence = 0`
  - `cumulative_size = 0`
  - `sha256 = SHA256(empty)`

### 3) Add a mode-only request
Add a new request type for metadata-only updates, conceptually:
- `FileSyncSetMode { path, mode, size, sha256 }`

Semantics:
- used only when contents are unchanged and mode differs
- sent after all content transfers are complete
- agent reuses normal `FileSyncAck { path }`
- if target file is missing, fail the stream
- agent trusts session state / manifest; it does not reread + rehash on disk for this

### 4) Keep `FileSyncCommit`
Still required for:
- end-of-file marker
- final explicit assertion of `size + sha256`
- atomic rename trigger

### 5) Keep implicit stale-file pruning
No explicit delete request.
- CLI may print deletions based on manifest diff
- agent still prunes stale files after EOF

## Swift agent changes

File:
- `swift/Sources/WendyAgent/Services/FileSyncService.swift`

### 1) Replace ad hoc temp tracking with per-file transfer state
Replace:
- `temporaryHandles`
- `temporaryURLs`

with a single per-path transfer state containing:
- temp file URL
- write handle
- incremental SHA256 state
- bytes received
- next expected sequence
- manifest entry for that path if useful

Also track:
- finalized paths
- current active path, since only one file may be in progress at a time

### 2) Validate `FileSyncStart` manifest up front
On session start:
- reject duplicate paths
- reject invalid SHA256 lengths
- build a manifest lookup by path

### 3) Enforce stream ordering/state machine
Rules:
- only manifest-declared paths may appear
- only one active file at a time
- once a file starts receiving chunks, only that path may appear until commit
- no operations allowed for already finalized paths
- no duplicate commits
- mode-only updates must not appear while a file transfer is active

### 4) Validate chunk before write
For each chunk:
- verify path is declared
- verify path matches active path, or open a new active transfer if none exists
- reject zero-length chunk for non-empty files
- reject invalid SHA256 length
- reject wrong `sequence`
- update in-memory hasher and cumulative byte count using chunk data
- compare:
  - computed cumulative size == `chunk.cumulative_size`
  - computed cumulative hash == `chunk.sha256`
- fail immediately on mismatch
- fail immediately if cumulative size exceeds manifest-declared size
- only after successful validation, write chunk data to temp file

### 5) Commit without reread
On `FileSyncCommit`:
- path must match active path
- close write handle
- verify commit SHA256 length
- verify commit `size` and `sha256` exactly match:
  - manifest entry
  - in-memory transfer state
- no reopening and rereading temp file
- set file mode
- atomically rename temp file into place
- send `FileSyncAck`
- mark path finalized
- clear active transfer

### 6) Mode-only update handling
For `FileSyncSetMode`:
- allowed only when no active file transfer exists
- path must be declared in manifest
- path must not already be finalized
- request `size + sha256 + mode` must match the start manifest entry
- target file must already exist on disk
- apply mode only
- send `FileSyncAck`
- mark path finalized

### 7) EOF behavior
Keep current semantics:
- unchanged files need no explicit operation
- after EOF, prune stale files not present in CLI manifest
- send `FileSyncComplete`

### 8) Cleanup
On any failure:
- close active temp handle
- remove temp file
- clean up any remaining temp state
- abort stream immediately with error

## Go CLI changes

File:
- `go/internal/cli/commands/filesync.go`

### 1) Update manifest hashing to use raw digest bytes
Local manifest entries should carry:
- `size`
- `mode`
- `sha256 []byte`

### 2) Diff manifests on full identity
Manifest diff should compare:
- path
- size
- sha256
- mode

And classify into:
- content transfers: size or sha256 differ, or file missing remotely
- mode-only updates: size+sha256 same, mode differs
- unchanged: no operation
- stale remote files: printed by CLI, pruned implicitly by agent

### 3) Deterministic ordering
Sort operations by path:
- content transfers first
- mode-only updates second

### 4) Content transfer path
For each changed file:
- stream chunks in order
- maintain:
  - incremental SHA256
  - cumulative size
  - contiguous `sequence`
- each chunk sent includes:
  - `path`
  - `data`
  - `sequence`
  - `cumulative_size`
  - `sha256`
- after EOF for that file:
  - compare final streamed size to manifest size
  - compare final streamed digest to manifest digest
  - if mismatch, fail before commit
- send `FileSyncCommit`
- wait for file-level ack

### 5) Empty files
No special case anymore:
- send one empty chunk with sequence/checkpoint data
- then send commit

### 6) Mode-only updates
After all content transfers:
- print one line per file, e.g. `mode changed: path 0644 -> 0755`
- send `FileSyncSetMode { path, mode, size, sha256 }`
- wait for normal `FileSyncAck`

### 7) Deletion printing
Before closing the stream:
- compute stale remote files from manifest diff
- print one simple line per stale file to be deleted
- still do not send delete requests

## Tests

### Swift tests
File:
- `swift/Tests/WendyAgentTests/FileSyncServiceTests.swift`

Add/update tests for:

#### Manifest/session validation
- duplicate manifest path rejected
- invalid 32-byte digest length rejected

#### Chunk validation
- multi-chunk happy path
- wrong cumulative hash fails immediately
- wrong cumulative size fails immediately
- wrong sequence fails
- cumulative size exceeding manifest size fails early
- zero-length chunk for non-empty file rejected
- undeclared path rejected
- switching paths mid-file rejected

#### Commit behavior
- commit must match manifest
- commit must match in-memory final state
- duplicate commit rejected
- no reread required
- empty file via one empty chunk + commit succeeds

#### Mode-only updates
- mode-only update succeeds for existing unchanged file
- missing target file fails
- mode-only during active transfer rejected
- duplicate/finalized-path mode update rejected

#### Cleanup
- temp file removed on chunk validation failure
- temp file removed on commit failure
- no destination file on corruption

#### Existing behavior
- stale file pruning still works
- path validation still works

### Go tests
File:
- `go/internal/cli/commands/filesync_test.go`

Add/update tests for:
- manifest entries use 32-byte SHA256 values
- diff detects mode-only changes separately
- chunk sequence starts at 0 and increments contiguously
- each chunk carries correct post-chunk cumulative size/hash
- final streamed size/hash must match manifest before commit
- empty file sends one empty chunk
- deterministic sorted operation order
- printed lines for mode changes and deletions

## Rollout order

1. Update `Proto/wendy/agent/services/v1/wendy_agent_v1_file_sync_service.proto`
2. Regenerate Go and Swift protobuf/gRPC bindings
3. Update Go CLI manifest + diffing logic
4. Update Go CLI chunk sending and mode-only update sending
5. Refactor Swift `FileSyncService` state machine
6. Implement chunk validation before write
7. Remove commit-time reread/rehash
8. Implement mode-only handling
9. Update Swift and Go tests
10. Run end-to-end validation on large files

## Expected outcome

- no post-transfer reread on the Swift agent
- integrity checked on every chunk
- immediate failure on corruption/misordering
- mode-only changes propagate without content retransfers
- stale files still pruned implicitly
- simpler and stricter file-sync state machine
