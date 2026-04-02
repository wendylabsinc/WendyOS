# Fix in-memory buffering in UpdateAgent and WriteLayer

Two existing RPCs — `UpdateAgent` and `WriteLayer` — accumulate all
incoming chunks into a `[]byte` in memory before writing to disk. This
makes them unusable for files larger than available RAM. This plan fixes
that bug as a standalone correctness change, independent of any feature
work.

## Development Approach

Each iteration starts with failing tests that pin down a specific
behaviour, then adds the minimum implementation to make them pass.

Test style throughout: plain `testing.T`, behaviour-descriptive names,
table-driven subtests for exhaustive validation cases, bufconn in-process
gRPC servers for service-level tests. No BDD framework.

---

### Iteration 1 — Large files don't OOM during agent update

*Touches: `agent_service.go`, `agent_service_test.go`*

Write tests proving `UpdateAgent` streams chunks to a temp file: the temp
file exists mid-transfer; the final file has correct content; a SHA256
mismatch on commit leaves no partial file; an interrupted stream leaves
no `.tmp` at the target path. All fail. Replace the `[]byte` accumulator
with a temp-file writer. Tests pass.

---

### Iteration 2 — Large files don't OOM during layer writes

*Touches: `container_service.go`, `container_service_test.go`*

Same pattern applied to `WriteLayer`. One additional case: if the blob
already exists with a matching digest the transfer is a no-op and the
existing file is untouched. All existing container service tests continue
to pass.

---

## Part 1 — Fix in-memory buffering in `UpdateAgent` and `WriteLayer`

**Goal:** Make the two existing streaming RPCs safe for arbitrarily large
files by writing chunks directly to disk as they arrive.

This is a correctness fix independent of the assets feature. It should be
landed first and in isolation.

### `UpdateAgent` — agent side

**File:** `go/internal/agent/services/agent_service.go`

Replace the `binaryData []byte` accumulator with a temp-file writer:

- On the first chunk, resolve `execPath` (do it early, not after receiving all
  data), create a temp file at `execPath + ".update.tmp"`.
- On each subsequent chunk, write the chunk to the temp file and feed it to the
  running `sha256.New()` hasher. Do not accumulate in memory.
- On the commit control message, flush and close the temp file, verify the
  SHA256, set permissions, rename backup, atomic rename into place — same logic
  as today, just operating on the temp file rather than an in-memory slice.
- On any error, remove the temp file before returning.

### `UpdateAgent` — CLI side

**File:** `go/internal/cli/commands/device.go`, `deviceUpdateUpload`

Replace the `binaryData []byte` parameter with an `io.Reader` and a size.
Open the source file with `os.Open`, stream it in 64 KiB chunks directly into
the send loop. Compute SHA256 on the fly during the read rather than calling
`sha256.Sum256` on the full slice beforehand.

Callers (`deviceUpdateUpload` call sites) pass an `*os.File` or a
`bytes.Reader` wrapping a small in-memory download; the function signature
becomes `deviceUpdateUpload(ctx, service, r io.Reader, size int64, sha256Hash
string)`.

### `WriteLayer` — agent side

**File:** `go/internal/agent/services/container_service.go`

Replace the `var data []byte` accumulator with a temp-file writer:

- On the first message (which carries the digest), derive the target path
  `<blobsDir>/sha256/<hex>` and create a temp file alongside it
  (`<blobsDir>/sha256/<hex>.tmp`).
- Write each chunk to the temp file while feeding the SHA256 hasher.
- On stream EOF, verify the digest, atomic rename `.tmp` → final path.
- If the blob already exists (digest matches), remove the temp file and return
  success without overwriting.

### Acceptance criteria

- `wendy device update` with a 200 MB+ binary completes without OOM.
- Interrupting the stream mid-transfer leaves no partial file at the target
  path (only the `.tmp` file, which is cleaned up on next attempt).
- All existing agent and container service tests pass.
