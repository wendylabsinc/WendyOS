"""Offline validation of the deployable SDK client; no DDS initialization."""

import importlib.util
import ctypes
import json
from pathlib import Path
import py_compile
import xml.etree.ElementTree as ET

from common import require
from verify_sources import main as verify_sdk


def main():
    verify_sdk()
    for source in Path(__file__).parent.glob("*.py"):
        py_compile.compile(str(source), doraise=True)
    require(importlib.util.find_spec("go2_sim") is None,
            "test image must not import the inherited simulator's Python modules")
    import unitree_api
    import unitree_go
    from unitree_api.msg import Request, Response
    from unitree_go.msg import SportModeState
    from unitree_sdk2py.idl.unitree_go.msg.dds_ import LowCmd_, LowState_
    from unitree_sdk2py.go2.sport.sport_client import SportClient
    require(all(str(Path(module.__file__).resolve()).startswith("/app/stock_ros/")
                for module in (unitree_api, unitree_go)), "ROS imports did not resolve to the stock overlay")
    require(not hasattr(SportModeState(), "path_point"), "VM client accidentally sourced derived simulator types")
    require(Response.get_fields_and_field_types()["binary"] == "sequence<int8>", "Response is not the stock ROS type")
    require(Request() is not None and LowCmd_ is not None and LowState_ is not None and SportClient is not None,
            "client message imports failed")
    ctypes.CDLL("/opt/ros/humble/lib/librmw_cyclonedds_cpp.so")
    root = ET.parse(Path(__file__).with_name("cyclonedds.vm.xml")).getroot()
    interfaces = root.findall(".//{*}NetworkInterface")
    peers = root.findall(".//{*}Peer")
    require(len(interfaces) == 1 and interfaces[0].get("name") == "lo", "ROS configuration must select only lo")
    require(len(peers) == 1 and peers[0].get("Address") == "127.0.0.1", "ROS discovery peer must be loopback")
    print(json.dumps({"offline_vm_client_validation": "passed", "ros_types": "stock unitree_ros2",
                      "sdk_sources_verified": True, "simulator_importable": False, "dds_initialized": False}), flush=True)


if __name__ == "__main__":
    main()
