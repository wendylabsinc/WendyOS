"""Pure checks for staged deployment and scene continuity; no runtime access."""
import ipaddress
import re
from urllib.parse import urlsplit
import uuid

from stage import APP, Refused, SERVICES


def selective_run_args(service):
    if service not in SERVICES:
        raise Refused("unknown service selection")
    return ["run", "--prefix", "/var/lib/agentmultimodal/project", "--builder", "buildkit", "--chunking", "force", "--detach", "--no-restart", "--skip-cloud-registration", "--yes", "--service", service]


def scene_receipt(plan, clock, nonce, endpoint, task, app_subnet):
    expected={"schema_version","run_id","boot_id","package_sha256","frame","clock_domain","anchor_unix_ns","simulation_time_ns","observed_unix_ns","observation_sequence","nonce"}
    if not isinstance(clock,dict) or set(clock)!=expected or clock["schema_version"]!="wendy.sim.clock.v1" or not re.fullmatch(r"[a-f0-9]{64}",nonce) or clock["nonce"]!=nonce:
        raise Refused("exact nonce-bound source clock required")
    pairs={"run_id":"run_id","package_sha256":"package_sha256","frame":"frame","clock_domain":"clock_domain","anchor_unix_ns":"anchor_unix_ns"}
    if any(clock[a]!=plan[b] for a,b in pairs.items()):
        raise Refused("scene differs from staged commissioning")
    try:
        if str(uuid.UUID(clock["boot_id"]))!=clock["boot_id"]:raise ValueError()
    except (ValueError,TypeError,AttributeError):
        raise Refused("canonical scene boot identity required") from None
    for key in ("simulation_time_ns","observed_unix_ns","observation_sequence"):
        minimum=0 if key=="simulation_time_ns" else 1
        if type(clock[key]) is not int or not minimum<=clock[key]<(1<<63):
            raise Refused("source observation range refused")
    if clock["simulation_time_ns"]>=(1<<63)-clock["anchor_unix_ns"]:
        raise Refused("source clock overflow")
    u=urlsplit(endpoint)
    try:
        address=ipaddress.IPv4Address(u.hostname)
        network=ipaddress.IPv4Network(app_subnet,strict=True)
        valid=u.scheme=="http" and u.port==8091 and not u.username and not u.password and not u.query and not u.fragment and not u.path and network.prefixlen==28 and address in network and address not in (network.network_address,network.broadcast_address)
    except (ValueError,TypeError):
        valid=False
    if not valid:
        raise Refused("exact scene endpoint within its owned CNI subnet required")
    fields={"container","app","service","pid","start_ticks","netns_inode","cgroup","ipv4","world_socket"}
    if not isinstance(task,dict) or set(task)!=fields or task["container"]!=APP+"_scene" or task["app"]!=APP or task["service"]!="scene" or task["world_socket"] or task["ipv4"]!=str(address):
        raise Refused("exact ordinary scene task without World socket required")
    if any(type(task[k]) is not int or task[k]<=1 for k in ("pid","start_ticks","netns_inode")) or not isinstance(task["cgroup"],str) or not task["cgroup"] or len(task["cgroup"])>4096:
        raise Refused("bounded live process identity required")
    return {"version":1,"kind":"wendy.agentmultimodal.scene.v1","scene_url":endpoint,"site":plan["site"],"app_id":APP,"run_id":clock["run_id"],"boot_id":clock["boot_id"],"package_sha256":clock["package_sha256"],"frame":dict(clock["frame"]),"anchor_unix_ns":clock["anchor_unix_ns"],"clock_domain":clock["clock_domain"],"simulation_time_ns":clock["simulation_time_ns"],"observed_unix_ns":clock["observed_unix_ns"],"observation_sequence":clock["observation_sequence"],"task":dict(task),"app_subnet":str(network)}


def scene_unchanged(before,after):
    for key in ("kind","scene_url","site","app_id","run_id","boot_id","package_sha256","frame","anchor_unix_ns","clock_domain","task","app_subnet"):
        if before.get(key)!=after.get(key):
            raise Refused("scene changed during agent restart or service deployment")
    if after["observation_sequence"]<=before["observation_sequence"] or after["simulation_time_ns"]<before["simulation_time_ns"] or after["observed_unix_ns"]<before["observed_unix_ns"]:
        raise Refused("scene continuity observation replayed or regressed")
    return True
