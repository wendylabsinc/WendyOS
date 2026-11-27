"""Stage regressions use fake Docker metadata; they never launch a process."""
import copy
import importlib.util
import io
import json
import os
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

HERE=Path(__file__).resolve().parent
sys.path.insert(0,str(HERE))
import stage
import validation

SCENE="sha256:"+"a"*64
RUNTIME="sha256:"+"b"*64


def rootfs(path,extra=None):
    with tarfile.open(path,"w") as archive:
        entries=[("usr/local/lib/python3.12/site-packages/mujoco_core-4.0.0.dist-info/METADATA",b"Metadata-Version: 2.1\nName: mujoco-core\nVersion: 4.0.0\n"),("usr/local/bin/python",b"fake interpreter")]
        if extra:entries.append(extra)
        for name,body in entries:
            member=tarfile.TarInfo(name);member.size=len(body);member.mode=0o644;archive.addfile(member,io.BytesIO(body))
    os.chmod(path,0o600)


class Docker:
    def __init__(self):self.commands=[];self.name=None;self.token=None;self.fail_export=False;self.fail_create=False;self.foreign=False
    def run(self,argv,**kwargs):
        self.commands.append(argv)
        if argv[1:3]==["image","inspect"]:
            item={"Id":argv[3],"Os":"linux","Architecture":"arm64","Size":1024,"Config":{"User":"65534:65534","WorkingDir":"/world","Entrypoint":["python","-m","sim.live"],"Env":["PATH=/usr/local/bin:/usr/bin:/bin","LANG=C.UTF-8","MUJOCO_GL=osmesa","MESA_SHADER_CACHE_DISABLE=true"]}}
            return subprocess.CompletedProcess(argv,0,json.dumps([item]).encode(),b"")
        if argv[1]=="create":
            self.name=argv[argv.index("--name")+1];self.token=argv[argv.index("--label")+1].split("=",1)[1]
            if self.fail_create:raise subprocess.TimeoutExpired(argv,30)
        if argv[1]=="inspect":
            item={"Image":SCENE,"Config":{"Labels":{stage.LABEL:"foreign" if self.foreign else self.token}},"State":{"Status":"created","Running":False,"Pid":0},"Mounts":[]}
            return subprocess.CompletedProcess(argv,0,json.dumps([item]).encode(),b"")
        if argv[1]=="export":
            if self.fail_export:raise subprocess.TimeoutExpired(argv,120)
            rootfs(Path(argv[argv.index("--output")+1]))
        return subprocess.CompletedProcess(argv,0,b"",b"")


class StageTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory(prefix="world-mm-stage-");self.addCleanup(self.tmp.cleanup)
        self.root=Path(self.tmp.name).resolve();os.chmod(self.root,0o700);self.staging=self.root/"staging";self.docker=Docker()
    def export(self):
        path=self.staging/"base"
        result=stage.export_rootfs(SCENE,path,run=self.docker.run,staging=self.staging)
        return path,result
    def inputs(self):
        files={}
        for name in stage.BINARIES:
            p=self.root/name;data=bytearray(64);data[:6]=b"\x7fELF\x02\x01";data[18:20]=(183).to_bytes(2,"little");p.write_bytes(data);p.chmod(0o700);files[name]=p
        return files
    def prepare(self):
        base,_=self.export();out=self.staging/"run"
        plan=stage.prepare(base,out,RUNTIME,self.inputs(),"run-1",1767225600000000000,7,run=self.docker.run,staging=self.staging)
        return out,plan
    def test_export_only_stopped_owned_container_and_cleanup(self):
        _,result=self.export();self.assertFalse(result["container_started"]);self.assertEqual(result["rootfs"]["mujoco_version"],"4.0.0")
        self.assertFalse(any(c[1] in ("run","start","exec","push","pull") for c in self.docker.commands))
        self.assertEqual(self.docker.commands[-1],["docker","rm",self.docker.name])
    def test_export_failure_still_cleans_only_own_stopped_container(self):
        self.docker.fail_export=True
        with self.assertRaises(subprocess.TimeoutExpired):self.export()
        self.assertEqual(self.docker.commands[-1],["docker","rm",self.docker.name])
    def test_ambiguous_create_timeout_still_checks_ownership_and_cleans(self):
        self.docker.fail_create=True
        with self.assertRaises(subprocess.TimeoutExpired):self.export()
        self.assertEqual(self.docker.commands[-1],["docker","rm",self.docker.name])
    def test_foreign_export_container_never_removed(self):
        self.docker.foreign=True
        with self.assertRaises(stage.Refused):self.export()
        self.assertFalse(any(c[1]=="rm" for c in self.docker.commands))
    def test_mutable_pin_and_reused_or_symlink_output_refused(self):
        with self.assertRaises(stage.Refused):stage.image_info("tag:latest",run=self.docker.run)
        self.export()
        with self.assertRaises(stage.Refused):stage.safe_output(self.staging/"base",self.staging)
        link=self.staging/"linked";link.symlink_to(self.root,target_is_directory=True)
        with self.assertRaises(stage.Refused):stage.safe_output(link/"escaped",self.staging)
    def test_archive_traversal_duplicate_and_wrong_mujoco_refused(self):
        for name,body in [("../escape",b"x"),("/absolute",b"x"),("usr/local/bin/python",b"duplicate"),("root/.ssh/id_ed25519",b"private")]:
            with self.subTest(name=name):
                p=self.root/"bad.tar";rootfs(p,(name,body))
                with self.assertRaises(stage.Refused):stage.inspect_rootfs(p)
        p=self.root/"bad.tar";rootfs(p)
        data=p.read_bytes().replace(b"Version: 4.0.0",b"Version: 3.9.9");p.write_bytes(data)
        with self.assertRaises(stage.Refused):stage.inspect_rootfs(p)
    def test_exact_staged_project_inputs_and_no_credentials(self):
        out,plan=self.prepare();self.assertEqual(stage.validate_stage(out),plan)
        project=json.loads((out/"project/wendy.json").read_bytes())
        self.assertEqual(set(project["services"]),set(stage.SERVICES));self.assertEqual(project["isolation"],"isolated")
        self.assertTrue(all("dependsOn" not in s for s in project["services"].values()))
        for name in ("scene","sensorlink-source"):
            self.assertFalse(any(e["type"]=="world" for e in project["services"][name]["entitlements"]))
        for path in (out/"project").rglob("*"):
            self.assertNotIn(path.name,("config.json","scene-read-token","actuator-token","authority.json","provisioning.json"))
        dockerfile=(out/"project/services/scene/Dockerfile").read_text()
        self.assertIn("FROM scratch\nADD rootfs.tar /",dockerfile);self.assertIn("USER 65534:65534",dockerfile);self.assertNotIn("# syntax",dockerfile)
    def test_changed_or_extra_staged_files_refused(self):
        out,_=self.prepare();extra=out/"project/token";extra.write_text("unapproved")
        with self.assertRaises(stage.Refused):stage.validate_stage(out)
        extra.unlink();p=out/"tools/cli";p.write_bytes(b"changed")
        with self.assertRaises(stage.Refused):stage.validate_stage(out)
    def test_scene_continuity_and_exact_deployment_selection(self):
        _,plan=self.prepare();nonce="e"*64
        clock={"schema_version":"wendy.sim.clock.v1","run_id":plan["run_id"],"boot_id":"b87f3e79-43e1-4280-a9bf-1d4060d6204a","package_sha256":plan["package_sha256"],"frame":plan["frame"],"clock_domain":plan["clock_domain"],"anchor_unix_ns":plan["anchor_unix_ns"],"simulation_time_ns":0,"observed_unix_ns":1767225600000000000,"observation_sequence":1,"nonce":nonce}
        task={"container":stage.APP+"_scene","app":stage.APP,"service":"scene","pid":101,"start_ticks":500,"netns_inode":900,"cgroup":"0::/owned/scene","ipv4":"10.65.1.2","world_socket":False}
        before=validation.scene_receipt(plan,clock,nonce,"http://10.65.1.2:8091",task,"10.65.1.0/28")
        clock["observation_sequence"]+=1
        after=validation.scene_receipt(plan,clock,nonce,"http://10.65.1.2:8091",task,"10.65.1.0/28")
        self.assertTrue(validation.scene_unchanged(before,after))
        for key in ("boot_id","package_sha256","task"):
            bad=copy.deepcopy(after);bad[key]="changed"
            with self.subTest(key=key),self.assertRaises(stage.Refused):validation.scene_unchanged(before,bad)
        for service in stage.SERVICES:
            args=validation.selective_run_args(service);self.assertEqual(args[-2:],["--service",service]);self.assertIn("--no-restart",args)
        with self.assertRaises(stage.Refused):validation.scene_receipt(plan,clock,"f"*64,"http://10.65.1.2:8091",task,"10.65.1.0/28")
        with self.assertRaises(stage.Refused):validation.scene_receipt(plan,clock,nonce,"http://192.0.2.1:8091",task,"10.65.1.0/28")


if __name__=="__main__":unittest.main()
