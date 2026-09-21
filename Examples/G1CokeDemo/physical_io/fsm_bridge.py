"""Isolated high-level Unitree FSM bridge for the harnessed u2525 entry."""
from __future__ import annotations

import json
import os
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any


GET_FSM_ID_API = 7001
GET_FSM_MODE_API = 7002
CUSTOM_ACTION_STOP_API = 7113
RPC_API_NOT_IMPLEMENTED = 3203
DAMP_FSM = 1
LOCK_STAND_FSM = 4
RUNNING_FSM = 801
READY_MODES = (0, 3)
TRANSITION_TIMEOUT_S = 12.0
LOCK_STAND_SETTLE_S = 3.0
PORT = int(os.environ.get("G1_FSM_BRIDGE_PORT", "8100"))
INTERFACE = os.environ.get("G1_DDS_INTERFACE", "").strip()
ADAPTER_URL = os.environ.get("G1_ADAPTER_STATUS_URL", "http://127.0.0.1:8098").rstrip("/")


def _pair(result: Any) -> tuple[int, Any]:
    if not isinstance(result, (tuple, list)) or len(result) != 2:
        raise RuntimeError(f"unexpected Unitree RPC result: {result!r}")
    return int(result[0]), result[1]


def _result_code(result: Any) -> int:
    return 0 if result is None else int(result)


class FsmRuntime:
    def __init__(self) -> None:
        if not INTERFACE:
            raise RuntimeError("G1_DDS_INTERFACE is required")
        from unitree_sdk2py.comm.motion_switcher.motion_switcher_client import MotionSwitcherClient
        from unitree_sdk2py.core import channel as unitree_channel
        from .unitree_io import EXPLICIT_INTERFACE_DDS_CONFIG
        from unitree_sdk2py.g1.arm.g1_arm_action_client import G1ArmActionClient
        from unitree_sdk2py.g1.loco.g1_loco_client import LocoClient

        unitree_channel.ChannelConfigHasInterface = EXPLICIT_INTERFACE_DDS_CONFIG
        if unitree_channel.ChannelFactoryInitialize(0, INTERFACE) is False:
            raise RuntimeError(f"DDS initialization failed on {INTERFACE}")
        self.loco = LocoClient()
        self.switcher = MotionSwitcherClient()
        self.arm = G1ArmActionClient()
        for client in (self.loco, self.switcher, self.arm):
            client.SetTimeout(3.0)
            client.Init()
        self.arm._RegistApi(CUSTOM_ACTION_STOP_API, 0)
        self.lock = threading.Lock()

    def adapter_state(self) -> dict:
        with urllib.request.urlopen(ADAPTER_URL + "/state", timeout=2.0) as response:
            value = json.load(response)
        live = value.get("live") or {}
        io = value.get("io") or {}
        errors = [
            *(live.get("body_errors") or [1]),
            *(live.get("left_hand_errors") or [1]),
            *(live.get("right_hand_errors") or [1]),
            *(live.get("left_hand_system_errors") or [1]),
            *(live.get("right_hand_system_errors") or [1]),
        ]
        if (
            value.get("healthy") is not True
            or value.get("motion_capability") is not True
            or io.get("publishers_armed") is not False
            or live.get("mode_machine") != 6
            or (live.get("remote") or {}).get("neutral") is not True
            or any(int(error) != 0 for error in errors)
        ):
            raise RuntimeError("adapter live-state interlock is not ready")
        return value

    def read_fsm(self) -> dict:
        id_code, id_data = _pair(self.loco._Call(GET_FSM_ID_API, "{}"))
        if id_code != 0 or not id_data:
            raise RuntimeError(f"FSM ID readback failed with code {id_code}")
        mode_code, mode_data = _pair(self.loco._Call(GET_FSM_MODE_API, "{}"))
        if mode_code != 0 or not mode_data:
            raise RuntimeError(f"FSM mode readback failed with code {mode_code}")
        return {
            "fsm_id": int(json.loads(id_data)["data"]),
            "fsm_mode": int(json.loads(mode_data)["data"]),
        }

    def stop(self) -> dict:
        action_code, _ = _pair(self.arm._Call(CUSTOM_ACTION_STOP_API, ""))
        if action_code not in (0, RPC_API_NOT_IMPLEMENTED):
            raise RuntimeError(f"custom action stop rejected with code {action_code}")
        move_code = _result_code(self.loco.StopMove())
        if move_code != 0:
            raise RuntimeError(f"StopMove rejected with code {move_code}")
        return {
            "custom_action_stop_code": action_code,
            "custom_action_stop_supported": action_code == 0,
            "stop_move_code": move_code,
        }

    def _wait_adapter_settled(self) -> None:
        good_since: float | None = None
        deadline = time.monotonic() + 3.0
        while time.monotonic() < deadline:
            value = self.adapter_state()
            speed = float((value.get("alignment") or {}).get("maximum_speed_rad_s", 99.0))
            now = time.monotonic()
            if speed <= 0.03:
                good_since = now if good_since is None else good_since
                if now - good_since >= 0.25:
                    return
            else:
                good_since = None
            time.sleep(0.025)
        raise RuntimeError("commanded joints did not settle after StopMove")

    def _select_ai(self) -> dict:
        before_code, before = _pair(self.switcher.CheckMode())
        select_code, _ = _pair(self.switcher.SelectMode("ai"))
        if select_code != 0:
            raise RuntimeError(f"AI motion mode rejected with code {select_code}")
        time.sleep(0.5)
        after_code, after = _pair(self.switcher.CheckMode())
        if after_code != 0 or not isinstance(after, dict) or after.get("name") != "ai":
            raise RuntimeError(f"AI mode readback failed: code={after_code}, mode={after!r}")
        return {"before_code": before_code, "before": before, "after": after}

    def _transition(self, target: int, source: int) -> dict:
        current = self.read_fsm()
        if current["fsm_id"] != source:
            raise RuntimeError(f"FSM {target} requires source {source}; current={current['fsm_id']}")
        code = _result_code(self.loco.SetFsmId(target))
        if code != 0:
            raise RuntimeError(f"FSM {target} request rejected with code {code}")
        deadline = time.monotonic() + TRANSITION_TIMEOUT_S
        while time.monotonic() < deadline:
            self.adapter_state()
            current = self.read_fsm()
            if current["fsm_id"] == target:
                return current
            if current["fsm_id"] != source:
                raise RuntimeError(f"FSM diverged while targeting {target}: {current['fsm_id']}")
            time.sleep(0.25)
        raise RuntimeError(f"FSM {target} was not confirmed before timeout")

    def arm_running(self) -> dict:
        with self.lock:
            self.adapter_state()
            before = self.read_fsm()
            if before["fsm_id"] == RUNNING_FSM:
                if before["fsm_mode"] not in READY_MODES:
                    raise RuntimeError(f"RUNNING FSM mode is not ready: {before['fsm_mode']}")
                stopped = self.stop()
                return {"before": before, "after": before, "transitions": [], "stop": stopped}
            if before["fsm_id"] != 500:
                raise RuntimeError(f"FSM entry requires source 500 or ready 801; current={before['fsm_id']}")
            stopped = self.stop()
            self._wait_adapter_settled()
            ai_mode = self._select_ai()
            transitions = [self._transition(DAMP_FSM, 500), self._transition(LOCK_STAND_FSM, DAMP_FSM)]
            deadline = time.monotonic() + LOCK_STAND_SETTLE_S
            while time.monotonic() < deadline:
                self.adapter_state()
                time.sleep(0.025)
            try:
                transitions.append(self._transition(RUNNING_FSM, LOCK_STAND_FSM))
            except RuntimeError:
                if self.read_fsm()["fsm_id"] != LOCK_STAND_FSM:
                    raise
                self.stop()
                self._select_ai()
                time.sleep(LOCK_STAND_SETTLE_S)
                transitions.append(self._transition(RUNNING_FSM, LOCK_STAND_FSM))
            after = self.read_fsm()
            if after["fsm_id"] != RUNNING_FSM or after["fsm_mode"] not in READY_MODES:
                raise RuntimeError(f"RUNNING FSM readback rejected: {after}")
            return {
                "before": before,
                "after": after,
                "transitions": transitions,
                "stop": stopped,
                "ai_mode": ai_mode,
            }


def make_server(runtime: FsmRuntime, host: str = "127.0.0.1", port: int = PORT):
    from coke_demo.access import require_loopback
    require_loopback(host)
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            return

        def do_GET(self):
            if self.path.split("?", 1)[0] != "/state":
                return self.reply(404, {"error": "not found"})
            try:
                self.reply(200, {"healthy": True, **runtime.read_fsm()})
            except Exception as exc:
                self.reply(503, {"healthy": False, "error": f"{type(exc).__name__}: {exc}"})

        def do_POST(self):
            path = self.path.split("?", 1)[0]
            if path not in ("/api/arm-running", "/api/stop"):
                return self.reply(404, {"error": "not found"})
            try:
                length = int(self.headers.get("Content-Length", "0"))
                body = json.loads(self.rfile.read(length))
                if set(body) != {"schema", "command_id", "operator_confirmed"}:
                    raise ValueError("unexpected request fields")
                if body["operator_confirmed"] is not True or not 16 <= len(str(body["command_id"])) <= 64:
                    raise ValueError("operator confirmation and command id are required")
                expected = "wendy.g1.fsm-arm-running-request.v1" if path.endswith("arm-running") else "wendy.g1.fsm-stop-request.v1"
                if body["schema"] != expected:
                    raise ValueError("unexpected request schema")
                value = runtime.arm_running() if path.endswith("arm-running") else runtime.stop()
                self.reply(200, {"accepted": True, **value})
            except Exception as exc:
                try:
                    runtime.stop()
                except Exception:
                    # Preserve the original request error if best-effort stop fails.
                    pass
                self.reply(409, {"accepted": False, "error": f"{type(exc).__name__}: {exc}"})

        def reply(self, status: int, value: dict):
            body = json.dumps(value, allow_nan=False, separators=(",", ":")).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

    return ThreadingHTTPServer((host, port), Handler)


def main() -> None:
    runtime = FsmRuntime()
    server = make_server(runtime)
    try:
        server.serve_forever()
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
