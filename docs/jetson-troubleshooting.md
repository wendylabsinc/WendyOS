# Jetson Operations How-To

Operational commands and fixes for WendyOS on NVIDIA Jetson Orin Nano.

---

## Restore Rootfs Slot Integrity

### Symptom

`/data/device-status.sh` shows a rootfs slot as `unbootable`:

```
slot: 1,    retry_count: 0,    status: unbootable
```

This blocks OTA updates — Mender will switch to the target slot, but UEFI
firmware detects the `unbootable` status and falls back to the current slot
before Linux even boots.

### Cause

The slot was previously written to but never marked successful (e.g. after a
failed or interrupted OTA). The UEFI variable `RootfsStatusSlotB` holds a
persistent `unbootable` flag.

### Fix

Run on the Jetson as root. The write format is always:
- bytes 0–3: UEFI variable attributes (`NV=1 + BS=2 + RT=4 = 0x07`)
- bytes 4–7: status payload (`0x00000000` = normal)

**Slot B (slot index 1):**

```bash
chattr -i /sys/firmware/efi/efivars/RootfsStatusSlotB-781e084c-a330-417c-b678-38e696380cb9
printf '\x07\x00\x00\x00\x00\x00\x00\x00' \
  > /sys/firmware/efi/efivars/RootfsStatusSlotB-781e084c-a330-417c-b678-38e696380cb9
```

**Slot A (slot index 0):**

```bash
chattr -i /sys/firmware/efi/efivars/RootfsStatusSlotA-781e084c-a330-417c-b678-38e696380cb9
printf '\x07\x00\x00\x00\x00\x00\x00\x00' \
  > /sys/firmware/efi/efivars/RootfsStatusSlotA-781e084c-a330-417c-b678-38e696380cb9
```

> **Caution:** Only reset slot A while booted from slot B (and vice versa).
> Resetting the currently active slot's status mid-boot is harmless, but doing
> it on the wrong slot during a half-completed OTA can confuse the bootloader.

### Verify

```bash
/data/device-status.sh
```

Expected output after fix:

```
slot: 1,    retry_count: 0,    status: normal
```

### Notes

- `nvbootctrl mark-boot-successful` was removed in L4T 35.2.1; the efivarfs
  write above is the replacement.
- The `retry_count` stays at 0 after this fix; it increments only on actual
  boot attempts. A successful OTA will reset it to the configured maximum.
- If `/data/mender/tegra-bl-version-before` is still present after a completed
  OTA cycle, it is safe to delete: `rm /data/mender/tegra-bl-version-before`

---
