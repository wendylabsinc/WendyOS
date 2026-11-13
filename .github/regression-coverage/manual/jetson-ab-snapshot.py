#!/usr/bin/env python3
"""Explicit read-only serial-console snapshot; generation never invokes this."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import stat
import subprocess


def run(argv):
    # Each known local observer has a finite deadline. No shell, reboot, install,
    # EFI writes, mount command, state cleanup or credential-file capture.
    result = subprocess.run(argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, timeout=20, check=False)
    return {"argv":argv, "exit_code":result.returncode,
            "stdout":result.stdout.decode(errors="replace"),
            "stderr":result.stderr.decode(errors="replace")}


def small(path, maximum=256*1024):
    with Path(path).open("rb") as source:
        data=source.read(maximum+1)
    if len(data)>maximum:
        raise ValueError("oversized snapshot input: "+str(path))
    return data


def sha(path):
    value=hashlib.sha256()
    with Path(path).open("rb") as source:
        for chunk in iter(lambda:source.read(1024*1024),b""):
            value.update(chunk)
    return value.hexdigest()


def check_disk_layout(devices, partitions, board):
    rows={row["path"]:row for row in devices}
    parents=set()
    labels={"app_partuuid":("APP",),"app_b_partuuid":("APP_b",),"esp_partuuid":("esp","UEFI-ESP")}
    for name, partition in partitions.items():
        row=rows[partition["path"]]
        if (row.get("partuuid","").lower()!=board[name].lower()
                or row.get("partlabel") not in labels[name] or row.get("type")!="part"
                or row.get("maj:min")!=partition["major_minor"]):
            raise ValueError("GPT label/UUID/device number does not match the expected root slots")
        parents.add(row["pkname"])
    if len(parents)!=1:
        raise ValueError("APP/APP_b/ESP do not share one disk")
    disk=rows[next(iter(parents))]
    if disk.get("type")!="disk" or (disk.get("serial") or "").strip()!=board["root_disk_serial"]:
        raise ValueError("APP/APP_b/ESP must share the reserved physical root disk")


def collect(config, slot, require_agent=False):
    board=config["board"]
    serial=small("/proc/device-tree/serial-number",1024).decode().rstrip("\x00\r\n")
    compatible=small("/proc/device-tree/compatible",4096).decode().split("\x00")
    if serial!=board["device_tree_serial"] or "nvidia,"+board["soc"] not in compatible:
        raise ValueError("physical serial/SoC mismatch; stop before all other observations")
    if small("/etc/wendyos/device-type",1024).decode().strip()!=board["device_type"]:
        raise ValueError("device type mismatch")
    partitions={}
    for name in ("app_partuuid","app_b_partuuid","esp_partuuid"):
        path=Path("/dev/disk/by-partuuid")/board[name]
        info=path.stat()
        if not stat.S_ISBLK(info.st_mode):
            raise ValueError("expected real block partition")
        partitions[name]={"path":str(path.resolve()),"major_minor":f"{os.major(info.st_rdev)}:{os.minor(info.st_rdev)}"}
    root=run(["findmnt","-n","-o","MAJ:MIN","--target","/"])
    selected="app_partuuid" if slot=="a" else "app_b_partuuid"
    if root["exit_code"] or root["stdout"].strip()!=partitions[selected]["major_minor"]:
        raise ValueError("running root does not match the expected exact slot partition")
    disk=run(["lsblk","--json","--list","--paths","--output","PATH,PKNAME,SERIAL,PARTUUID,PARTLABEL,MAJ:MIN,SIZE,TYPE"])
    if disk["exit_code"]:
        raise ValueError("cannot observe root disk identity")
    devices=json.loads(disk["stdout"])["blockdevices"]
    check_disk_layout(devices,partitions,board)
    updater=Path("/usr/bin/wendyos-update")
    if sha(updater)!=config["updater_sha256"]:
        raise ValueError("installed updater differs from the pinned executable")
    settings=Path("/etc/wendyos-update/config.json")
    if settings.exists():
        cfg=json.loads(small(settings))
        allowed={"connector":("","tegrauefi"),"state_dir":("","/data/wendyos-update"),"hooks_dir":("","/etc/wendyos-update"),"health_dir":("","/etc/wendyos-update/health.d"),"device_type_path":("","/etc/wendyos/device-type")}
        if any(key not in allowed or value not in allowed[key] for key,value in cfg.items()):
            raise ValueError("nonstandard updater configuration needs a separately reviewed plan")
    result={"serial":serial,"slot_expected":slot,"boot_id":small("/proc/sys/kernel/random/boot_id",128).decode().strip(),"root":root,"partitions":partitions,"disk":disk,"updater_sha256":config["updater_sha256"],"observations":{},"efi":{},"durable_state":{}}
    for name in ("RootfsRedundancyLevel","RootfsRetryCountMax"):
        raw=small("/sys/firmware/efi/efivars/"+name+"-781e084c-a330-417c-b678-38e696380cb9",8)
        if len(raw)!=8:raise ValueError("invalid EFI UINT32: "+name)
        result["efi"][name]={"attributes":int.from_bytes(raw[:4],"little"),"value":int.from_bytes(raw[4:],"little"),"sha256":hashlib.sha256(raw).hexdigest()}
    pid=run(["systemctl","show","wendy-agent.service","--property=MainPID","--value"])
    value=pid["stdout"].strip()
    if pid["exit_code"] or not value.isdecimal() or int(value)<=1:
        raise ValueError("no live Agent to fingerprint")
    result["agent"]={"pid":int(value),"exe":os.readlink("/proc/"+value+"/exe"),"sha256":sha("/proc/"+value+"/exe")}
    if require_agent and result["agent"]["sha256"]!=config["agent_sha256"]:
        raise ValueError("live Agent is not the pinned candidate")
    for name,args in {"engine_status":[str(updater),"status","--json","--verbose"],"engine_check":[str(updater),"check","--json"],"boot_chain":["nvbootctrl","dump-slots-info"],"rootfs_slots":["nvbootctrl","-t","rootfs","dump-slots-info"],"kernel":["uname","-a"],"units":["systemctl","show","wendy-agent.service","wendyos-update-verify.service","wendyos-update-commit.service","--property=ActiveState,SubState,Result,ExecMainStatus"]}.items():
        result["observations"][name]=run(args)
    for name in ("state.json","installed.json","boot_attempted","bl-version-before"):
        path=Path("/data/wendyos-update")/name
        result["durable_state"][name]=small(path).decode(errors="replace") if path.exists() else None
    result["os_release"]=small("/etc/os-release").decode()
    return result


if __name__=="__main__":
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("config",type=Path)
    parser.add_argument("--slot",choices=("a","b"),required=True)
    parser.add_argument("--require-agent",action="store_true")
    args=parser.parse_args()
    print(json.dumps(collect(json.loads(args.config.read_text()),args.slot,args.require_agent),indent=2))
