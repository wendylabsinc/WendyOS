#!/usr/bin/env python3
"""Prepare physical Dragonwing config/data reset acceptance, without device access."""
import argparse
from decimal import Decimal
import hashlib
import importlib.util
import ipaddress
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import xml.etree.ElementTree as ET

ROOT=Path(__file__).resolve().parents[3]
HELPER=Path(__file__).with_name("orin-firstboot-rehearsal.py")
SPEC=importlib.util.spec_from_file_location("dragonwing_plan_helpers",HELPER)
COMMON=importlib.util.module_from_spec(SPEC);SPEC.loader.exec_module(COMMON)
line,artifact=COMMON.line,COMMON.artifact
BOARDS={"dragonwing-iq-9075":{"msm_id":"0x002eb0e1","switch":"MODE switch 3","on":"up (ON)","off":"down (OFF)","management":"Use the inventory's verified wired or seeded Wi-Fi management endpoint."},
        "dragonwing-iq-8275":{"msm_id":"0x002e70e1","switch":"S2-8","on":"ON","off":"OFF","management":"After boot, USB2 (micro-USB) supplies USB networking; USB0 (USB-C) remains EDL/recovery."}}
BUILDER_FILES=("meta-qcom-extensions/recipes-core/wendyos-config-setup/files/wendyos-config-init.sh","recipes-support/data-device/files/wendyos-data.sh")


def bounded_xml(path):
    with Path(path).open("rb") as source:data=source.read(4*1024**2+1)
    if len(data)>4*1024**2 or b"<!DOCTYPE" in data or b"<!ENTITY" in data:raise ValueError("require bounded plain descriptor XML")
    return ET.fromstring(data)


def generate(inventory,output):
    data=json.loads(Path(inventory).read_text())
    def reject_secrets(value):
        if isinstance(value,dict):
            for key,child in value.items():
                if any(word in key.lower() for word in ("password","passphrase","private_key","token","eabhmac")):raise ValueError("do not record credentials")
                reject_secrets(child)
        elif isinstance(value,list):
            for child in value:reject_secrets(child)
    reject_secrets(data)
    board=data["board"];kind=BOARDS.get(board.get("device"))
    if not kind or board.get("storage")!="ufs" or board.get("msm_id")!=kind["msm_id"]:raise ValueError("require an exact supported IQ9075/IQ8275 UFS target")
    for key in ("serial","soc_serial","owner","edl_usb_path","serial_console","ufs_serial","carrier_revision","management_address"):line(board[key])
    if board["soc_serial_path"] not in ("/proc/device-tree/serial-number","/sys/devices/soc0/serial_number"):raise ValueError("pin an actual supported serial observer")
    if type(board["ufs_size_bytes"]) is not int or board["ufs_size_bytes"]<1024**3:raise ValueError("pin actual UFS capacity")
    if board["management_address"].startswith(("-","cloud:","mesh:","vm:")):raise ValueError("require a direct physical management address")
    if not re.fullmatch(r"[a-z][a-z0-9-]{2,54}",board["device_name"]) or board["device_name"].endswith("-"):raise ValueError("invalid unique seed name")
    if not re.fullmatch(r"[a-f0-9]{32}",data["trial"]):raise ValueError("require independent random trial nonce")
    for key in ("cli_source_commit","agent_source_commit","builder_source_commit"):
        if not re.fullmatch(r"[a-f0-9]{40}",data[key]):raise ValueError("pin exact source commits")
    for key,low,high in (("flash_seconds",300,7200),("boot_seconds",60,900)):
        if type(data[key]) is not int or not low<=data[key]<=high:raise ValueError("require bounded observation windows")
    wifi=data["wifi"]
    if wifi.get("security")!="open" or len(line(wifi["ssid"]).encode())>32 or any(c in wifi["ssid"] for c in ",\\"):raise ValueError("use a reserved open lab AP without credential input")
    if not re.fullmatch(r"[A-Za-z0-9_.-]{1,15}",wifi["interface"]):raise ValueError("pin exact Wi-Fi interface")
    for key in ("station_mac","bssid"):
        if not re.fullmatch(r"(?:[0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}",wifi[key]):raise ValueError("pin actual AP/station MAC")
    ipaddress.ip_address(wifi["gateway"])
    keys=("cli_binary","agent_binary","release_descriptor","release_archive","device_catalog","rawprogram","patches","programmer","bundle_review","identity_record","storage_record","recovery_record","authorization_record","ap_record")
    records={key:artifact(data[key]) for key in keys}
    if not os.access(records["cli_binary"]["path"],os.X_OK):raise ValueError("CLI must be executable")
    release=json.loads(Path(records["release_descriptor"]["path"]).read_text())
    if release.get("device")!=board["device"] or release.get("storage")!="ufs":raise ValueError("bundle descriptor targets a different board/storage")
    version=release["version"]
    if not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9._-]*",version) or version in ("latest","nightly"):raise ValueError("pin immutable version")
    archive=records["release_archive"]
    if release.get("sha256")!=archive["sha256"] or type(release.get("size_bytes")) is not int or release["size_bytes"]!=archive["size_bytes"]:raise ValueError("bundle bytes differ from descriptor")
    catalog=json.loads(Path(records["device_catalog"]["path"]).read_text())
    if catalog["device_id"]!=board["device"]:raise ValueError("catalog device mismatch")
    entry=catalog["versions"][version]
    from urllib.parse import urlsplit
    url=urlsplit(line(release["url"]))
    expected="https://storage.googleapis.com/wendyos-images-public/"+entry["qcomflash_path"]
    if url._replace(query="").geturl()!=expected or url.scheme!="https" or url.netloc!="storage.googleapis.com" or url.fragment or (url.query and not re.fullmatch(r"generation=[0-9]+",url.query)) or any(v in url.path.split("/") for v in ("..","latest","nightly")) or not url.path.endswith(".tar.gz"):
        raise ValueError("require exact immutable public qcomflash URL")
    if entry["qcomflash_checksum"]!=archive["sha256"] or entry["qcomflash_size_bytes"]!=archive["size_bytes"]:raise ValueError("catalog and archive identity differ")
    declarations=bounded_xml(records["rawprogram"]["path"]).findall("program")
    geometry={}
    for label in ("config","data"):
        selected=[p.attrib for p in declarations if p.get("label")==label]
        if len(selected)!=1:raise ValueError("require unique config and data declarations")
        p=selected[0];sector=int(p["SECTOR_SIZE_IN_BYTES"]);count=int(p["num_partition_sectors"])
        if p.get("filename")!="" or p.get("sparse","false").lower()!="false" or sector<512 or sector>65536 or sector&(sector-1) or not p["start_sector"].isdigit() or count<0:raise ValueError("unsupported seed/blank geometry")
        if label=="config" and (count<=0 or Decimal(p["size_in_KB"])*1024!=count*sector):raise ValueError("config payload must fill its declared partition")
        geometry[label]={"lun":int(p["physical_partition_number"]),"start_sector":int(p["start_sector"]),"sector_size":sector,"num_sectors":count}
        if geometry[label]!=board["partitions"][label]:raise ValueError("descriptor differs from reviewed physical geometry")
    cfg,datum=geometry["config"],geometry["data"]
    if cfg["lun"]!=datum["lun"] or cfg["sector_size"]!=datum["sector_size"] or datum["start_sector"]<cfg["start_sector"]+cfg["num_sectors"]:raise ValueError("require ordered nonoverlapping config/data on one UFS LUN")
    if not any(p.get("filename")=="DISK" for p in bounded_xml(records["patches"]["path"]).findall("patch")):raise ValueError("require actual disk GPT patches")
    review=json.loads(Path(records["bundle_review"]["path"]).read_text())
    for key in ("release_archive","rawprogram","patches","programmer"):
        if review.get(key+"_sha256")!=records[key]["sha256"]:raise ValueError("independent bundle inspection does not bind supplied bytes")
    if review.get("builder_source_commit")!=data["builder_source_commit"]:raise ValueError("bundle build provenance differs")
    line(review["reviewer"])
    builder=Path(data["builder_source"]).resolve(strict=True)
    provenance=[]
    for path in BUILDER_FILES:
        content=subprocess.check_output(["git","show",data["builder_source_commit"]+":"+path],cwd=builder,timeout=10)
        provenance.append({"revision":data["builder_source_commit"],"path":path,"sha256":hashlib.sha256(content).hexdigest()})
    out=Path(output).resolve();out.mkdir(mode=0o700,parents=True,exist_ok=False)
    config=out/"cli-config";config.mkdir(mode=0o700)
    prefix=["env","WENDY_CONFIG_DIR="+str(config),"WENDY_SECRET_STORE=file","WENDY_ANALYTICS=false","WENDY_NO_UPDATE_CHECK=1",records["cli_binary"]["path"]]
    selection=["--device-type",board["device"],"--version",version]
    target=["--device",board["management_address"]]
    commands={"plan":[*prefix,"install","plan",*selection],"install":[*prefix,"install",*selection,"--binary",records["agent_binary"]["path"],"--device-name",board["device_name"],"--pre-enroll=false","--wifi","ssid="+wifi["ssid"]+",security=open"],
              "verify":[*prefix,"install","verify","--address",board["management_address"],"--expected-device-type",board["device"],"--expected-os-version",version,"--timeout","30s"],"info":[*prefix,"--json","device","info",*target],"wifi_status":[*prefix,"--json","device","wifi","status",*target]}
    (out/"commands.json").write_text(json.dumps(commands,indent=2)+"\n")
    observer=Path(__file__).with_name("dragonwing-reset-marker.py")
    (out/observer.name).write_text(observer.read_text())
    (out/"marker-config.json").write_text(json.dumps({"board":board,"trial":data["trial"]},indent=2)+"\n")
    guide=Path(__file__).with_suffix(".txt").read_text()
    replacements={"@SERIAL@":board["serial"],"@TRIAL@":data["trial"],"@NAME@":board["device_name"],"@SWITCH@":kind["switch"],"@ON@":kind["on"],"@OFF@":kind["off"],"@MSM@":kind["msm_id"],"@MANAGEMENT@":kind["management"],"@FLASH@":str(data["flash_seconds"]),"@BOOT@":str(data["boot_seconds"]),"@AGENT@":records["agent_binary"]["sha256"],"@INTERFACE@":wifi["interface"],"@GATEWAY@":wifi["gateway"],"@SSID@":wifi["ssid"]}
    for key,value in replacements.items():guide=guide.replace(key,value)
    guide+="\nExact host commands, not executed by generation:\n"+"\n".join(k+": "+shlex.join(v) for k,v in commands.items())+"\n"
    (out/"procedure.txt").write_text(guide)
    sources=[Path(__file__),Path(__file__).with_suffix(".txt"),observer,HELPER]+[ROOT/p for p in ("go/internal/cli/commands/os_install_dragonwing.go","go/internal/cli/commands/os_install_dragonwing_config.go","go/internal/cli/commands/os_install_dragonwing_brief.go","go/internal/cli/qdl/flashdesc.go","go/internal/cli/qdl/firehose.go","go/internal/agent/configpartition/apply.go")]
    evidence={"audit_entries":["1998"],"hardware_execution":"not_run","inventory":data,"inventory_record":artifact(str(Path(inventory).resolve())),"records":records,"geometry":geometry,"data_blank_bytes":1048576,"builder_sources":provenance,"sources":[artifact(str(p)) for p in sources],"generated":[artifact(str(out/p)) for p in ("commands.json","marker-config.json",observer.name,"procedure.txt")],
              "cases":[{"id":name,"status":"not_run","observations":[],"reviewer":None} for name in ("target_bundle_and_consent","persisted_before_flash_nonce","actual_edl_config_and_data_writes","fresh_data_and_seeded_first_boot","second_boot_persistence","cleanup_or_recovery")],"scope":"This board and plain-ext4 UFS data only. No secure erase, encrypted/custom layout, another board, artifact extraction, device operation or physical result is claimed by generation."}
    (out/"evidence.json").write_text(json.dumps(evidence,indent=2)+"\n")
    return evidence


if __name__=="__main__":
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument("inventory",type=Path);parser.add_argument("output",type=Path)
    args=parser.parse_args();generate(args.inventory,args.output)
