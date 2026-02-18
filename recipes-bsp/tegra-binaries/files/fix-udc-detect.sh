#!/bin/bash
# This script fixes the UDC detection in nv_enable_remote.sh

FILE="$1"

# Use awk to replace the UDC detection section
awk '
/# find UDC device for usb device mode/ {
    print
    getline; print  # udc_dev=""
    # Skip the old lines and insert new logic
    while (getline) {
        if (/if \[ -z "\$\{udc_dev\}" \]/ || /if \[ "\$\{udc_dev\}" == "" \]/) {
            # Print the new UDC detection logic before this line
            print "\tfor _ in $(seq 5); do"
            print "\t\techo \"Finding UDC\""
            print "\t\t# Dynamically find any UDC device (works for all Tegra platforms)"
            print "\t\tfor udc_candidate in /sys/class/udc/*; do"
            print "\t\t\tif [ -e \"${udc_candidate}\" ]; then"
            print "\t\t\t\tudc_dev=$(basename \"${udc_candidate}\")"
            print "\t\t\t\tbreak 2"
            print "\t\t\tfi"
            print "\t\tdone"
            print "\t\tif [ -n \"${udc_dev}\" ]; then"
            print "\t\t\tbreak"
            print "\t\tfi"
            print "\t\tsleep 1"
            print "\tdone"
            # Replace == with -z
            gsub(/if \[ "\$\{udc_dev\}" == "" \]/, "if [ -z \"${udc_dev}\" ]")
            print
            next
        }
    }
}
{print}
' "$FILE" > "$FILE.tmp" && mv "$FILE.tmp" "$FILE"
