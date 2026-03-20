#!/bin/bash
# fix-cdi-gstreamer-paths.sh
# Post-processes the CDI spec to fix path mappings for container compatibility
#
# Issue: Yocto installs GStreamer plugins to /usr/lib/gstreamer-1.0/deepstream/
# but containers (Ubuntu/Debian based) expect /usr/lib/aarch64-linux-gnu/gstreamer-1.0/deepstream/
#
# Solution: Modify the CDI spec to mount the Yocto path to the container's expected path

CDI_SPEC="/etc/cdi/nvidia.yaml"

if [ ! -f "$CDI_SPEC" ]; then
    echo "CDI spec not found at $CDI_SPEC"
    exit 1
fi

# Fix GStreamer plugin path: mount Yocto path to container's expected Ubuntu/Debian path
# Change: hostPath: /usr/lib/aarch64-linux-gnu/gstreamer-1.0/deepstream
# To:     hostPath: /usr/lib/gstreamer-1.0/deepstream
# This way the container sees plugins at the path it expects

if grep -q "aarch64-linux-gnu/gstreamer-1.0/deepstream" "$CDI_SPEC"; then
    sed -i 's|hostPath: /usr/lib/aarch64-linux-gnu/gstreamer-1.0/deepstream|hostPath: /usr/lib/gstreamer-1.0/deepstream|g' "$CDI_SPEC"
    echo "Fixed GStreamer plugin path mapping in CDI spec"
fi

# Add GST_PLUGIN_PATH environment variable if not present
# This tells GStreamer inside the container where to find DeepStream plugins
if ! grep -q "GST_PLUGIN_PATH=" "$CDI_SPEC"; then
    # Add GST_PLUGIN_PATH after NVIDIA_VISIBLE_DEVICES
    sed -i 's|NVIDIA_VISIBLE_DEVICES=void|NVIDIA_VISIBLE_DEVICES=void\n  - GST_PLUGIN_PATH=/usr/lib/aarch64-linux-gnu/gstreamer-1.0/deepstream|g' "$CDI_SPEC"
    echo "Added GST_PLUGIN_PATH to CDI spec"
fi

echo "CDI spec post-processing complete"
