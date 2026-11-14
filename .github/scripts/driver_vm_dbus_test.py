"""Execute guest bus setup/preflight with inert command children, no services."""
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
HELPER = ROOT/'.github/scripts/fixtures/driver-vm/dbus.sh'
STUB = r'''
import os,pathlib,subprocess,sys,time
name=pathlib.Path(sys.argv[0]).name; args=sys.argv[1:]
with open(os.environ['CALLS'],'a') as out: out.write(name+' '+repr(args)+'\n')
mode=os.environ.get('MODE','')
if name=='timeout':
    assert args[0]=='--kill-after=2s' and args[1] in ('5s','8s','15s'),args
    # Exercise real stalled-child termination at a reduced portable test cost.
    try:sys.exit(subprocess.run(args[2:],timeout=.3).returncode)
    except subprocess.TimeoutExpired:sys.exit(124)
if name=='systemctl' and args[0].startswith('--root='):
    root=pathlib.Path(args[0].split('=',1)[1])
    assert args[1:]==['enable','dbus-daemon.service'],args
    if mode=='enable-fails':sys.exit(1)
    if mode!='missing-alias':
        dest=root/'etc/systemd/system/dbus.service'
        target=root/'usr/lib/systemd/system'/('other.service' if mode=='wrong-alias' else 'dbus-daemon.service')
        dest.symlink_to(target)
elif name=='systemctl' and args==['start','dbus.service']:
    if mode=='hang':time.sleep(10)
    if mode=='start-fails':sys.exit(1)
elif name=='busctl':
    assert args[:3]==['--system','--timeout=5','call'],args
    if mode=='bus-fails' or (mode=='manager-fails' and args[3]=='org.freedesktop.systemd1'):sys.exit(1)
    print('s "owned-system-bus"' if args[-1]=='GetId' else '')
elif name not in ('journalctl','systemctl'):raise AssertionError(name)
'''


class DriverBus(unittest.TestCase):
    def run_case(self, mode, function):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp); binary=root/'bin'; binary.mkdir()
            for name in ('systemctl','busctl','journalctl','timeout'):
                path=binary/name;path.write_text('#!'+sys.executable+'\n'+STUB);path.chmod(0o700)
            image=root/'image'
            for path in ('etc/systemd/system','usr/lib/systemd/system'):(image/path).mkdir(parents=True)
            for name in ('dbus-daemon.service','other.service'):(image/'usr/lib/systemd/system'/name).write_text(name)
            env={**os.environ,'PATH':str(binary)+os.pathsep+os.environ['PATH'],'MODE':mode,'CALLS':str(root/'calls')}
            result=subprocess.run(['bash','-c','set -euo pipefail; source "$1"; if [[ $2 == setup ]]; then driver_vm_enable_bus "$3"; else driver_vm_require_bus; fi',
                                   'fixture',str(HELPER),function,str(image)],env=env,capture_output=True,text=True,timeout=8)
            return result,(root/'calls').read_text()

    def test_enables_exact_installed_provider_and_checks_alias(self):
        result,calls=self.run_case('','setup')
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertIn('DRIVER_VM_DBUS_PROVIDER=',result.stdout)
        self.assertIn("'enable', 'dbus-daemon.service'",calls)
        for mode in ('enable-fails','missing-alias','wrong-alias'):
            with self.subTest(mode=mode):
                result,_=self.run_case(mode,'setup');self.assertNotEqual(result.returncode,0)

    def test_requires_real_bus_and_systemd_rpc_success(self):
        result,calls=self.run_case('','boot')
        self.assertEqual(result.returncode,0,result.stderr)
        self.assertIn('DRIVER_VM_DBUS_READY:',result.stdout)
        self.assertIn("'org.freedesktop.DBus', 'GetId'",calls)
        self.assertIn("'org.freedesktop.DBus.Peer', 'Ping'",calls)
        self.assertNotIn('journalctl ',calls)

    def test_failure_or_stall_produces_bounded_diagnostics_without_ready(self):
        for mode in ('start-fails','bus-fails','manager-fails','hang'):
            with self.subTest(mode=mode):
                result,calls=self.run_case(mode,'boot')
                self.assertNotEqual(result.returncode,0)
                self.assertIn('DRIVER_VM_DBUS_PREFLIGHT_FAILED:',result.stderr)
                self.assertNotIn('DRIVER_VM_DBUS_READY:',result.stdout)
                self.assertIn('journalctl ',calls)
                self.assertIn("'cat', 'dbus.socket', 'dbus.service'",calls)

    def test_boot_admission_precedes_all_driver_mutations(self):
        source=(HELPER.parent/'run-guest.sh').read_text()
        self.assertLess(source.index('WendyDriverFixture-$nonce'),source.index('driver_vm_require_bus'))
        self.assertLess(source.index('trap finish EXIT'),source.index('driver_vm_require_bus'))
        self.assertLess(source.index('driver_vm_require_bus'),source.index('cp /fixture/omit-unload.sh'))
        self.assertLess(source.index('driver_vm_require_bus'),source.index('\n/usr/sbin/wendyos-sysext-apply.sh'))


if __name__=='__main__':unittest.main()
