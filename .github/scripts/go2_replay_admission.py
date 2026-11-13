"""Admission for an explicitly confirmed disposable VM, never VM creation."""
import os
from pathlib import Path
import re
import subprocess
import sys

VM_KINDS = {'kvm', 'qemu', 'vmware', 'oracle', 'microsoft', 'xen', 'parallels'}
UUID = re.compile(r'[0-9a-f]{8}(?:-[0-9a-f]{4}){3}-[0-9a-f]{12}')


def admit():
    if not sys.platform.startswith('linux') or os.geteuid() != 0:
        raise ValueError('requires root inside the reserved disposable Linux VM or hosted runner')
    if os.environ.get('GITHUB_ACTIONS') == 'true' and os.environ.get('RUNNER_ENVIRONMENT') == 'github-hosted':
        return {'kind': 'github-hosted'}
    expected = [os.environ.get('WENDY_GO2_VM_BOOT_ID', '').lower(), os.environ.get('WENDY_GO2_VM_PRODUCT_UUID', '').lower()]
    if any(not UUID.fullmatch(value) or set(value.replace('-', '')) in ({'0'}, {'f'}) for value in expected) or expected[0] == expected[1]:
        raise ValueError('explicit distinct current VM boot_id and product_uuid confirmations are required')
    actual = [Path('/proc/sys/kernel/random/boot_id').read_text().strip().lower(), Path('/sys/class/dmi/id/product_uuid').read_text().strip().lower()]
    if actual != expected:
        raise ValueError('confirmed VM identity changed or differs from this host')
    container = subprocess.run(['systemd-detect-virt', '--container'], capture_output=True, text=True, timeout=5)
    if container.returncode != 1 or container.stdout.strip() != 'none':
        raise ValueError('container, unknown or nested execution is not an operator VM admission')
    vm = subprocess.run(['systemd-detect-virt', '--vm'], capture_output=True, text=True, timeout=5)
    if vm.returncode != 0 or vm.stdout.strip() not in VM_KINDS:
        raise ValueError('this is not a recognized disposable VM')
    return {'kind': vm.stdout.strip(), 'boot_id': actual[0], 'product_uuid': actual[1]}


if __name__ == '__main__':
    import json
    print(json.dumps(admit(), sort_keys=True))
