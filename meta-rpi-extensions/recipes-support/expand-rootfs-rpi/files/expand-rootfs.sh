#!/usr/bin/env bash
set -euo pipefail

STAMP="/var/lib/expand-rootfs.done"
LOG="/var/log/expand-rootfs.log"

# Redirect all output to log file and console
exec > >(tee -a "$LOG") 2>&1 || true
echo "[expand-rootfs] Start $(date -Is || true)"

# Run only once
if [[ -f "$STAMP" ]]; then
  echo "[expand-rootfs] Stamp file exists; exiting."
  exit 0
fi

# Detect root device and filesystem type
ROOTDEV="$(findmnt -n -o SOURCE /)"
FSTYPE="$(findmnt -n -o FSTYPE /)"

if [[ "$ROOTDEV" != /dev/* ]]; then
  echo "[expand-rootfs] Root device is not a physical partition ($ROOTDEV) -> skip."
  mkdir -p "$(dirname "$STAMP")"; touch "$STAMP"; exit 0
fi

# Use sysfs to find partition number and parent disk (portable)
PART_BASENAME="$(basename "$ROOTDEV")"   # e.g. mmcblk0p2
PARTNUM="$(cat "/sys/class/block/${PART_BASENAME}/partition")"
PARENT_SYS="$(readlink -f "/sys/class/block/${PART_BASENAME}/..")"
DISK="/dev/$(basename "$PARENT_SYS")"    # e.g. /dev/mmcblk0
echo "[expand-rootfs] RootDEV=$ROOTDEV Disk=$DISK Part=$PARTNUM FS=$FSTYPE"

# Skip if an A/B rootfs layout is detected (e.g. mender)
if lsblk -rno PARTLABEL "$DISK" 2>/dev/null | grep -qiE 'root[a-b]'; then
  echo "[expand-rootfs] Detected A/B rootfs layout -> skip."
  mkdir -p "$(dirname "$STAMP")"; touch "$STAMP"; exit 0
fi

# GPT backup header when image is flashed to a larger card
if command -v sgdisk >/dev/null 2>&1; then
  echo "[expand-rootfs] sgdisk -e $DISK (fix GPT backup at end)"
  sgdisk -e "$DISK" || true
  command -v partprobe >/dev/null 2>&1 && partprobe "$DISK" || true
  udevadm settle || true
fi

#Resize root partition to fill the entire disk
if command -v growpart >/dev/null 2>&1; then
  echo "[expand-rootfs] growpart $DISK $PARTNUM"
  growpart "$DISK" "$PARTNUM" || true
else
  echo "[expand-rootfs] parted -s $DISK unit % resizepart $PARTNUM 100%"
  parted -s "$DISK" unit % resizepart "$PARTNUM" 100% || true
fi

command -v partprobe >/dev/null 2>&1 && partprobe "$DISK" || true
udevadm settle || true

#Resize filesystem
case "$FSTYPE" in
  ext4|ext3|ext2) resize2fs "$ROOTDEV" ;;
  xfs)            xfs_growfs / ;;
  btrfs)          btrfs filesystem resize max / ;;
  *)              echo "[expand-rootfs] Unknown filesystem type: $FSTYPE -> skipping resize." ;;
esac

echo "[expand-rootfs] OK"
mkdir -p "$(dirname "$STAMP")"
touch "$STAMP"
