#!/usr/bin/env python3
"""Lab image boot observer. Explicit root systemd invocation only; never reboots."""
import hashlib
import json
import os
from pathlib import Path
import re
import stat
import subprocess
import tempfile


def small(path, maximum=1048576):
    with Path(path).open("rb") as source:
        data=source.read(maximum+1)
    if len(data)>maximum: raise ValueError("oversized observation: "+str(path))
    return data


def run(*argv):
    return subprocess.run(argv, check=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=10).stdout.decode()


def collect(config, *, root=Path("/")):
    board=config["board"]
    if small(root/"proc/device-tree/serial-number").rstrip(b"\0").decode()!=board["device_tree_serial"] or ("nvidia,"+board["soc"]).encode() not in small(root/"proc/device-tree/compatible").split(b"\0"):
        raise ValueError("reserved serial/SoC mismatch")
    boot_id=small(root/"proc/sys/kernel/random/boot_id").decode().strip()
    if not re.fullmatch(r"[a-f0-9-]{36}",boot_id): raise ValueError("invalid boot ID")
    device=(root/"dev/disk/by-partuuid")/board["esp_partuuid"]
    info=device.stat()
    if not stat.S_ISBLK(info.st_mode): raise ValueError("ESP is not a block device")
    rows=json.loads(run("lsblk","--json","--list","--paths","--output","PATH,PKNAME,SERIAL,PARTUUID,PARTLABEL,MAJ:MIN,TYPE"))["blockdevices"]
    matches=[r for r in rows if str(r.get("partuuid","")).lower()==board["esp_partuuid"].lower()]
    if len(matches)!=1 or matches[0]["type"]!="part" or matches[0]["partlabel"] not in ("esp","UEFI-ESP") or matches[0]["maj:min"]!=f"{os.major(info.st_rdev)}:{os.minor(info.st_rdev)}":
        raise ValueError("ESP GPT role/device mismatch")
    parents=[r for r in rows if r["path"]==matches[0]["pkname"]]
    if len(parents)!=1 or parents[0]["type"]!="disk" or parents[0].get("serial")!=board["root_disk_serial"]:
        raise ValueError("ESP parent disk mismatch")
    root_device=run("findmnt","-n","-o","MAJ:MIN","--target","/").strip()
    roots=[r for r in rows if r.get("maj:min")==root_device]
    roles={board["app_partuuid"].lower():"APP",board["app_b_partuuid"].lower():"APP_b"}
    if len(roots)!=1 or roots[0].get("type")!="part" or roots[0].get("pkname")!=parents[0]["path"] or str(roots[0].get("partuuid","")).lower() not in roles or roles[str(roots[0].get("partuuid","")).lower()]!=roots[0].get("partlabel"):
        raise ValueError("active root is not a reserved slot on the ESP disk")
    result={"nonce":config["nonce"],"boot_id":boot_id,"serial":board["device_tree_serial"],"esp_partuuid":board["esp_partuuid"],"root_device":root_device,"boot_chain":run("nvbootctrl","get-current-slot"),"rootfs_slot":run("nvbootctrl","-t","rootfs","get-current-slot"),"firmware":run("nvbootctrl","dump-slots-info"),"esrt":{},"os_indications":{}}
    for entry in (root/"sys/firmware/efi/esrt/entries").glob("entry*"):
        result["esrt"][entry.name]={p.name:small(p,4096).decode().strip() for p in entry.iterdir() if p.is_file()}
    for path in (root/"sys/firmware/efi/efivars").glob("OsIndications-*"):
        result["os_indications"][path.name]=small(path,128).hex()
    # This runs AFTER firmware, BEFORE verifier/Agent cleanup. No observations
    # are inserted into the pre-reboot dirty-write window.
    mount_table=json.loads(run("findmnt","--json","--list","-o","MAJ:MIN,TARGET,FSTYPE,OPTIONS"))["filesystems"]
    existing=[r for r in mount_table if r["maj:min"]==matches[0]["maj:min"]]
    owned=None
    if len(existing)>1: raise ValueError("ambiguous ESP mounts")
    if existing:
        if existing[0]["fstype"]!="vfat": raise ValueError("ESP mount is not vfat")
        view=Path(existing[0]["target"])
    else:
        owned=Path(tempfile.mkdtemp(prefix="ci1682-esp-",dir=root/"run"))
        print("CI1682_OWNED_ESP_VIEW "+str(owned),flush=True)
        # On timeout/failure do not race cleanup against uncertain kernel state.
        run("mount","-t","vfat","-o","ro",str(device),str(owned))
        check=json.loads(run("findmnt","--json","--mountpoint",str(owned),"-o","MAJ:MIN,FSTYPE,OPTIONS"))["filesystems"]
        if len(check)!=1 or check[0]["maj:min"]!=matches[0]["maj:min"] or check[0]["fstype"]!="vfat" or "ro" not in check[0]["options"].split(","):
            raise ValueError("owned read-only ESP mount verification failed")
        view=owned
    try:
        capsule=view/"EFI/UpdateCapsule/TEGRA_BL.Cap"
        try:
            size=capsule.stat().st_size
        except FileNotFoundError:
            result["capsule"]={"present":False}
        else:
            if not 0<size<=256*1024**2: raise ValueError("capsule outside observer size bound")
            data=small(capsule,256*1024**2)
            result["capsule"]={"present":True,"size_bytes":len(data),"sha256":hashlib.sha256(data).hexdigest()}
    finally:
        if owned is not None:
            run("umount",str(owned))
            owned.rmdir()
    return result


def save(config,result, *, root=Path("/")):
    # /data path is private to this independently generated lab nonce. Refuse
    # any symlink or existing boot record rather than replacing evidence.
    nonce=config["nonce"]
    if not re.fullmatch(r"[a-f0-9]{32}",nonce): raise ValueError("invalid nonce")
    data_root=root/"data"
    if not data_root.is_dir() or data_root.is_symlink(): raise ValueError("data mount unavailable")
    directory=data_root/"ci1682"/nonce
    for owned in (directory.parent,directory):
        try: os.mkdir(owned,0o700)
        except FileExistsError: pass
        info=owned.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid!=os.geteuid() or info.st_mode & 0o022:
            raise ValueError("unowned or unsafe evidence directory")
    payload=(json.dumps(result,sort_keys=True)+"\n").encode()
    fd=os.open(directory/(result["boot_id"]+".json"),os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
    with os.fdopen(fd,"wb") as target:
        target.write(payload); target.flush(); os.fsync(target.fileno())
    fd=os.open(directory,os.O_RDONLY|os.O_DIRECTORY)
    try: os.fsync(fd)
    finally: os.close(fd)
    print("CI1682_BOOT_OBSERVATION "+payload.decode().strip(),flush=True)


if __name__=="__main__":
    if os.geteuid()!=0: raise PermissionError("reserved root systemd observer only")
    config=json.loads(small("/etc/ci1682-boundary.json"))
    result=collect(config)
    save(config,result)
