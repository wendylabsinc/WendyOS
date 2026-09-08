Lists all local caches used by Wendy: downloaded OS images and the per-app build caches (`buildx/` and `ocilayout/`) that make warm rebuilds and chunk-diff deploys fast.

Pass `--json` to receive a JSON array on stdout instead of the human-readable output described below.

## Output structure

- **OS images** (`os-images/`) — each image file is listed as a separate row prefixed `os-images/`.
- **Build caches** (`buildx/` and `ocilayout/`) — each per-app sub-directory is listed as a separate row named `<store>/<app-id>-<platform>`, e.g. `ocilayout/com.example.myapp-linux_arm64` (app ID and platform are lower-cased, with characters outside `a-z`, `0-9`, `-` and `.` replaced by `_`). Build-cache rows also carry a last-built age. The root-level shared store inside each of these directories (`blobs/`, `ingest/`, index files) is folded into the summary line rather than listed, because it cannot be deleted without taking every app cache in that store down with it.
- **Everything else** — listed as a single top-level row.

Entries are sorted largest-first so the biggest reclaimable items are at the top, in both interactive and non-interactive output.

> **Build-cache sizes are per-link.** Automatic maintenance hardlinks a layer shared by several apps into each of their directories. A per-row size (and the `sizeBytes` / `size` JSON fields) counts that layer once per link, so build-cache rows can sum past the real on-disk total. The summary line's "on disk" figure is the dedup-aware total — the same number the automatic size cap is enforced against.

When `--json` is **not** passed:

- In an **interactive terminal** (stdin and stdout are both TTYs), an interactive picker is shown listing all cached items. Build-cache entries show their last-built age alongside the size (e.g. `2.0 GB  ·  built 3h ago`); other entries show only the size. When build caches are present, a summary line showing real on-disk usage and the cap appears above the picker. Selected items can be removed.
- In a **non-interactive** context (pipe, CI, etc.), a plain text list is printed, one entry per line. Build-cache rows use the format `  <name>  (<size>, built <age>)`; all other rows use `  <name>  (<size>)`. When build caches are present, a summary line is printed after the list:

  ```
    ocilayout/com.example.myapp-linux_arm64  (2.0 GB, built 3h ago)
    os-images/wendyos-raspberry-pi-5-0.10.4.img  (4.5 GB)

  Build cache: 83.0 GB on disk (1.2 GB shared) · cap 100.0 GB
  ```

  "on disk" is the dedup-aware total; "shared" is the size of the root-level shared store (`blobs/`, `ingest/`) that is never evicted on its own; "cap" is the current size cap (see [Automatic maintenance](#automatic-maintenance)).

When the cache is empty (or the cache directory does not exist), the command prints `Cache is empty.` in human-readable mode, or an empty JSON array (`[]`) with `--json`.

## JSON output

### `wendy cache list --json`

Each element of the returned array represents one cached item. The summary line is not included in JSON output.

```json
[
  {
    "name": "os-images/wendyos-raspberry-pi-5-0.10.4.img",
    "path": "/Users/you/Library/Caches/wendy/os-images/wendyos-raspberry-pi-5-0.10.4.img",
    "sizeBytes": 4831838208,
    "size": "4.5 GB"
  },
  {
    "name": "ocilayout/com.example.myapp-linux_arm64",
    "path": "/Users/you/Library/Caches/wendy/ocilayout/com.example.myapp-linux_arm64",
    "sizeBytes": 2147483648,
    "size": "2.0 GB",
    "lastBuilt": "3h ago"
  }
]
```

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Display name of the cache entry. OS images are prefixed with `os-images/`; per-app build caches with `buildx/` or `ocilayout/` |
| `path` | string | Absolute path to the cached file or directory |
| `sizeBytes` | integer | Size in bytes. For build-cache entries this is the per-link size and can exceed the entry's share of real disk usage (see the note above) |
| `size` | string | Human-readable size string |
| `lastBuilt` | string | How long ago a build last wrote to this cache (e.g. `"12m ago"`, `"3h ago"`, `"2d ago"`). Only present on build-cache entries, and omitted when the directory holds no layers yet |

> **Note:** `path` is a full local filesystem path and may include the current username or CI runner cache location. Redact it before forwarding command output to shared logs or support channels.

Returns `[]` when the cache is empty.

### `wendy os cache list --json`

Each element represents one cached OS image file:

```json
[
  {
    "name": "wendyos-raspberry-pi-5-0.10.4.img",
    "sizeBytes": 4831838208,
    "size": "4.5 GB"
  }
]
```

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Filename of the cached OS image |
| `sizeBytes` | integer | Size in bytes |
| `size` | string | Human-readable size string |

Returns `[]` when no cached OS images are found.

## Automatic maintenance

The build caches are bounded automatically. After a `wendy run` deploy that exports through the persistent OCI layout (the chunk-diff path used with the Docker builder), Wendy:

1. **Deduplicates** content-identical layers across the `buildx` and `ocilayout` stores by hardlinking them, so a layer shared by several apps — or held in both stores — costs one copy instead of N.
2. **Evicts** whole per-app cache directories, least-recently-built first, until the dedup-aware on-disk total is at or under the size cap. Evicting a directory only frees a layer when it held the layer's last remaining link.
3. **Bounds the builder's own store** by running `docker buildx prune --max-used-space` against the OCI-export builder (best-effort, with a 60-second budget).

No manual action is needed. Maintenance is best-effort: a failure leaves the caches as they are for the next run to reclaim.

### `WENDY_BUILD_CACHE_MAX_BYTES`

Set this environment variable (in bytes) to override the default size cap of 100 GiB. A value of `0` or a negative number disables eviction and the builder-store prune; deduplication still runs.

### Eviction safety guards

A per-app cache directory is never evicted while it may be in use:

- **Lock protection** — a directory whose layout lock is held by another `wendy` process (for the whole of its build → read → push) is skipped, however old its contents look. It becomes eligible again once that process releases the lock.
- **Active-window protection** — a directory that a build wrote a layer into within the last 10 minutes is skipped as a likely in-progress build.
- **Own build** — the deploying process always pins its own app's directory.

### `wendy cache dedup`

A hidden `wendy cache dedup [--max <bytes>]` command runs the same on-disk maintenance on demand, for manual reclaim or debugging. It is not shown in `wendy cache --help` because the automatic path is the intended one.

- `--max` sets the size cap for this run (defaults to `WENDY_BUILD_CACHE_MAX_BYTES` or 100 GiB). Pass `0` to deduplicate only, without evicting anything.
- It only ever deduplicates or deletes cache content, honours the same safety guards as automatic maintenance, and is safe to run at any time.
- It prints `Deduplicated <size>, evicted <size>.` on completion.
