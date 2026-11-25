"""Bounded runner tests; no Docker or scene runtime is launched."""
import base64
import hashlib
import io
import json
import os
from pathlib import Path
import subprocess
import signal
import shutil
import stat
import sys
import tempfile
import tarfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

sys.path.insert(0,str(Path(__file__).resolve().parent))
import configs
import run
import stage


def small_stage(root):
    source=root/'source';source.mkdir(mode=0o700)
    path=source/'project/services/example/Dockerfile';path.parent.mkdir(parents=True)
    path.write_bytes(b'FROM scratch\n');path.chmod(0o644)
    index={'version':1,'files':{'project/services/example/Dockerfile':
           {'sha256':hashlib.sha256(path.read_bytes()).hexdigest(),'size':path.stat().st_size,'mode':0o644}}}
    stage.write_new(source/'files.json',stage.json_bytes(index))
    return source


def archive_bytes(source,explicit=True):
    raw,files,directories=run.upload_inventory(source);output=io.BytesIO()
    with tarfile.open(fileobj=output,mode='w') as archive:
        run.write_stage_archive(archive,source,raw,files,directories if explicit else [])
    return raw,output.getvalue()


def gnu_tar():
    command=shutil.which('gtar') or shutil.which('tar')
    if command and b'GNU tar' in subprocess.run([command,'--version'],capture_output=True,timeout=5).stdout:
        return command
    raise unittest.SkipTest('GNU tar is needed for the actual extraction regression')


class StageUploadTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory(prefix='world-stage-upload-');self.addCleanup(self.temp.cleanup)
        self.root=Path(self.temp.name).resolve();self.root.chmod(0o700)
        self.source=small_stage(self.root)

    def test_pinned_ancestor_headers_precede_files_and_ignore_source_modes(self):
        (self.source/'project').chmod(0o777)
        (self.source/'unused-empty-directory').mkdir(mode=0o777)
        _,data=archive_bytes(self.source)
        with tarfile.open(fileobj=io.BytesIO(data)) as archive:
            entries=archive.getmembers()
        self.assertEqual([m.name for m in entries],['project','project/services','project/services/example',
                         'files.json','project/services/example/Dockerfile'])
        self.assertTrue(all(m.isdir() and m.mode==0o755 and m.uid==m.gid==0 for m in entries[:3]))
        self.assertTrue(all(m.isfile() for m in entries[3:]))

    def test_upload_inventory_rejects_unpinned_files_links_and_parent_collisions(self):
        path=self.source/'unexpected';path.write_text('extra')
        with self.assertRaises(stage.Refused):run.upload_inventory(self.source)
        path.unlink();path.symlink_to('files.json')
        with self.assertRaises(stage.Refused):run.upload_inventory(self.source)
        path.unlink()
        index=json.loads((self.source/'files.json').read_bytes());original=index['files']
        for name in ('project','project//bad','project/./bad','../bad','files.json/child'):
            with self.subTest(name=name):
                index['files']={**original,name:next(iter(original.values()))}
                (self.source/'files.json').write_bytes(stage.json_bytes(index))
                with self.assertRaises(stage.Refused):run.upload_inventory(self.source)

    def test_source_file_metadata_change_is_refused_during_stream(self):
        raw,files,directories=run.upload_inventory(self.source)
        (self.source/'project/services/example/Dockerfile').chmod(0o666)
        with tarfile.open(fileobj=io.BytesIO(),mode='w') as archive:
            with self.assertRaises(stage.Refused):run.write_stage_archive(archive,self.source,raw,files,directories)

    def test_actual_gnu_tar_umask_zero_requires_explicit_parent_entries(self):
        tar=gnu_tar()
        for explicit,wanted in ((False,0o777),(True,0o755)):
            with self.subTest(explicit=explicit):
                target=self.root/str(explicit);target.mkdir(mode=0o700)
                _,data=archive_bytes(self.source,explicit)
                subprocess.run([tar,'--no-same-owner','-xf','-','-C',str(target)],input=data,
                               check=True,capture_output=True,timeout=10,umask=0)
                for name in ('project','project/services','project/services/example'):
                    info=(target/name).stat()
                    self.assertEqual(stat.S_IMODE(info.st_mode),wanted)
                    self.assertEqual(info.st_uid,os.geteuid())

    def test_mock_upload_creates_fresh_root_then_streams_and_verifies_pinned_index(self):
        runner=run.Runner.__new__(run.Runner);runner.source=self.source;runner.out=self.root/'receipts'
        runner.out.mkdir();runner.name='owned';runner.count=0
        events=[];captured=[]
        class Pipe(io.BytesIO):
            def close(self):captured.append(self.getvalue());super().close()
        class Process:
            def __init__(self,*args,**kwargs):
                events.append('tar');self.stdin=Pipe();self.returncode=None
            def wait(self,**kwargs):self.returncode=0;return 0
            def poll(self):return self.returncode
            def kill(self):self.returncode=-9
        def python(script,*args):events.append((script,args))
        with patch.object(runner,'python',side_effect=python),patch.object(run.subprocess,'Popen',Process):
            runner.upload()
        self.assertEqual(events[0],(run.CREATE_STAGE,(run.REMOTE,)))
        self.assertEqual(events[1],'tar')
        self.assertEqual(events[2],(run.VERIFY_STAGE,(run.REMOTE,hashlib.sha256((self.source/'files.json').read_bytes()).hexdigest())))
        with tarfile.open(fileobj=io.BytesIO(captured[0])) as archive:
            self.assertTrue(archive.getmembers()[0].isdir())
        with patch.object(runner,'python',side_effect=stage.Refused('unsafe existing root')),patch.object(run.subprocess,'Popen') as popen:
            with self.assertRaises(stage.Refused):runner.upload()
            popen.assert_not_called()


@unittest.skipUnless(sys.platform=='linux' and os.geteuid()==0 and not os.stat(tempfile.gettempdir()).st_mode&0o022,
                     'requires isolated Linux root with a protected temporary filesystem')
class StageReceiverLinuxTests(unittest.TestCase):
    """Execute the exact receiver scripts on real fds; no Docker or agent here."""
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory(prefix='world-stage-receiver-');self.addCleanup(self.temp.cleanup)
        self.root=Path(self.temp.name).resolve();self.root.chmod(0o700)
        self.source=small_stage(self.root)

    def script(self,script,path,*args,data=None):
        return subprocess.run([sys.executable,'-c',script,str(path),*map(str,args)],input=data,
                              capture_output=True,timeout=10)

    def extract(self,name,explicit=True):
        target=self.root/name
        self.assertEqual(self.script(run.CREATE_STAGE,target).returncode,0)
        raw,data=archive_bytes(self.source,explicit)
        subprocess.run([gnu_tar(),'--no-same-owner','-xf','-','-C',str(target)],input=data,
                       capture_output=True,check=True,timeout=10,umask=0)
        return target,hashlib.sha256(raw).hexdigest()

    def test_exact_guard_accepts_explicit_modes_and_preserves_history_receiver(self):
        import history_runtime
        for explicit in (False,True):
            target,digest=self.extract(str(explicit),explicit)
            result=self.script(run.VERIFY_STAGE,target,digest)
            self.assertEqual(result.returncode==0,explicit)
            context=target/'project/services/proof-reader';context.mkdir(mode=0o755)
            data=b'FROM scratch\n'
            result=self.script(history_runtime.WRITE_CHUNK,context/'Dockerfile',0,len(data),
                               hashlib.sha256(data).hexdigest(),0o644,data=data)
            self.assertEqual(result.returncode==0,explicit)

    def test_fresh_root_refuses_existing_symlink_or_writable_parent_without_repair(self):
        existing=self.root/'existing';existing.mkdir(mode=0o777);existing.chmod(0o777)
        self.assertNotEqual(self.script(run.CREATE_STAGE,existing).returncode,0)
        self.assertEqual(stat.S_IMODE(existing.stat().st_mode),0o777)
        link=self.root/'link';link.symlink_to(existing,target_is_directory=True)
        self.assertNotEqual(self.script(run.CREATE_STAGE,link).returncode,0)
        self.assertNotEqual(self.script(run.CREATE_STAGE,existing/'child').returncode,0)
        self.assertFalse((existing/'child').exists())

    def test_verifier_refuses_unsafe_metadata_links_membership_and_changed_bytes(self):
        for fault in ('root-mode','parent-mode','file-mode','symlink-dir','symlink-file','hardlink',
                      'extra-dir','extra-file','bytes','size','index'):
            with self.subTest(fault=fault):
                target,digest=self.extract(fault);file=target/'project/services/example/Dockerfile'
                if fault=='root-mode':target.chmod(0o777)
                elif fault=='parent-mode':file.parent.chmod(0o777)
                elif fault=='file-mode':file.chmod(0o666)
                elif fault=='symlink-dir':
                    directory=file.parent;directory.rename(target/'moved');directory.symlink_to(target/'moved',target_is_directory=True)
                elif fault=='symlink-file':file.unlink();file.symlink_to(target/'files.json')
                elif fault=='hardlink':os.link(file,target/'hardlink')
                elif fault=='extra-dir':(target/'extra').mkdir()
                elif fault=='extra-file':(target/'extra').write_text('extra')
                elif fault=='bytes':file.write_bytes(b'FROM changed\n')
                elif fault=='size':file.write_bytes(b'changed size')
                elif fault=='index':(target/'files.json').write_bytes(b'{}')
                self.assertNotEqual(self.script(run.VERIFY_STAGE,target,digest).returncode,0)


class RunnerTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory(prefix="world-mm-runner-");self.addCleanup(self.tmp.cleanup)
        self.runner=run.Runner.__new__(run.Runner)
        self.runner.out=Path(self.tmp.name);self.runner.count=0;self.runner.name="owned-run";self.runner.network="owned-net";self.runner.volume="owned-volume";self.runner.created=[];self.runner.args=SimpleNamespace(keep=False);self.runner.plan={"run_id":"test-run"}
    def test_history_is_explicit_and_all_pins_are_required(self):
        self.assertIsNone(run.history_options(SimpleNamespace()))
        values=dict(history_bundle=Path('/private/bundle'),history_reader_sha256='a'*64,history_plan_sha256='b'*64,history_inventory_sha256='c'*64)
        self.assertEqual(run.history_options(SimpleNamespace(**values)),values)
        for key in values:
            with self.subTest(missing=key),self.assertRaises(stage.Refused):
                run.history_options(SimpleNamespace(**{k:v for k,v in values.items() if k!=key}))
            with self.subTest(empty=key),self.assertRaises(stage.Refused):
                run.history_options(SimpleNamespace(**{key:''}))
        for bad in ('A'*64,'a'*63,'a'*64+'\n'):
            with self.subTest(pin=bad),self.assertRaises(stage.Refused):
                run.history_options(SimpleNamespace(**{**values,'history_reader_sha256':bad}))
    def test_rpc_timings_are_explicit_per_agent_start(self):
        for enabled in (False,True):
            self.runner.args.trace_rpc=enabled
            with patch.object(self.runner,'inside'),patch.object(self.runner,'wait'),patch.object(self.runner,'call') as call:
                self.runner.start_agent('world-active')
            argv=call.call_args.args[0]
            self.assertEqual('WENDY_WORLD_RPC_TIMING=1' in argv,enabled)
            self.assertEqual(argv[-1],'world-active')
            self.assertIn('WENDY_CONFIG_PATH=/etc/wendy-agent',argv)
            self.assertIn('WENDY_CONTAINERD_ADDR=/run/containerd/containerd.sock',argv)
    def test_runtime_keeps_existing_bounded_namespace_and_capability_scope(self):
        args=run.runtime_args("sha256:"+"a"*64,"owned-run","owned-net","owned-volume")
        self.assertIn("--cgroupns=private",args);self.assertIn("--read-only",args)
        self.assertEqual(args[args.index("--network")+1],"owned-net")
        self.assertEqual(args[args.index("--mount")+1],"type=volume,src=owned-volume,dst=/var/lib")
        self.assertNotIn("--privileged",args);self.assertNotIn("SYS_TIME",args)
        self.assertFalse(any("docker.sock" in arg or "type=bind" in arg or arg in ("--pid=host","--network=host") for arg in args))
        self.assertIn("SYS_CHROOT",args);self.assertEqual(args[-1],"sha256:"+"a"*64)
    def test_local_subprocess_timeout_retains_bounded_failure_receipt(self):
        result=self.runner.call([sys.executable,"-c","import time; print('retained',flush=True); time.sleep(10)"],timeout=.1,check=False)
        self.assertEqual(result.returncode,124);self.assertIn(b"retained",result.stdout)
        receipt=json.loads((self.runner.out/"001.json").read_bytes());self.assertTrue(receipt["timeout"])
    def test_log_quota_is_explicit_failure(self):
        with patch.object(run,"MAX_LOG",32):
            result=self.runner.call([sys.executable,"-c","print('x'*1000)"],check=False)
        self.assertEqual(result.returncode,125);self.assertEqual(len(result.stdout),32)
        self.assertTrue(json.loads((self.runner.out/"001.json").read_bytes())["output_limited"])
    def cleanup(self,owned,failed_remove=None,daemon_failure=False):
        commands=[];remaining={"container","volume","network"}
        def call(argv,**kwargs):
            commands.append(argv)
            owner=self.runner.name if owned else "foreign-run"
            if "inspect" in argv:
                if daemon_failure:return subprocess.CompletedProcess(argv,1,b"",b"daemon unavailable")
                if argv[1] not in remaining:return subprocess.CompletedProcess(argv,1,b"",b"not found")
                data=[{"Id":"a"*64,"Labels":{run.LABEL:owner},"Config":{"Labels":{run.LABEL:owner}},"State":{"Running":False,"Pid":0}}]
                return subprocess.CompletedProcess(argv,0,json.dumps(data).encode(),b"")
            if "ls" in argv:return subprocess.CompletedProcess(argv,1 if daemon_failure else 0,b"",b"")
            if "rm" in argv:
                kind="container" if argv[1]=="rm" else argv[1]
                if kind==failed_remove:return subprocess.CompletedProcess(argv,1,b"",b"remove failed")
                remaining.discard(kind)
            return subprocess.CompletedProcess(argv,124 if argv[1] in ("logs","cp") else 0,b"",b"")
        self.runner.created=["network","volume","container"]
        with patch.object(self.runner,"call",side_effect=call):receipt=self.runner.finish()
        return commands,receipt
    def test_log_failure_does_not_skip_owned_cleanup(self):
        commands,receipt=self.cleanup(True)
        self.assertIn(["docker","rm","-f","a"*64],commands)
        self.assertIn(["docker","volume","rm","owned-volume"],commands)
        self.assertIn(["docker","network","rm","owned-net"],commands)
        self.assertTrue(receipt["complete"])
        self.assertTrue(all(item["state"]=="absent" for item in receipt["resources"].values()))
    def test_foreign_resources_are_neither_read_nor_removed(self):
        commands,receipt=self.cleanup(False)
        self.assertFalse(any("rm" in command or command[1] in ("logs","cp") for command in commands))
        self.assertFalse(receipt["complete"])
    def test_failed_removal_cannot_claim_cleanup_and_other_removals_continue(self):
        commands,receipt=self.cleanup(True,failed_remove="container")
        self.assertFalse(receipt["complete"]);self.assertEqual(receipt["resources"]["container"]["state"],"owned")
        self.assertIn(["docker","volume","rm","owned-volume"],commands)
        self.assertIn(["docker","network","rm","owned-net"],commands)
    def test_unavailable_daemon_is_unknown_not_absence(self):
        commands,receipt=self.cleanup(True,daemon_failure=True)
        self.assertFalse(receipt["complete"])
        self.assertTrue(all(item["state"]=="unknown" for item in receipt["resources"].values()))
        self.assertFalse(any("rm" in command for command in commands))
    def test_explicit_keep_records_owned_resources_retained(self):
        self.runner.args.keep=True
        commands,receipt=self.cleanup(True)
        self.assertTrue(receipt["complete"])
        self.assertTrue(all(item["state"]=="retained" for item in receipt["resources"].values()))
        self.assertFalse(any("rm" in command for command in commands))
        self.assertFalse(any("stop" in command for command in commands))

    def test_database_snapshot_requires_stopped_owned_runtime(self):
        for name,running,pid,owner,expected in (
            ("stopped",False,0,"owned-run",True),
            ("running",True,9,"owned-run",False),
            ("pid_present",False,9,"owned-run",False),
            ("pid_missing",False,None,"owned-run",False),
            ("foreign",False,0,"foreign-run",False),
        ):
            with self.subTest(name=name):
                self.runner.out=Path(self.tmp.name)/name;self.runner.out.mkdir()
                commands=[]
                def call(argv,**kwargs):
                    commands.append(argv)
                    value=[{"Id":"a"*64,"Config":{"Labels":{run.LABEL:owner}},"State":{"Running":running,"Pid":pid}}]
                    return subprocess.CompletedProcess(argv,0,json.dumps(value).encode(),b"")
                with patch.object(self.runner,"call",side_effect=call),patch.object(self.runner,"snapshot_file",return_value={"status":"captured"}) as snapshot:
                    receipt=self.runner.snapshot_stopped_state()
                self.assertEqual(receipt["writers_stopped"],expected)
                self.assertEqual(snapshot.call_count,2 if expected else 0)
                self.assertEqual(commands[0],["docker","container","inspect","owned-run"])
                if owner=="owned-run":self.assertEqual(commands[1],["docker","stop","--time","10","a"*64])
                else:self.assertFalse(any("stop" in command for command in commands))
                if expected:
                    self.assertEqual({args.args[0] for args in snapshot.call_args_list},{"/var/lib/wendy/world/journal/journal.db","/var/lib/wendy/world/archive/proofs.db"})
                    self.assertTrue(all(args.args[2]==16<<20 for args in snapshot.call_args_list))
                    self.assertTrue(all(args.kwargs["container"]=="a"*64 for args in snapshot.call_args_list))

    def test_failed_state_snapshot_cannot_prevent_cleanup(self):
        with patch.object(self.runner,"snapshot_stopped_state",side_effect=OSError("diagnostic unavailable")):
            commands,receipt=self.cleanup(True)
        self.assertTrue(receipt["complete"])
        self.assertIn({"step":"stopped_state","error":"OSError"},receipt["evidence_errors"])
        self.assertIn(["docker","volume","rm","owned-volume"],commands)

    def test_foreign_name_replacement_during_snapshots_is_not_removed(self):
        commands=[];replaced=[False]
        def call(argv,**kwargs):
            commands.append(argv)
            if "inspect" in argv:
                owner="foreign" if replaced[0] else self.runner.name
                value=[{"Id":"b"*64 if replaced[0] else "a"*64,"Config":{"Labels":{run.LABEL:owner}},"State":{"Running":True,"Pid":9}}]
                return subprocess.CompletedProcess(argv,0,json.dumps(value).encode(),b"")
            return subprocess.CompletedProcess(argv,0,b"",b"")
        def changed():replaced[0]=True;return {"writers_stopped":False}
        self.runner.created=["container"]
        with patch.object(self.runner,"call",side_effect=call),patch.object(self.runner,"snapshot_outputs"),patch.object(self.runner,"snapshot_file"),patch.object(self.runner,"snapshot_stopped_state",side_effect=changed):
            receipt=self.runner.finish()
        self.assertFalse(any("rm" in argv for argv in commands))
        self.assertFalse(receipt["complete"])
        self.assertEqual(receipt["resources"]["container"]["state"],"foreign")

    def test_unknown_removal_identity_does_not_skip_other_resources(self):
        self.runner.created=["container","volume","network"]
        commands=[]
        states=iter(["owned",OSError("inspection unavailable"),"owned","absent","owned","absent"])
        def state(*args):
            value=next(states)
            if isinstance(value,Exception):raise value
            return value
        def call(argv,**kwargs):
            commands.append(argv)
            return subprocess.CompletedProcess(argv,1 if "inspect" in argv else 0,b"",b"")
        with patch.object(self.runner,"call",side_effect=call),patch.object(self.runner,"resource_state",side_effect=state),patch.object(self.runner,"snapshot_outputs"),patch.object(self.runner,"snapshot_file"),patch.object(self.runner,"snapshot_stopped_state",return_value={}):
            receipt=self.runner.finish()
        self.assertEqual(receipt["resources"]["container"]["state"],"unknown")
        self.assertFalse(receipt["complete"])
        self.assertIn(["docker","volume","rm","owned-volume"],commands)
        self.assertIn(["docker","network","rm","owned-net"],commands)
    def test_final_result_waits_for_verified_cleanup(self):
        with patch.object(self.runner,"run",return_value={"passed":True}),patch.object(self.runner,"finish",return_value={"complete":False}):
            with self.assertRaises(stage.Refused):self.runner.execute()
        result=json.loads((self.runner.out/"result.json").read_bytes())
        self.assertTrue(result["qualification_passed"]);self.assertFalse(result["passed"])
    def test_cleanup_exception_does_not_mask_original_failure(self):
        original=stage.Refused("qualification failed first")
        with patch.object(self.runner,"run",side_effect=original),patch.object(self.runner,"finish",side_effect=OSError("cleanup failed")):
            with self.assertRaises(stage.Refused) as caught:self.runner.execute()
        self.assertIs(caught.exception,original)
        result=json.loads((self.runner.out/"result.json").read_bytes())
        self.assertEqual(result["error"],"qualification failed first");self.assertEqual(result["cleanup"]["error_type"],"OSError")
    def test_sigterm_finishes_owned_cleanup_and_records_interruption(self):
        previous=signal.getsignal(signal.SIGTERM)
        def interrupted():os.kill(os.getpid(),signal.SIGTERM)
        with patch.object(self.runner,"run",side_effect=interrupted),patch.object(self.runner,"finish",return_value={"complete":True}) as finish:
            with self.assertRaises(run.InterruptedRun):self.runner.execute()
        finish.assert_called_once();self.assertEqual(signal.getsignal(signal.SIGTERM),previous)
        result=json.loads((self.runner.out/"result.json").read_bytes())
        self.assertFalse(result["passed"]);self.assertIn("SIGTERM",result["error"])
    @staticmethod
    def archive(name,data,symlink=False):
        output=io.BytesIO()
        with tarfile.open(fileobj=output,mode="w") as archive:
            member=tarfile.TarInfo(name)
            if symlink:member.type=tarfile.SYMTYPE;member.linkname="/config/secret"
            else:member.size=len(data)
            archive.addfile(member,None if symlink else io.BytesIO(data))
        return output.getvalue()
    def app_outputs(self,argv,**kwargs):
        name=Path(argv[2]).name
        payload={"exit.json":b'{"exit_code":7,"succeeded":false,"stdout_bytes":0,"stderr_bytes":6}',"stdout.json":b"","stderr.log":b"failed"}[name]
        return subprocess.CompletedProcess(argv,0,self.archive(name,payload),b"")
    def test_failed_empty_stdout_and_every_stderr_retained_before_validation(self):
        with patch.object(self.runner,"wait"),patch.object(self.runner,"call",side_effect=self.app_outputs):
            with self.assertRaises(json.JSONDecodeError):self.runner.collect()
        for service in stage.SERVICES:
            directory=self.runner.out/"receipts"/service
            self.assertEqual((directory/"stderr.log").read_bytes(),b"failed")
            self.assertEqual((directory/("stdout.stream" if service=="scene" else "stdout.json")).read_bytes(),b"")
            self.assertEqual(json.loads((directory/"exit.json").read_bytes())["exit_code"],7)
    def test_readiness_failure_preserves_outputs_and_original_error(self):
        original=stage.Refused("readiness deadline")
        with patch.object(self.runner,"wait",side_effect=original),patch.object(self.runner,"call",side_effect=self.app_outputs):
            with self.assertRaises(stage.Refused) as caught:self.runner.collect()
        self.assertIs(caught.exception,original)
        self.assertEqual((self.runner.out/"receipts"/"fusion"/"stderr.log").read_bytes(),b"failed")
    def test_snapshot_keeps_bounded_prefix_and_refuses_symlink(self):
        for link in (False,True):
            with self.subTest(symlink=link):
                path=self.runner.out/("link" if link else "oversize")
                response=subprocess.CompletedProcess([],0,self.archive("stdout.json",b"x"*100,link),b"")
                with patch.object(self.runner,"call",return_value=response):receipt=self.runner.snapshot_file("/proofs/stdout.json",path,32)
                if link:self.assertEqual(receipt["status"],"invalid");self.assertFalse(path.exists())
                else:self.assertEqual(receipt["status"],"truncated_or_failed");self.assertEqual(path.read_bytes(),b"x"*32)
    def test_configs_keep_fixed_capture_bounds_and_no_signing_or_actuator_material(self):
        plan={"run_id":"run","site":"synthetic-office","frame":{"id":"office-floor","revision":"v1"},"anchor_unix_ns":1767225600000000000,"scenario_version":"v1","seed":7}
        scene={**plan,"scene_url":"http://10.65.1.2:8091","app_id":stage.APP,"boot_id":"boot","package_sha256":"a"*64}
        root=b"public-test-root";public={"tenant":"tenant","device_id":"device","principal":"spiffe://wendy.sh/tenant/tenant/device/device","root_der":base64.b64encode(root).decode(),"root_sha256":hashlib.sha256(root).hexdigest()}
        cfg=configs.configurations(plan,scene,public,"10.65.1.3")
        self.assertEqual(cfg["fusion"]["max_skew_ms"],350)
        self.assertEqual(cfg["camera-a"]["ttl_ms"],3000)
        self.assertEqual(cfg["camera-a"]["poll_interval_ms"],100)
        for service in ("sensorlink-source","sensorlink-bridge"):self.assertEqual(cfg[service]["bounds"]["max_frames"],32)
        self.assertEqual(cfg["sensorlink-source"]["poll_interval_ms"],500)
        text=json.dumps(cfg)
        for forbidden in ("actuator-token","issuer_key","private_key","eab_hmac"):self.assertNotIn(forbidden,text)
        self.assertNotIn("token",json.dumps(cfg["fusion"]))
        spec=configs.commission_spec(scene)
        self.assertTrue(spec["isolated_simulation_network"]);self.assertEqual(spec["read_token_file"],"/etc/wendy-agent/.vm-fixture/scene-read-token")
    def test_strict_receipt_json_rejects_duplicates_and_nonfinite(self):
        for raw in (b'{"a":1,"a":2}',b'{"a":NaN}',b'{"a":1e999}'):
            with self.subTest(raw=raw),self.assertRaises(stage.Refused):stage.strict_json(raw)
        self.assertEqual(stage.strict_json(b'{"a":1}'),{"a":1})


if __name__=="__main__":unittest.main()
