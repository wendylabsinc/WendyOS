#!/usr/bin/env python3
"""Prepare historical1682 source overlays and early-boot observer. No hardware calls."""
import argparse
import hashlib
import importlib.util
import json
from pathlib import Path
import re
import shlex
import subprocess

HERE=Path(__file__).resolve().parent
spec=importlib.util.spec_from_file_location("capsule_boundary_base",HERE/"jetson-ab-rehearsal.py")
AB=importlib.util.module_from_spec(spec);spec.loader.exec_module(AB)
AGENT_REV="7c2d5787b7e1cfc7347caa700e6e1bfe584125f2"
UPDATER_REV="2ef50a953e29809f2bd2a59df0f50625e2030289"
AGENT_PATH="go/internal/agent/services/agent_service.go"
REBOOT_PATH="go/internal/agent/services/reboot_linux.go"
AGENT_SHA="cec3e3177dfc5e6e6b42604c78033e356864ea00f856de7b3e0786b0ebd34aa5"
UPDATER_SHA="30c43076d9a2c1809baa7392ab1b7471e9225c481138e10bf972a3f0b422586f"


def git(repo,*args):
    return subprocess.check_output(["git",*args],cwd=repo,timeout=10).decode().strip()


def pin(repo,revision):
    if git(repo,"rev-parse","HEAD")!=revision or git(repo,"status","--porcelain","--untracked-files=no"):
        raise ValueError("require a clean exact historical checkout: "+revision)


def replace_once(source,old,new):
    if source.count(old)!=1: raise ValueError("historical instrumentation anchor changed")
    return source.replace(old,new)


def generate(inventory,output):
    data=json.loads(Path(inventory).read_text())
    source=Path(data["agent_source"]).resolve(strict=True)
    updater=Path(data["updater_source"]).resolve(strict=True)
    pin(source,AGENT_REV);pin(updater,UPDATER_REV)
    base_record=AB.record(data["capsule_plan"])
    base=json.loads(Path(base_record["path"]).read_text())
    if base.get("audit_entries")!=["1682"]: raise ValueError("require exact-target capsule plan")
    for item in [base["archive"],base["capsule"],*base["records"].values(),*base["sources"],*base["generated"]]:
        if AB.record(item["path"])!=item: raise ValueError("capsule preparation input changed")
    ab_record=base["ab_plan"]
    if AB.record(ab_record["path"])!=ab_record: raise ValueError("identity plan changed")
    ab_plan=json.loads(Path(ab_record["path"]).read_text())
    cli=ab_plan["records"]["cli_binary"]
    if AB.record(cli["path"])!=cli: raise ValueError("CLI binary changed")
    if AB.record(base["descriptor"]["path"])!=base["descriptor"]: raise ValueError("artifact descriptor changed")
    artifact_url=json.loads(Path(base["descriptor"]["path"]).read_text())["url"]
    board=base["board"]
    if board["soc"]!="tegra264" or board["device_type"]!="jetson-agx-thor": raise ValueError("this exact historical updater stages capsules only on Thor")
    nonce=data["nonce"]
    if not re.fullmatch(r"[a-f0-9]{32}",nonce): raise ValueError("require independent lab nonce")
    if data["running_os"] not in ("0.16.1","0.17.0","0.18.0"):
        raise ValueError("require an actually affected release, never a spoofed version")
    legacy=AB.record(data["legacy_updater_binary"])
    provenance=AB.record(data["legacy_producer_record"])
    producer=json.loads(Path(provenance["path"]).read_text())
    if producer.get("source_revision")!=UPDATER_REV or producer.get("binary_sha256")!=legacy["sha256"] or producer.get("running_os")!=data["running_os"]:
        raise ValueError("bind affected updater binary to exact producer source and OS")
    AB.line(producer["reviewer"])
    text=(source/AGENT_PATH).read_text()
    if hashlib.sha256(text.encode()).hexdigest()!=AGENT_SHA or AB.record(str(updater/"internal/connector/tegrauefi/swap-slot.go"))["sha256"]!=UPDATER_SHA:
        raise ValueError("historical source hash mismatch")
    call="rebootAfterOSUpdate(newRebooter(s.logger), osVersion)"
    # Remove the now-unused version read in the two immediate variants. No
    # instrumentation logging, sleeps, reads or syncs enter the dirty window.
    instant=replace_once(text,"\tosVersion, _ := wendyOSVersion()\n","")
    variants={"fixed":{},"historical-instant":{AGENT_PATH:replace_once(instant,call,"newRebooter(s.logger).immediate()")},"sync-only":{AGENT_PATH:replace_once(instant,call,"newRebooter(s.logger).reboot()")}}
    wiring=(source/REBOOT_PATH).read_text()
    variants["clean-request-failure"]={REBOOT_PATH:replace_once(wiring,"clean:     systemctlReboot,",'clean:     func() error { return fmt.Errorf("CI1682 injected systemctl request failure") },')}
    variants["watchdog-no-reboot"]={REBOOT_PATH:replace_once(wiring,"clean:     systemctlReboot,","clean:     func() error { return nil },")}
    out=Path(output).resolve();out.mkdir(parents=True,mode=0o700,exist_ok=False)
    commands={}
    for variant,files in variants.items():
        directory=out/variant;directory.mkdir()
        replacements={}
        for filename,body in files.items():
            replacement=directory/Path(filename).name;replacement.write_text(body)
            replacements[str(source/filename)]=str(replacement)
        overlay=directory/"overlay.json";overlay.write_text(json.dumps({"Replace":replacements},indent=2)+"\n")
        commands[variant]=["env","GOTOOLCHAIN=go1.26.5","GOOS=linux","GOARCH=arm64","CGO_ENABLED=0","go","build","-mod=readonly","-trimpath","-overlay",str(overlay),"-o",str(directory/"wendy-agent-lab-only"),"./go/cmd/wendy-agent"]
    package=out/"image-files";package.mkdir()
    def emit(name,body,mode=0o644):
        path=package/name;path.parent.mkdir(parents=True,exist_ok=True);path.write_text(body);path.chmod(mode)
    emit("usr/local/libexec/ci1682-observe.py",(HERE/"jetson-capsule-boundary-observer.py").read_text(),0o755)
    emit("etc/ci1682-boundary.json",json.dumps({"nonce":nonce,"board":board},indent=2)+"\n",0o600)
    emit("etc/systemd/system/ci1682-observe.service","[Unit]\nDescription=Reserved CI1682 pre-verifier boot evidence\nAfter=local-fs.target data.mount\nRequiresMountsFor=/data\nBefore=wendyos-update-verify.service wendyos-update-commit.service wendy-agent.service\n[Service]\nType=oneshot\nExecStart=/usr/bin/python3 /usr/local/libexec/ci1682-observe.py\nTimeoutStartSec=60\nKillMode=control-group\nRemainAfterExit=yes\nStandardOutput=journal+console\nStandardError=journal+console\n")
    for unit in ("wendyos-update-verify.service","wendyos-update-commit.service","wendy-agent.service"):
        emit("etc/systemd/system/"+unit+".d/ci1682-observer.conf","[Unit]\nRequires=ci1682-observe.service\nAfter=ci1682-observe.service\n")
    (out/"build-commands.json").write_text(json.dumps(commands,indent=2)+"\n")
    device_commands={}
    for variant in variants:
        command=["device","update","--device",board["address"],"--binary",str(out/variant/"wendy-agent-lab-only")]
        # Agent upload only: without --artifact-url, --yes applies whatever OS
        # the public manifest offers, and omitting it prompts on a terminal.
        # JSON mode uploads and verifies the Agent, then skips the OS step.
        device_commands["deploy_"+variant]=[cli["path"],"--json",*command]
        device_commands["update_"+variant]=[cli["path"],*command,"--yes","--artifact-url",artifact_url]
    (out/"device-commands.json").write_text(json.dumps(device_commands,indent=2)+"\n")
    guide=(HERE/"jetson-capsule-boundary.txt").read_text()
    for key,value in {"@SERIAL@":board["serial"],"@NONCE@":nonce,"@OS@":data["running_os"],"@UPDATER@":legacy["sha256"],"@SOURCE@":str(source),"@OUTPUT@":str(out)}.items():guide=guide.replace(key,value)
    guide+="\nDevice commands (only after owner authorization and complete image qualification):\n"+"\n".join(k+": "+shlex.join(v) for k,v in device_commands.items())+"\n"
    guide+="\nBuild commands (host only, cwd "+shlex.quote(str(source))+"):\n"+"\n".join(k+": "+shlex.join(v) for k,v in commands.items())+"\n"
    (out/"procedure.txt").write_text(guide)
    evidence={"audit_entries":["1682"],"hardware_execution":"not_run","capsule_plan":base_record,"agent_revision":AGENT_REV,"updater_revision":UPDATER_REV,"legacy_updater":legacy,"producer_record":provenance,"nonce":nonce,"running_os":data["running_os"],"board":board,"build_commands":commands,"sources":[AB.record(str(p)) for p in (Path(__file__),HERE/"jetson-capsule-boundary-observer.py",HERE/"jetson-capsule-boundary.txt",source/AGENT_PATH,source/REBOOT_PATH,source/"go/internal/agent/services/reboot.go",source/"go.mod",source/"go.sum",updater/"internal/connector/tegrauefi/swap-slot.go")],"generated":[AB.record(str(p)) for p in sorted(out.rglob("*")) if p.is_file()],"cases":[{"id":v,"status":"not_run","observations":[]} for v in variants],"scope":"Historical abrupt reboot fault, no cache manipulation or physical power cut; observer runs only after firmware and before verifier/Agent cleanup."}
    (out/"evidence.json").write_text(json.dumps(evidence,indent=2)+"\n")
    return evidence


if __name__=="__main__":
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument("inventory");parser.add_argument("output")
    args=parser.parse_args();generate(args.inventory,args.output)
