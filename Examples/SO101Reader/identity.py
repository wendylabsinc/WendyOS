"""Best-effort USB adapter metadata, never treated as robot identity proof."""

import os
from pathlib import Path
import plistlib
import subprocess
import sys


def adapter_metadata(port):
    result = {"source": "unavailable", "robot_identity_proven": False}
    if sys.platform == "linux":
        path = Path("/sys/class/tty") / os.path.basename(os.path.realpath(port)) / "device"
        for parent in [path.resolve(), *path.resolve().parents]:
            if (parent / "idVendor").exists():
                result["source"] = "linux_sysfs"
                for field, file in {"vendor_id": "idVendor", "product_id": "idProduct", "serial_number": "serial"}.items():
                    try:
                        result[field] = (parent / file).read_text().strip()
                    except OSError:
                        pass
                break
    elif sys.platform == "darwin":
        try:
            # USB device subtrees retain the serial client's ancestors without
            # serializing unrelated (and potentially very large) GPU services.
            raw = subprocess.run(["/usr/sbin/ioreg", "-a", "-l", "-r", "-c", "IOUSBHostDevice"],
                                 capture_output=True, check=True, timeout=2).stdout
            if len(raw) > 16_777_216:
                return result
            tree = plistlib.loads(raw)

            def walk(node, inherited):
                found = dict(inherited)
                for key, value in {"serial_number": node.get("USB Serial Number"),
                                   "vendor_id": node.get("idVendor"), "product_id": node.get("idProduct")}.items():
                    if value is not None:
                        found[key] = value
                if port in (node.get("IOCalloutDevice"), node.get("IODialinDevice")):
                    return found
                for child in node.get("IORegistryEntryChildren", []):
                    match = walk(child, found)
                    if match is not None:
                        return match
                return None

            for node in tree:
                found = walk(node, {})
                if found:
                    result.update(found, source="macos_ioreg")
                    break
        except (OSError, ValueError, subprocess.SubprocessError):
            pass
    return result
