"""Build ordinary app configuration from observed scene and public test identity."""
import base64
import hashlib
import ipaddress

from stage import APP, Refused, SENSORS


def commission_spec(scene):
    keys=("scene_url","run_id","boot_id","package_sha256","frame","anchor_unix_ns","site","app_id")
    return {**{key:scene[key] for key in keys},"isolated_simulation_network":True,"read_token_file":"/etc/wendy-agent/.vm-fixture/scene-read-token"}


def configurations(plan,scene,public,source_ip):
    try:
        address=ipaddress.IPv4Address(source_ip)
        root=base64.b64decode(public["root_der"],validate=True)
    except (ValueError,KeyError,TypeError):
        raise Refused("actual source address and public fixture roots required") from None
    if not address.is_private or not root or len(root)>64<<10 or hashlib.sha256(root).hexdigest()!=public["root_sha256"] or public["principal"]!="spiffe://wendy.sh/tenant/"+public["tenant"]+"/device/"+public["device_id"]:
        raise Refused("public fixture identity or source endpoint mismatch")
    result={}
    for service in ("camera-a","gas","audio"):
        result[service]={"scene_url":scene["scene_url"],"read_token_file":"/config/scene-read-token","run_id":plan["run_id"],"site":plan["site"],"frame":plan["frame"],"sensor_id":SENSORS[service],"simulation_anchor_unix_ns":plan["anchor_unix_ns"],"duration_ms":120000,"max_samples":64,"poll_interval_ms":100,"ttl_ms":3000,"request_timeout_ms":1500}
    commission={"run_id":scene["run_id"],"boot_id":scene["boot_id"],"package_sha256":scene["package_sha256"],"site":scene["site"],"frame":scene["frame"],"sensor_id":"camera-b","simulation_anchor_unix_ns":scene["anchor_unix_ns"]}
    common={"commission":commission,"bounds":{"duration_ms":120000,"max_frames":32,"request_timeout_ms":1500,"ttl_ms":3000},"isolated_simulation_network":True,"metadata_read_token_file":"/config/metadata-read-token","channel_id":4}
    result["sensorlink-source"]={**common,"scene_url":scene["scene_url"],"scene_read_token_file":"/config/scene-read-token","tcp_listen":"0.0.0.0:50060","metadata_listen":"0.0.0.0:50061","chunk_bytes":16384,"jpeg_quality":80,"poll_interval_ms":500}
    result["sensorlink-bridge"]={**common,"tcp_address":str(address)+":50060","metadata_url":"http://"+str(address)+":50061"}
    result["fusion"]={"version":1,"tenant":public["tenant"],"site":plan["site"],"device":public["principal"],"app":APP,"service":"fusion","roots_der":[public["root_der"]],"run_id":plan["run_id"],"scene_boot_id":scene["boot_id"],"package_sha256":scene["package_sha256"],"simulation_anchor_unix_ns":scene["anchor_unix_ns"],"frame":plan["frame"],"scenario_version":plan["scenario_version"],"seed":plan["seed"],"sources":[{"sensor_id":sensor,"app":APP,"service":service} for service,sensor in SENSORS.items()],"result_stream":"hazard","duration_ms":120000,"request_timeout_ms":1000,"poll_interval_ms":500,"max_attempts":16,"retain_for_ms":60000,"clock_max_width_ms":5000,"max_skew_ms":350}
    return result
