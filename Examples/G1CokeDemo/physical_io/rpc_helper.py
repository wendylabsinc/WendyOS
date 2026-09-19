"""Fresh-process Unitree locomotion RPCs used by physical preflight/stop."""
from __future__ import annotations

import argparse
import json
from typing import Any

GET_FSM_ID_API = 7001
GET_FSM_MODE_API = 7002


def _pair(result: Any) -> tuple[int, Any]:
    if not isinstance(result, (tuple, list)) or len(result) != 2:
        raise RuntimeError(f"unexpected Unitree RPC result: {result!r}")
    return int(result[0]), result[1]


def run(operation: str, interface: str, timeout_s: float) -> dict:
    from unitree_sdk2py.core import channel as unitree_channel
    from .unitree_io import EXPLICIT_INTERFACE_DDS_CONFIG
    from unitree_sdk2py.g1.loco.g1_loco_client import LocoClient

    unitree_channel.ChannelConfigHasInterface = EXPLICIT_INTERFACE_DDS_CONFIG
    if unitree_channel.ChannelFactoryInitialize(0, interface) is False:
        raise RuntimeError(f"DDS initialization failed on {interface}")
    client = LocoClient()
    client.SetTimeout(timeout_s)
    client.Init()
    if operation == "stop-move":
        result = client.StopMove()
        return {"code": 0 if result is None else int(result)}
    id_code, id_data = _pair(client._Call(GET_FSM_ID_API, "{}"))
    if id_code != 0 or not id_data:
        return {"code": id_code, "error": "FSM ID readback failed"}
    mode_code, mode_data = _pair(client._Call(GET_FSM_MODE_API, "{}"))
    if mode_code != 0 or not mode_data:
        return {"code": mode_code, "error": "FSM mode readback failed"}
    return {
        "code": 0,
        "fsm_id": int(json.loads(id_data)["data"]),
        "fsm_mode": int(json.loads(mode_data)["data"]),
    }


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("operation", choices=("read-fsm", "stop-move"))
    parser.add_argument("--interface", required=True)
    parser.add_argument("--timeout-s", required=True, type=float)
    args = parser.parse_args()
    try:
        result = run(args.operation, args.interface, args.timeout_s)
    except Exception as exc:
        result = {"code": -1, "error": f"{type(exc).__name__}: {exc}"}
    print(json.dumps(result, separators=(",", ":")), flush=True)


if __name__ == "__main__":
    main()
