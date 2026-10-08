# Flashing WendyOS

The `wendy install` command writes a WendyOS image to a drive (SD card, USB, NVMe enclosure, etc.).

## Usage

```bash
wendy install --device-type <type> --drive <device>
```

Use `wendy os list-drives` to enumerate available drives.

## Migrating from WendyOS < 0.17.0

WendyOS 0.17.0 introduces a new OTA update system that is not backward-compatible
with pre-0.17.0 images. Devices running an older image cannot receive OTA updates;
`wendy os update` and `wendy device update` refuse the OS step and print guidance
to reflash. Perform a one-time reflash with `wendy install` (or `wendy os install`)
to migrate the device to the new update system. Once reflashed, OTA updates work
normally.

## How it works

`wendy install` downloads the WendyOS release zip (~5.5 GB) for the selected device type and writes it directly to the target drive. The compressed zip is never fully extracted to disk — the image entry is streamed from the zip to the drive in a single pass, so the peak temporary disk usage is the zip file itself.

### Block map acceleration

When a block map (`.bmap`) and seekable-zstd image (`.img.zst`) are available for the target storage variant, `wendy install` uses them to flash significantly faster:

- Only mapped (non-zero) ranges are written; holes are skipped entirely
- The seekable zstd format allows random access, so zero regions are never decompressed
- Typical Jetson images (~19 GB uncompressed, ~4 GB mapped data) flash in a fraction of the time

Use `--no-bmap` to disable this optimization and flash the full image. Use `--storage nvme` or `--storage sd` to force a specific storage variant when the auto-detection is incorrect (e.g., an NVMe drive in a USB enclosure).

### Block map error handling

If a bmap-accelerated write fails — for example due to a checksum mismatch or a stale/incorrect published bmap — `wendy install` automatically falls back to a full sequential write using the already-cached `.img.zst` or `.zip`. No user action and no re-download are required.

A failure that occurs *during* the fallback write (short write, decode error, or a non-zero helper exit) is fatal: the helper's stderr is surfaced as the error.

### Drive-size check

`wendy install` refuses a target drive that is smaller than the uncompressed image, since a truncated image never boots:

```
sdb (/dev/sdb) is too small for this image: it holds 7.9 GB but the image needs 18.2 GB; use a larger SD card or drive
```

When a block map is published, its image size is checked before the Wi-Fi, device-name and pre-enrollment prompts and before the download, so an undersized drive is rejected up front. The check runs again before writing, using the image actually written. It is skipped when the drive or image size cannot be determined.

### Image formats

Both raw (`.img`) and gzip-compressed (`.img.gz`) images are supported, in addition to zip archives. Gzip content is detected by inspecting the file's magic bytes rather than its extension, so a cached or renamed image without a `.gz` suffix is still handled correctly. Gzip images are decompressed on the fly and streamed straight to the drive — the full decompressed image is never written to a temporary file.

### Caching

The downloaded zip is cached locally. On subsequent installs for the same device type the cached zip is used directly, avoiding a repeat download. Legacy `.img` cache entries from older versions of the tool are still recognised as a fallback.

Use `wendy os download` to pre-populate the cache without performing an install.

### Progress

While writing, the tool reports bytes written to the drive. There is no separate "Extracting image…" step.

## Disk usage

| Scenario | Peak temporary disk usage | Cache at rest |
|---|---|---|
| First install | ~5.5 GB (zip only) | ~5.5 GB `.zip` |
| Cached install | negligible | ~5.5 GB `.zip` |

## Platform notes

### macOS

The image is written via `dd` to the raw disk device (`/dev/rdiskN`), bypassing the filesystem buffer cache.

### Linux

Before writing, all mounted partitions on the target disk are unmounted automatically. If any partition cannot be unmounted, the error is reported and the write does not proceed.

The image is written via `dd` with `conv=fdatasync` to ensure the device is flushed before the command exits.

### Windows

The image is written directly to the raw disk device. After writing, any auto-assigned drive letters are removed from all partitions to prevent phantom drives from appearing in Explorer. For fixed (non-removable) disks, the disk is then taken offline. For removable media (USB, SD, MMC), the offline step is skipped — Windows does not support setting removable media offline, and physically removing the media serves as the eject action.

## Errors

On macOS and Linux, if `dd` exits with a non-zero status its stderr output (the underlying cause, e.g. "Operation not permitted" or "No space left on device") is captured and included in the error reported to you, rather than just the exit code. Continuous `status=progress` output is filtered out and the captured diagnostics are bounded, so the message stays focused on the actual failure.
