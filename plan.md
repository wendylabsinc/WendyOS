# WENDYCONFIG on Jetson — Pairing Plan

We work through this together in phases. At each checkpoint you share output,
we look at it together, and only then move to the next step. The goal is a
solid, tested implementation by the end — not just something that builds.

---

## Phase 1 — Understand the baseline

Before touching anything, we need to understand exactly what we're working
with. Surprises here save a lot of pain later.

### 1.1 — Get a tegraflash bundle

```bash
cd WendyOS
make deploy
```

Share: the filename and size of what lands in `deploy/`.

### 1.2 — Extract it

```bash
mkdir -p /tmp/tegrawork
tar -xzf deploy/wendyos-image-jetson-orin-nano-devkit-nvme-wendyos.tegraflash.tar.gz \
    -C /tmp/tegrawork
ls -lh /tmp/tegrawork
```

Share: the full `ls -lh` output. We want to see all the blob files and
confirm the XML is there.

### 1.3 — Read the XML partition layout

```bash
cat /tmp/tegrawork/flash_l4t_t234_nvme_rootfs_ab.xml
```

Share: the full XML. We'll read it together and map out:
- The exact partition order and IDs
- Where p16 sits (is the slot truly empty, or is something else there?)
- The `mender_data` block — size, allocation\_attribute, filename
- The `UDA` block — does it have a filename or not (the bbappend strips it)

### 1.4 — Understand what doexternal.sh does

```bash
head -80 /tmp/tegrawork/doexternal.sh
```

Share the output. We want to confirm:
- Does it look for blob files relative to CWD, or does it resolve paths from
  the XML?
- What does the `-s` flag do exactly?
- Any other flags worth knowing about?

**Checkpoint 1:** We don't proceed until we understand the partition layout
and how doexternal.sh resolves filenames.

---

## Phase 2 — Create and validate the FAT32 image

### 2.1 — Create it

On macOS (install dosfstools via `brew install dosfstools` if needed):

```bash
dd if=/dev/zero of=/tmp/wendy-config.fat32.img bs=1m count=64
mkfs.fat -F 32 -n "WENDYCONFIG" /tmp/wendy-config.fat32.img
```

Share: the full output of both commands.

### 2.2 — Inspect the image metadata

```bash
file /tmp/wendy-config.fat32.img
```

Share: the output. We want to see the label `WENDYCONFIG` confirmed in the
file metadata.

### 2.3 — Mount it on macOS

```bash
hdiutil attach /tmp/wendy-config.fat32.img
```

Share:
- The device and mount path that hdiutil reports
- The output of `ls /Volumes/WENDYCONFIG`
- The output of `diskutil info /Volumes/WENDYCONFIG`

This is the key test: if macOS auto-mounts it as `/Volumes/WENDYCONFIG` here,
it will do the same after `dd` to the real NVMe. Pay attention to the
filesystem type and volume name reported by diskutil.

### 2.4 — Write a test file and read it back

```bash
echo "WENDYCONFIG test" > /Volumes/WENDYCONFIG/test.txt
cat /Volumes/WENDYCONFIG/test.txt
rm /Volumes/WENDYCONFIG/test.txt
```

### 2.5 — Unmount

```bash
hdiutil detach /dev/diskX   # the disk hdiutil reported in 2.3
```

**Checkpoint 2:** The image mounts cleanly, shows the right label, is
writable, and unmounts cleanly. Only then do we touch the bundle.

---

## Phase 3 — Inject into the bundle and assemble

### 3.1 — Copy the image into the bundle

```bash
cp /tmp/wendy-config.fat32.img /tmp/tegrawork/wendy-config.fat32.img
ls -lh /tmp/tegrawork/wendy-config.fat32.img
```

### 3.2 — Edit the XML

Open `/tmp/tegrawork/flash_l4t_t234_nvme_rootfs_ab.xml` and insert the
WENDYCONFIG partition block immediately before the `mender_data` block:

```xml
    <partition name="wendy_config" id="16" type="data">
        <allocation_policy> sequential </allocation_policy>
        <filesystem_type> basic </filesystem_type>
        <size> 67108864 </size>
        <file_system_attribute> 0 </file_system_attribute>
        <allocation_attribute> 0x0 </allocation_attribute>
        <partition_type_guid> EBD0A0A2-B9E5-4433-87C0-68B6B72699C7 </partition_type_guid>
        <percent_reserved> 0 </percent_reserved>
        <align_boundary> 16384 </align_boundary>
        <filename> wendy-config.fat32.img </filename>
        <description> WendyOS first-boot config partition (FAT32, 4 MB). </description>
    </partition>
```

After editing, share:

```bash
grep -n "wendy_config\|mender_data\|UDA\|secondary_gpt" \
    /tmp/tegrawork/flash_l4t_t234_nvme_rootfs_ab.xml
```

We want to confirm the order is: UDA → wendy_config → mender_data →
secondary_gpt, with the right IDs.

### 3.3 — Assemble wendyos.img

This runs inside the Docker build container. From the WendyOS project root:

```bash
make shell
```

Inside the container:

```bash
cd /tmp/tegrawork
sudo ./doexternal.sh -s 64G /tmp/wendyos-test.img
```

Share: the full output of doexternal.sh. We're watching for:
- Each partition being placed at its offset — confirm `wendy_config` appears
  in the output
- Any errors about missing files (would mean the filename lookup works
  differently than expected)
- Final image size

### 3.4 — Inspect the assembled image

Still inside the container:

```bash
# Check the partition table of the assembled image
sudo fdisk -l /tmp/wendyos-test.img | head -40
# or
sudo gdisk -l /tmp/wendyos-test.img
```

Share the output. We want to confirm:
- WENDYCONFIG appears as a partition
- Its size is 4 MB
- Its type GUID is correct
- Partition order and numbering matches the XML

Copy the image to the deploy directory:

```bash
cp /tmp/wendyos-test.img /path/to/WendyOS/deploy/wendyos.img
```

**Checkpoint 3:** We can see WENDYCONFIG in the partition table of the
assembled image before we flash anything.

---

## Phase 4 — Flash and verify on hardware

### 4.1 — Flash

```bash
cd WendyOS
make flash-to-external
# when prompted: pick your NVMe disk
# it will use deploy/wendyos.img directly (already assembled)
```

### 4.2 — Plug the NVMe into the Mac

Share:

```bash
diskutil list external physical
```

We want to see the full partition table. Confirm:
- Partition count and order
- WENDYCONFIG present with correct size
- All existing partitions (APP, APP\_b, esp, UDA, mender\_data) intact

### 4.3 — Check auto-mount

```bash
ls /Volumes/
```

Share: does `/Volumes/WENDYCONFIG` appear automatically without any manual
mount command? This is the key macOS integration test.

```bash
diskutil info /Volumes/WENDYCONFIG
```

Share the full output.

### 4.4 — Write and read back

```bash
echo "hello from wendy" > /Volumes/WENDYCONFIG/wendy.conf
cat /Volumes/WENDYCONFIG/wendy.conf
rm /Volumes/WENDYCONFIG/wendy.conf
diskutil unmount /Volumes/WENDYCONFIG
```

### 4.5 — Boot the Jetson and verify mender\_data still expands ⏳ PENDING (no power supply)

Put the NVMe back in the Jetson, boot it, and SSH in. Share:

```bash
lsblk
df -h /mender_data    # or wherever mender_data mounts
```

We want to confirm `mender-growfs-data` still expanded mender\_data correctly
into the free space after it — WENDYCONFIG sitting before it should not have
affected this.

**Checkpoint 4:** WENDYCONFIG auto-mounts on macOS, is writable, and
mender\_data still expands normally on first boot.

> ⚠️ **Step 4.5 is pending** — skipped due to no power supply available at time of testing.
> Come back and run the two commands once a 19V/3A barrel jack supply (5.5mm/2.1mm) is available.
> Steps 4.1–4.4 passed. Phase 5 proceeded without 4.5.

---

## Phase 5 — Implement in Yocto

Only once Phase 4 passes do we write the actual recipes. At this point we know
exactly what we're automating — no guesswork.

### 5.1 — Write `wendy-config-partition_1.0.bb`

Based on what we learned in Phase 2, write the recipe. Share a draft and we'll
review it together before committing.

Key things to nail:
- `mkfs.fat` flags match exactly what worked in Step 2.1
- Deploy path matches where `doexternal.sh` expects the file (confirmed in
  Phase 3)

### 5.2 — Write the bbappend changes

Based on what we learned from reading the XML in Phase 1.3, write the `sed`
block that inserts the WENDYCONFIG partition. Share a draft.

Key things to nail:
- The `sed` anchor pattern matches the actual `mender_data` element exactly as
  it appears after the existing bbappend runs
- The inserted XML matches byte-for-byte what worked in Step 3.2

### 5.3 — Build and compare

Run a full Yocto build:

```bash
make build
make deploy
```

Then diff the XML from the new tegraflash bundle against the one we edited
manually in Phase 3:

```bash
diff /tmp/tegrawork/flash_l4t_t234_nvme_rootfs_ab.xml \
     <(tar -xOf deploy/wendyos-image-*.tegraflash.tar.gz \
         flash_l4t_t234_nvme_rootfs_ab.xml)
```

Share: the diff. Ideally it's empty. If not, we reconcile before proceeding.

### 5.4 — Full flash from the Yocto build

```bash
make flash-to-external
```

Repeat the verification from Phase 4: auto-mount, write/read, mender\_data
expansion on boot.

**Checkpoint 5:** The Yocto build produces an image that passes all the same
tests as the manually assembled one. No diff in the XML.

---

## Done

At this point we have:
- A validated FAT32 partition in every Jetson image
- Confirmed macOS auto-mount behaviour
- Confirmed no regression on mender\_data expansion
- Two clean, reviewed Yocto recipes ready to merge
