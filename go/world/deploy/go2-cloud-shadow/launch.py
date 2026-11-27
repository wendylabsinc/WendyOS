"""Enforce the passive app's UID even when an operator exec starts as root."""
import os
import runpy
import signal
import sys


def drop_privileges():
    if os.geteuid() == 0:
        os.setgroups([])
        os.setgid(65532)
        os.setuid(65532)
    if os.getuid() != 65532 or os.geteuid() != 65532 or os.getgid() != 65532 or os.getegid() != 65532:
        raise RuntimeError('passive_observer_identity_required')
    if any(g != 65532 for g in os.getgroups()):
        raise RuntimeError('passive_observer_groups_required')


def main():
    if len(sys.argv) < 2 or sys.argv[1] not in ('idle', 'capture', 'export'):
        raise ValueError('explicit_passive_operation_required')
    operation = sys.argv[1]
    if operation == 'idle' and len(sys.argv) != 2:
        raise ValueError('idle_arguments')
    drop_privileges()
    if operation == 'idle':
        signal.pause()
    else:
        sys.argv = [sys.argv[0], *sys.argv[2:]]
        if operation == 'capture':
            runpy.run_module('sim.go2_cloud_shadow', run_name='__main__')
        else:
            runpy.run_path('/opt/world/cloud-shadow-export.py', run_name='__main__')


if __name__ == '__main__':
    main()
