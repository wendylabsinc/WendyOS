# `wendy os cache list`

Lists cached WendyOS OS images stored locally.

## Usage

```sh
wendy os cache list [--json]
```

## Description

`wendy os cache list` shows OS image files and Jetson flashpacks that have been downloaded to the local cache by `wendy install` or `wendy os download`. A flashpack is listed with the total size of its extracted files. Cached images are reused on subsequent installs to avoid re-downloading.

Pass `--json` to receive a JSON array on stdout instead of the human-readable output described below.

## Human-readable output

When `--json` is **not** passed, each cached image is printed one per line with its size in MiB:

```text
  wendyos-raspberry-pi-5-0.10.4.img  (4608.0 MB)
  ...

Cache directory: /Users/you/Library/Caches/wendy/os-images
```

When no cached images are found (or the cache directory does not exist), the command prints:

```text
No cached OS images.
```

## JSON output (`--json`)

Each element of the returned array represents one cached OS image file or Jetson flashpack:

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
| `name` | string | Filename of the cached OS image, or the flashpack directory name |
| `sizeBytes` | integer | Size in bytes |
| `size` | string | Human-readable size string |

Returns `[]` when no cached OS images are found.

> **Note:** Unlike `wendy cache list --json`, this output does not include a `path` field.

## Error handling

If reading the metadata for a cache entry fails, the command exits with an error rather than silently skipping the entry.

## Related

- [`wendy cache list`](../../cache/list.md) — lists all cached items, including OS images
- [`wendy install`](../install.md) — installs WendyOS and populates the cache
- [`wendy os download`](../download.md) — downloads WendyOS images into the cache
