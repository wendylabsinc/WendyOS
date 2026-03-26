# WENDYCONFIG on RPi5 — Pairing Plan

We work through this together in phases. At each checkpoint you share output,
we look at it together, and only then move to the next step. The goal is a
solid, tested implementation — not just something that builds.

The RPi5 path is simpler than Jetson in one way (WIC handles FAT32 formatting
automatically — no pre-built image blob, no XML injection) and the
`expand-rootfs.sh` conflict is avoided entirely by inserting WENDYCONFIG
**between** boot and root rather than appending it at the end. Root stays the
trailing partition and grows freely to 100% — no changes to expand-rootfs
needed.

> **⚠️ Team check required before starting Phase 2**
>
> This plan uses the partition order **p1=boot, p2=WENDYCONFIG, p3=root**
> instead of the originally assumed p1=boot, p2=root, p3=WENDYCONFIG.
>
> Root is still found at runtime via `findmnt` + sysfs (no hardcoded partition
> numbers anywhere in expand-rootfs.sh), and the bootloader references root by
> PARTUUID — so the number shift from p2→p3 is transparent to both the
> bootloader and the resize logic. However, confirm with the team that nothing
> else in the stack (U-Boot env, CI tooling, documentation, Go CLI assumptions)
> expects root to always be p2 before we cut the WKS change.

Files we will touch:

| File | Change |
|---|---|
| `wic/rpi-nvme-partuuid.wks` | Insert WENDYCONFIG as p2 (64 MB FAT32); root becomes p3 |
| `classes/partuuid-rpi.bbclass` | Add `WENDYOS_CONFIG_PARTUUID` in cache read, cache write, `do_generate_partuuids`, and `WICVARS` |

`expand-rootfs.sh` is **not touched** — it locates root via `findmnt` and
reads the partition number from sysfs, so it adapts automatically when root
moves from p2 to p3.

---

## Phase 1 — Understand the baseline

Before touching anything, we confirm the current partition layout.

### 1.1 — Inspect the current WIC image

If there is a recent build, we can read the partition table directly without
rebuilding. From outside the Docker container:

```bash
WIC=build/tmp/deploy/images/raspberrypi5-nvme-wendyos/wendyos-image-raspberrypi5-nvme-wendyos.rootfs.wic
fdisk -l "$WIC"
```

If there is no build yet, or the image is stale, kick off a build first:

```bash
make build MACHINE=raspberrypi5-nvme-wendyos
```

Share: the full `fdisk -l` output. We want to see the two existing partitions —
boot (p1, FAT32, 128 MB) and root (p2, ext4, 8 GB) — and confirm there is no
p3 yet.

**Checkpoint 1:** The existing WIC image is boot + root, no third partition.

---

## Phase 2 — Add WENDYCONFIG to the WKS and rebuild

### 2.1 — Add the partition line to the WKS

Edit `wic/rpi-nvme-partuuid.wks`. Insert WENDYCONFIG **between** boot and root,
using a temporary hardcoded UUID for now (the bbclass integration comes in
Phase 4). Generate one with `uuidgen`:

```
bootloader --ptable gpt
part /boot  --source bootimg-partition --ondisk nvme0n1 --fstype=vfat --label boot        --active --align 4096 --size 128M   --uuid "${WENDYOS_BOOT_PARTUUID}"
part                                   --ondisk nvme0n1 --fstype=vfat  --label WENDYCONFIG --align 4096 --size 64M    --uuid "XXXXXXXX-XXXX-XXXX-XXXX-XXXXXXXXXXXX"
part /      --source rootfs            --ondisk nvme0n1 --fstype=ext4  --label root        --align 4096 --size 8192M  --uuid "${WENDYOS_ROOT_PARTUUID}"
```

Result: p1=boot, p2=WENDYCONFIG, p3=root (trailing, grows freely).

The `--label WENDYCONFIG` is what sets the FAT32 volume label — the same flag
that makes `/boot` show up as `boot` on macOS today.

### 2.2 — Rebuild

```bash
make build MACHINE=raspberrypi5-nvme-wendyos
```

### 2.3 — Inspect the new partition table

```bash
WIC=build/tmp/deploy/images/raspberrypi5-nvme-wendyos/wendyos-image-raspberrypi5-nvme-wendyos.rootfs.wic
fdisk -l "$WIC"
```

Share: the full output. We want to see three partitions — boot, WENDYCONFIG,
root — in that order, and confirm:
- p2 size is ~64 MB
- p2 type is `EFI System` or `Microsoft basic data`
- p3 is root at 8 GB, last on disk
- No gaps or overlaps

### 2.4 — Attach the WIC on macOS and verify the label

```bash
cp "$WIC" /tmp/wendyos-rpi-test.img
hdiutil attach /tmp/wendyos-rpi-test.img
```

Share:
- The hdiutil output (device numbers and mount paths for each partition)
- `ls /Volumes/` — does `/Volumes/WENDYCONFIG` appear alongside `/Volumes/boot`?
- `diskutil info /Volumes/WENDYCONFIG`

This is the key macOS label test. WIC calls `mkfs.vfat` with the `--label`
argument internally; this verifies it actually sets the FAT32 volume label as
expected. If this does not work here, it will not work after a real `dd` either.

### 2.5 — Write and read a test file, then detach

```bash
echo "WENDYCONFIG test" > /Volumes/WENDYCONFIG/test.txt
cat /Volumes/WENDYCONFIG/test.txt
rm /Volumes/WENDYCONFIG/test.txt
hdiutil detach /dev/diskX   # the disk device hdiutil reported for WENDYCONFIG in 2.4
```

**Checkpoint 2:** The new WIC image has three partitions in the correct order.
The WENDYCONFIG partition auto-mounts on macOS under `/Volumes/WENDYCONFIG`,
is writable, and detaches cleanly.

---

## Phase 3 — Flash and verify on hardware

### 3.1 — Flash

```bash
make flash-to-external MACHINE=raspberrypi5-nvme-wendyos
# when prompted: pick the NVMe disk
```

### 3.2 — Plug the NVMe into the Mac and check the partition table

Share:

```bash
diskutil list external physical
```

We want to confirm all three partitions are present, sized correctly, and
that WENDYCONFIG has the right label.

### 3.3 — Check auto-mount

```bash
ls /Volumes/
```

Share: does `/Volumes/WENDYCONFIG` appear automatically without any manual
mount command? This is what the Go CLI will rely on after `dd`.

```bash
diskutil info /Volumes/WENDYCONFIG
```

Share the full output — especially `File System Personality` and `Volume Name`.

### 3.4 — Write and read back

```bash
echo "hello from wendy" > /Volumes/WENDYCONFIG/wendy.conf
cat /Volumes/WENDYCONFIG/wendy.conf
rm /Volumes/WENDYCONFIG/wendy.conf
diskutil unmount /Volumes/WENDYCONFIG
```

### 3.5 — Boot the RPi5 and verify expand-rootfs

Put the NVMe back in the RPi5 and boot. SSH in. Share:

```bash
lsblk -o NAME,SIZE,FSTYPE,LABEL,MOUNTPOINT
df -h /
cat /var/log/expand-rootfs.log
```

We want:
- p3 (root) sized to fill the remaining NVMe space (NVMe size minus 128 MB
  boot minus 64 MB WENDYCONFIG, roughly)
- `/` df output showing the expanded space
- Log showing the `growpart` or `parted 100%` path succeeded cleanly
- p2 (WENDYCONFIG) still present and untouched

**Checkpoint 3:** WENDYCONFIG auto-mounts on macOS after a real `dd` flash,
is writable, and unmounts cleanly. Root expands to fill the remaining disk on
first boot. All three partitions are intact.

---

## Phase 4 — Wire up `WENDYOS_CONFIG_PARTUUID`

### 4.1 — Update `classes/partuuid-rpi.bbclass`

Four locations:

1. **Cache read block** — add a branch for `WENDYOS_CONFIG_PARTUUID=`:
   ```python
   elif line.startswith('WENDYOS_CONFIG_PARTUUID='):
       config_uuid = line.split('=', 1)[1].strip()
   ```

2. **Cache write block** — append the third UUID to the file:
   ```python
   f.write(f"WENDYOS_CONFIG_PARTUUID={config_uuid}\n")
   ```

3. **`do_generate_partuuids`** — write the UUID to both workdir and deploy
   conf files alongside boot and root.

4. **`WICVARS` append** — add `WENDYOS_CONFIG_PARTUUID` to the extra string so
   WIC receives it as a variable.

### 4.2 — Update `wic/rpi-nvme-partuuid.wks`

Replace the temporary hardcoded UUID with the variable:

```
part  --ondisk nvme0n1 --fstype=vfat --label WENDYCONFIG --align 4096 --size 64M --uuid "${WENDYOS_CONFIG_PARTUUID}"
```

### 4.3 — Rebuild

```bash
make build MACHINE=raspberrypi5-nvme-wendyos
```

Share the deploy conf file that the bbclass writes — it should now contain
all three UUIDs:

```bash
cat build/tmp/deploy/images/raspberrypi5-nvme-wendyos/partuuids-wendyos-image-raspberrypi5-nvme-wendyos.conf
```

Confirm `WENDYOS_CONFIG_PARTUUID` is present and a valid UUID4.

### 4.4 — Flash and do a final end-to-end verification

```bash
make flash-to-external MACHINE=raspberrypi5-nvme-wendyos
```

Repeat the checks from Phase 3:
- WENDYCONFIG auto-mounts on macOS ✓
- Root expands to fill remaining disk space on first boot ✓
- The UUID in `diskutil info /Volumes/WENDYCONFIG` matches
  `WENDYOS_CONFIG_PARTUUID` in the deploy conf ✓

**Checkpoint 4:** The full implementation is working end-to-end with a stable,
tracked partition UUID. Ready to commit.

---

## Done

At this point we have:
- WENDYCONFIG present in every RPi5 NVMe image as p2 (64 MB FAT32)
- Root as p3, trailing, expanding freely on first boot — no changes to
  `expand-rootfs.sh`
- Confirmed macOS auto-mount behaviour after `dd`
- A stable partition UUID generated and cached by `partuuid-rpi.bbclass`
- Two clean commits ready for the PR
