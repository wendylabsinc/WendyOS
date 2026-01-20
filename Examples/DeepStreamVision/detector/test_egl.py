#!/usr/bin/env python3
import os

# Set environment variables BEFORE importing GStreamer
os.environ['EGL_PLATFORM'] = 'device'
os.environ['LD_LIBRARY_PATH'] = '/opt/nvidia/deepstream/deepstream-7.1/lib:/usr/lib/aarch64-linux-gnu/gstreamer-1.0/deepstream:/usr/lib/aarch64-linux-gnu:/usr/lib'
os.environ['GST_PLUGIN_PATH'] = '/usr/lib/aarch64-linux-gnu/gstreamer-1.0/deepstream:/usr/lib/aarch64-linux-gnu/gstreamer-1.0'
os.environ['GST_DEBUG'] = '2'

print("Testing EGL/GPU access...")
print(f"EGL_PLATFORM={os.environ.get('EGL_PLATFORM')}")

# Check devices
import subprocess
result = subprocess.run(['ls', '-la', '/dev/dri/'], capture_output=True, text=True)
print("\n/dev/dri/ contents:")
print(result.stdout if result.returncode == 0 else f"Error: {result.stderr}")

result = subprocess.run(['ls', '-la', '/dev/nvidia*'], capture_output=True, text=True, shell=True)
print("\n/dev/nvidia* devices:")
print(result.stdout if result.returncode == 0 else f"Error: {result.stderr}")

# Test GStreamer
import gi
gi.require_version('Gst', '1.0')
from gi.repository import Gst
Gst.init(None)

print("\nTesting GStreamer element creation:")
elements = ['nvstreammux', 'nvinferserver', 'nvvideoconvert', 'nvdsosd', 'fakesink']
for elem_name in elements:
    elem = Gst.ElementFactory.make(elem_name, f"test-{elem_name}")
    if elem:
        print(f"✓ {elem_name} created successfully")
    else:
        print(f"✗ {elem_name} FAILED to create")

print("\nChecking library paths:")
print(f"LD_LIBRARY_PATH={os.environ.get('LD_LIBRARY_PATH', 'NOT SET')}")
print(f"GST_PLUGIN_PATH={os.environ.get('GST_PLUGIN_PATH', 'NOT SET')}")
print(f"PATH={os.environ.get('PATH', 'NOT SET')}")
print(f"PYTHONUNBUFFERED={os.environ.get('PYTHONUNBUFFERED', 'NOT SET')}")
print(f"EGL_PLATFORM={os.environ.get('EGL_PLATFORM', 'NOT SET')}")
print(f"GST_DEBUG={os.environ.get('GST_DEBUG', 'NOT SET')}")

print("\nAll environment variables:")
for key, value in sorted(os.environ.items()):
    if 'PATH' in key or 'GST' in key or 'LD_' in key or 'EGL' in key:
        print(f"{key}={value}")
