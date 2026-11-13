#!/bin/sh
# One line per whole disk per second: cumulative sectors written and flushes.
while true; do
  awk -v t="$(date +%s)" '$3 ~ /^(nvme[0-9]+n[0-9]+|mmcblk[0-9]+|sd[a-z]+)$/ {
    printf "%s %s sectors_written=%s flushes=%s\n", t, $3, $10, $19 }' /proc/diskstats
  sleep 1
done
