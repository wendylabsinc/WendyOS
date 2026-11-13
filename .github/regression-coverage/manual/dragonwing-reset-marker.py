#!/usr/bin/env python3
"""Explicit console-only marker observation; never invoked by plan generation."""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess


def observe(argv):
    result = subprocess.run(argv, stdin=subprocess.DEVNULL, capture_output=True, timeout=15, check=True)
    if len(result.stdout) > 1024*1024:
        raise ValueError("oversized inventory observation")
    return result.stdout.decode()


def device_type(content):
    # Same BOARD-before-MACHINE contract as Agent parseDeviceType, without shell evaluation.
    content=content.strip()
    if "=" not in content:return content
    fields={}
    for line in content.splitlines():
        key,separator,value=line.partition("=")
        if separator and key.strip() in ("BOARD","MACHINE","STORAGE"):
            if key.strip() in fields:raise ValueError("duplicate device identity field")
            fields[key.strip()]=value.strip()
    if fields.get("STORAGE") not in (None,"","ufs"):raise ValueError("unexpected device storage identity")
    return fields.get("BOARD") or fields.get("MACHINE","")


def target(config, root=Path("/"), run=observe):
    board=config["board"]
    if board["soc_serial_path"] not in ("/proc/device-tree/serial-number", "/sys/devices/soc0/serial_number"):
        raise ValueError("unsupported serial observer")
    serial=(root/board["soc_serial_path"].lstrip("/")).read_bytes().decode().rstrip("\x00\r\n")
    if serial!=board["soc_serial"] or device_type((root/"etc/wendyos/device-type").read_text())!=board["device"]:
        raise ValueError("physical serial or board mismatch")
    mounted=run(["findmnt","--json","--target","/data","--output","TARGET,FSTYPE,UUID,MAJ:MIN"])
    mounts=json.loads(mounted)["filesystems"]
    if len(mounts)!=1 or mounts[0]["target"]!="/data" or mounts[0]["fstype"]!="ext4" or not mounts[0].get("uuid"):
        raise ValueError("require an independently mounted plain-ext4 /data with a UUID")
    rows=json.loads(run(["lsblk","--json","--list","--bytes","--paths","--output","PATH,PKNAME,MAJ:MIN,SERIAL,SIZE,PARTLABEL,PARTUUID,TYPE"]))["blockdevices"]
    by_path={row["path"]:row for row in rows}
    parts=[row for row in rows if row["maj:min"]==mounts[0]["maj:min"]]
    if len(parts)!=1 or parts[0]["type"]!="part" or parts[0]["partlabel"]!="data":
        raise ValueError("data mount must resolve to the actual data partition")
    part=parts[0];disk=by_path.get(part["pkname"],{})
    if disk.get("type")!="disk" or str(disk.get("serial","")).strip()!=board["ufs_serial"] or disk.get("size")!=board["ufs_size_bytes"]:
        raise ValueError("data is on a different physical UFS disk")
    root_major=run(["findmnt","-n","-o","MAJ:MIN","--target","/"]).strip()
    roots=[row for row in rows if row["maj:min"]==root_major]
    if len(roots)!=1 or roots[0].get("pkname")!=part["pkname"]:
        raise ValueError("root and data must belong to the same reserved UFS disk")
    return {"soc_serial":serial,"data":mounts[0],"partition":part,"disk":disk,"boot_id":(root/"proc/sys/kernel/random/boot_id").read_text().strip()}


def marker(config, mode, root=Path("/"), run=observe):
    trial=config["trial"]
    if not re.fullmatch(r"[a-f0-9]{32}",trial):raise ValueError("invalid trial nonce")
    result=target(config,root,run)
    directory=root/"data";path=directory/(".wendy-1998-"+trial)
    expected=(trial+"\n").encode()
    if mode=="create":
        fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
        try:
            with os.fdopen(fd,"wb",closefd=False) as out:
                out.write(expected);out.flush();os.fsync(fd)
        finally:os.close(fd)
        fd=os.open(directory,os.O_RDONLY|os.O_DIRECTORY)
        try:os.fsync(fd)
        finally:os.close(fd)
    if mode in ("create","present"):
        fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
        with os.fdopen(fd,"rb") as source:data=source.read(65)
        if data!=expected:raise ValueError("owned marker differs from this trial")
    elif mode=="absent":
        try:path.lstat()
        except FileNotFoundError:pass
        else:raise ValueError("pre-flash marker survived the reset")
    else:raise ValueError("unknown marker mode")
    result.update(marker=str(path),observation=mode)
    return result


if __name__=="__main__":
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument("config",type=Path)
    parser.add_argument("mode",choices=("create","present","absent"))
    args=parser.parse_args()
    print(json.dumps(marker(json.loads(args.config.read_text()),args.mode),indent=2))
