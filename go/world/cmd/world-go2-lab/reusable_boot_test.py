"""Local filesystem/fake-child tests. No ROS, HTTP, Docker, workers or privileges."""
import contextlib
import hashlib
import io
import json
import os
from pathlib import Path
import shutil
import stat
import sys
import tempfile
import unittest
from unittest.mock import patch

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / 'deploy/go2-sim'))
import boot
import install
import release_boot
import run_bundle
import seal_release


class Child:
    def poll(self): return None


class Steps:
    def __init__(self): self.calls = 2
    def is_set(self): return self.calls <= 0
    def wait(self, _): self.calls -= 1; return self.is_set()


class ReusableTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(); self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name); self.app = self.root / 'image'; self.app.mkdir(mode=0o700)
        self.source = self.root / 'bundle'; self.source.mkdir(mode=0o700)
        self.volume = self.root / 'volume'; self.volume.mkdir(mode=0o700)
        self.image = 'sha256:'+'c'*64
        code = {}
        for name in run_bundle.CODE_FILES:
            path = self.app / name; path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
            raw = ('fixed code '+name).encode();path.write_bytes(raw)
            code[name] = {'bytes':len(raw),'sha256':run_bundle.sha(raw)}
        raw = json.dumps({'schema':'wendy.world.go2-release.v1','domain':'simulation','arch':'arm64','files':code}).encode()
        (self.app / 'release.json').write_bytes(raw);self.release = run_bundle.sha(raw)
        (self.app / 'world-go2-sensors').write_bytes(b'compiled native helper')
        (self.app / 'world-go2-cloud-live').write_bytes(b'compiled cloud helper')
        seal_release.seal(self.app)
        files = {}
        for name in run_bundle.BUNDLE_FILES:
            raw = ('private bytes '+name).encode();path = self.source / name;path.write_bytes(raw);path.chmod(0o600)
            files[name] = {'bytes':len(raw),'sha256':run_bundle.sha(raw)}
        self.cfg = {'schema':'wendy.world.go2-run.v1','domain':'simulation','release_id':self.release,
            'image_digest':self.image,'app_id':'com.wendy.world.go2','service':'mission',
            'run_id':'synthetic-run','task_id':'mission','gateway_port':18885,'bridge_port':18886,'files':files}
        raw = json.dumps(self.cfg).encode();(self.source / 'run.json').write_bytes(raw);self.bundle = run_bundle.sha(raw)
        self.validation = []
        def validate(path,*args):
            self.validation.append((path,args))
            # The actual signature/scope validator has separate Go tests. This
            # callback proves installer ordering and exact snapshot bytes only.
            self.assertEqual((path/'node.json').read_bytes(),(self.source/'node.json').read_bytes())
            return True
        self.validate = validate

    def install(self, **kwargs):
        return install.install(self.source,self.volume,self.bundle,self.release,self.image,
                               validate=kwargs.pop('validate',self.validate),app=self.app,**kwargs)

    def test_exclusive_complete_install_and_reporting_only_restart(self):
        self.install();self.assertEqual(set(p.name for p in self.volume.iterdir()),{'.install.lock','commission','installed.json'})
        cfg,captured = run_bundle.load_installed(self.volume,self.bundle,self.release,self.image)
        self.assertEqual(cfg,self.cfg)
        for name,raw in captured.items():
            self.assertEqual(raw,(self.source/name).read_bytes())
            self.assertEqual(stat.S_IMODE((self.volume/'commission'/name).stat().st_mode),0o600)
        with self.assertRaises(boot.BootError):self.install()
        data = self.volume/'world-go2'
        with patch.object(boot,'UID',os.getuid()):
            lock = boot.prepare_state(self.bundle,data,change_owner=False)
            self.assertTrue(boot.claim_launch(self.bundle,data));self.assertFalse(boot.claim_launch(self.bundle,data))
            os.close(lock)
            with self.assertRaises(boot.BootError):boot.prepare_state('f'*64,data,change_owner=False)
        self.assertTrue((data/'launch-once.json').exists())

    def test_partial_install_sync_failure_never_completes_or_retries(self):
        real = install.create_durable;count = 0
        def fail(path,raw):
            nonlocal count
            count += 1
            if count==2:raise OSError('injected fsync unavailable')
            return real(path,raw)
        with patch.object(install,'create_durable',side_effect=fail):
            with self.assertRaises(OSError):self.install()
        self.assertFalse((self.volume/'installed.json').exists())
        with self.assertRaises(boot.BootError):self.install()
        with self.assertRaises(FileNotFoundError):run_bundle.load_installed(self.volume,self.bundle,self.release,self.image)
        self.assertFalse((self.volume/'world-go2').exists())

    def test_parent_directory_sync_failure_consumes_install_attempt(self):
        with patch.object(install,'sync_directory',side_effect=OSError('no sync')):
            with self.assertRaises(OSError):self.install()
        self.assertTrue((self.volume/'.install.lock').exists())
        with self.assertRaises(boot.BootError):self.install()
        self.assertFalse((self.volume/'installed.json').exists())

    def test_final_commit_acknowledgment_can_be_ambiguous_not_a_success(self):
        # All bundle bytes were durable before this injected post-commit error.
        # A marker is not evidence of observed installer exit0; deployment must
        # retain the failed outcome and must not retry or start motion from it.
        real=install.create_durable
        def fail_after_final_write(path,raw):
            real(path,raw)
            if path.name=='installed.json':raise OSError('final acknowledgment unavailable')
        with patch.object(install,'create_durable',side_effect=fail_after_final_write):
            with self.assertRaises(OSError):self.install()
        cfg,_=run_bundle.load_installed(self.volume,self.bundle,self.release,self.image)
        self.assertEqual(cfg,self.cfg)
        with self.assertRaises(boot.BootError):self.install()
        self.assertFalse((self.volume/'world-go2').exists())

    def test_existing_state_and_symlink_volumes_refuse_without_mutation(self):
        (self.volume/'old-task').write_bytes(b'retained')
        with self.assertRaises(boot.BootError):self.install()
        self.assertEqual((self.volume/'old-task').read_bytes(),b'retained')
        alias = self.root/'link';alias.symlink_to(self.volume,target_is_directory=True)
        with self.assertRaises(boot.BootError):install.install(self.source,alias,self.bundle,self.release,self.image,validate=self.validate,app=self.app)

    def test_invalid_release_or_bundle_never_starts_installation(self):
        for target in [self.app/'worldd',self.app/'world-go2-sensors',self.app/'world-go2-cloud-live',self.source/'node.json']:
            old = target.read_bytes();target.write_bytes(b'changed')
            with self.assertRaises(boot.BootError):self.install()
            self.assertEqual(list(self.volume.iterdir()),[]);target.write_bytes(old)
        node = self.source/'node.json';old = node.read_bytes();node.unlink();node.symlink_to(self.app/'worldd')
        with self.assertRaises(boot.BootError):self.install()
        node.unlink();node.write_bytes(old);node.chmod(0o600)
        self.assertEqual(list(self.volume.iterdir()),[])

    def test_expired_or_invalid_grants_refuse_before_volume_write(self):
        with self.assertRaises(boot.BootError):self.install(validate=lambda *_:False)
        self.assertEqual(list(self.volume.iterdir()),[])

    def test_exact_snapshot_does_not_reload_mutated_volume(self):
        self.install();cfg,captured = run_bundle.load_installed(self.volume,self.bundle,self.release,self.image)
        snapshot = run_bundle.private_snapshot(captured,parent=self.root)
        (self.volume/'commission/node.json').write_bytes(b'changed after capture')
        self.assertEqual((snapshot/'node.json').read_bytes(),captured['node.json'])
        with self.assertRaises(boot.BootError):run_bundle.load_installed(self.volume,self.bundle,self.release,self.image)
        shutil.rmtree(snapshot)

    def test_fixed_validator_argv_and_sanitized_failure(self):
        import subprocess
        with patch.object(run_bundle.subprocess,'run',side_effect=subprocess.TimeoutExpired('secret',5)) as run:
            self.assertFalse(run_bundle.validate_snapshot(self.source,self.bundle,self.release,self.image,True,app=self.app))
        call=run.call_args;self.assertEqual(call.args[0][0],str(self.app/'world-go2-lab'))
        self.assertEqual(call.args[0][-1],'--current');self.assertEqual(call.kwargs['timeout'],5)
        self.assertEqual(call.kwargs['stdout'],subprocess.DEVNULL);self.assertEqual(call.kwargs['stderr'],subprocess.DEVNULL)
        self.assertNotIn('private bytes',' '.join(call.args[0]))

    def test_service_selectors_are_exact_and_not_custody(self):
        env={'WENDY_APP_ID':'com.wendy.world.go2','WENDY_APP_GROUP':'com.wendy.world.go2','WENDY_HOSTNAME':'mission.local',
            'WORLD_GO2_BUNDLE_SHA256':self.bundle,'WORLD_GO2_RELEASE_ID':self.release,'WORLD_GO2_IMAGE_DIGEST':self.image}
        self.assertEqual(release_boot.identity(env),(self.bundle,self.release,self.image))
        for key in env:
            changed=dict(env);changed[key]='wrong'
            with self.assertRaises(boot.BootError):release_boot.identity(changed)

    def test_late_current_refusal_consumes_launch_and_cannot_rearm(self):
        data=self.root/'state';data.mkdir(mode=0o700);spawned=[];stopped=[];events=[]
        spawn=lambda argv:(spawned.append(argv) or Child())
        def current():
            self.assertTrue((data/'launch-once.json').exists());return False
        with patch.object(boot,'emit',side_effect=events.append):
            boot.supervise(self.cfg,self.bundle,Steps(),data=data,spawn=spawn,ready=lambda *_:True,stop=stopped.append,config_dir=self.source,can_launch=current)
            self.assertEqual(len(spawned),1);self.assertIn('motion_configuration_refused_reporting_only',events)
            boot.supervise(self.cfg,self.bundle,Steps(),data=data,spawn=spawn,ready=lambda *_:True,stop=stopped.append,config_dir=self.source,can_launch=lambda:True)
        self.assertEqual(len(spawned),2);self.assertIn('restart_reporting_only',events)
        self.assertEqual(spawned[0],[str(boot.APP/'worldd'),'-config',str(self.source/'node.json')])

    def test_worker_inputs_are_only_private_snapshot_and_fixed_code(self):
        data=self.root/'state';data.mkdir(mode=0o700);spawned=[]
        with patch.object(boot,'emit'):
            boot.supervise(self.cfg,self.bundle,Steps(),data=data,spawn=lambda argv:(spawned.append(argv) or Child()),ready=lambda *_:True,stop=lambda _:None,config_dir=self.source)
        self.assertEqual(len(spawned),3)
        for argv in spawned:
            self.assertNotIn(str(self.volume),' '.join(argv))
            for name in ['node.json','executor.json','binding.json','alignment.json','profile.json','bridge-token']:
                for arg in argv:
                    if arg.endswith('/'+name):self.assertEqual(arg,str(self.source/name))
        self.assertEqual(spawned[0][0],str(boot.APP/'worldd'));self.assertEqual(spawned[2][0],str(boot.APP/'world-go2-executor'))

    def test_cancel_during_validation_cannot_launch_bridge(self):
        import threading
        closing=threading.Event();data=self.root/'state';data.mkdir(mode=0o700);spawned=[]
        def validate():closing.set();return True
        with patch.object(boot,'emit'):
            boot.supervise(self.cfg,self.bundle,closing,data=data,spawn=lambda argv:(spawned.append(argv) or Child()),ready=lambda *_:True,stop=lambda _:None,can_launch=validate)
        self.assertEqual(len(spawned),1);self.assertTrue((data/'launch-once.json').exists())

    def test_cancel_after_bridge_readiness_cannot_launch_executor(self):
        import threading
        closing=threading.Event();data=self.root/'state';data.mkdir(mode=0o700);spawned=[]
        def ready(*_):
            if len(spawned)==2:closing.set()
            return True
        with patch.object(boot,'emit'):
            boot.supervise(self.cfg,self.bundle,closing,data=data,spawn=lambda argv:(spawned.append(argv) or Child()),ready=ready,stop=lambda _:None)
        self.assertEqual(len(spawned),2);self.assertTrue((data/'launch-once.json').exists())

    def test_installer_main_never_logs_raw_exception(self):
        stream=io.StringIO()
        with patch.object(sys,'argv',['install','--bundle-sha256',self.bundle,'--release-id',self.release,'--image-digest',self.image]),patch.object(install.os,'geteuid',return_value=0),patch.object(install,'install',side_effect=boot.BootError('private bytes bridge-token')),contextlib.redirect_stdout(stream):
            self.assertEqual(install.main(),1)
        event=json.loads(stream.getvalue());self.assertEqual(event['stage'],'installation_refused');self.assertNotIn('private',stream.getvalue())


if __name__=='__main__':
    unittest.main(verbosity=2)
