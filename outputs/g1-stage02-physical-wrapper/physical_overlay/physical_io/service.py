"""Motion-disabled-first live-state service for the Stage 2 adapter."""
from __future__ import annotations

import hashlib
import json
import os
import re
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.parse import parse_qs, urlsplit

import numpy as np

from .unitree_io import (
    COMMAND_TARGET_INDICES,
    COMMAND_TARGET_LOWER_RAD,
    COMMAND_TARGET_UPPER_RAD,
    HAND_MAPPING_ID,
    CommandProfile,
    ExactPolicyUnitreeIO,
    InterlockError,
    IoConfig,
)

BUNDLE = Path(os.environ.get("G1_POLICY_BUNDLE", "/bundle"))
PORT = int(os.environ.get("G1_PHYSICAL_PORT", "8098"))
REFERENCE_SHA256 = "05fd43a10a2968141aaba140a82cc39ce683aa19418cc24938bea29f6aae5002"
CHECKPOINT_SHA256 = "3aaf0c8279e9190a6d47740f6dfdd427ff4fb495ab7e1ae11334e1e1d3573738"
RUN_LOG_DIR = Path(os.environ.get("G1_RUN_LOG_DIR", "/var/lib/wendy/g1-stage02-runs"))
COMMAND_ID_PATTERN = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{15,63}")
COMMAND_POLICY_INDICES = (*range(12, 22), *range(29, 43))
ENTRY_POSITION_LIMIT_RAD = float(
    os.environ.get("G1_ENTRY_POSITION_LIMIT_RAD", "0.03")
)
ENTRY_SPEED_LIMIT_RAD_S = float(
    os.environ.get("G1_ENTRY_SPEED_LIMIT_RAD_S", "0.03")
)
# Dex3 velocity feedback is quantized in repeatable 0.1145 rad/s impulses even
# while its position is stationary.  Keep the 0.03 rad/s body/waist gate and
# admit only that measured hand-feedback floor during settled-dwell checks.
ENTRY_HAND_SPEED_LIMIT_RAD_S = float(
    os.environ.get("G1_ENTRY_HAND_SPEED_LIMIT_RAD_S", "0.15")
)
RUNNING_FSM = int(os.environ.get("G1_REQUIRED_FSM_ID", "801"))
READY_FSM_MODES = (0, 3)
DAMP_FSM = 1
LOCK_STAND_FSM = 4
CANARY_DELTA_RAD = 0.02
CANARY_HOLD_STEPS = 40
RAMP_STEPS = 40
TICK_S = 0.025
ENTRY_OWNERSHIP_RAMP_S = 10.0
ENTRY_OWNERSHIP_RAMP_STEPS = round(ENTRY_OWNERSHIP_RAMP_S / TICK_S)
TIMING_RETRY_DELAY_S = 0.005
FSM_TRANSITION_TIMEOUT_S = 12.0
FSM_TRANSITION_POLL_S = 0.25
LOCK_STAND_SETTLE_S = 3.0
ENTRY_MINIMUM_DURATION_S = 6.0
ENTRY_MAXIMUM_DURATION_S = 20.0
ENTRY_MAXIMUM_VELOCITY_RAD_S = 0.18
ENTRY_MAXIMUM_STEP_RAD = float(
    os.environ.get("G1_ENTRY_MAXIMUM_STEP_RAD", "0.02")
)
ENTRY_TRACKING_ABORT_RAD = float(
    os.environ.get("G1_ENTRY_TRACKING_ABORT_RAD", "0.10")
)
if not 0.03 <= ENTRY_TRACKING_ABORT_RAD <= 0.22:
    raise ValueError("G1_ENTRY_TRACKING_ABORT_RAD must be between 0.03 and 0.22")
ENTRY_STATIC_COMPENSATION_STEP_RAD = 0.0005
ENTRY_STATIC_COMPENSATION_LIMIT_RAD = 0.04
ENTRY_STATIC_COMPENSATION_DEADBAND_RAD = 0.0225
ENTRY_SETTLE_S = 0.25
ENTRY_SETTLE_TIMEOUT_S = 8.0

COMMAND_LOWER_BY_INDEX = dict(zip(COMMAND_TARGET_INDICES, COMMAND_TARGET_LOWER_RAD, strict=True))
COMMAND_UPPER_BY_INDEX = dict(zip(COMMAND_TARGET_INDICES, COMMAND_TARGET_UPPER_RAD, strict=True))

CONTROL_UI_HTML = r"""<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>G1 Stage 2 Policy</title><style>
:root{color-scheme:dark;font-family:ui-sans-serif,system-ui,-apple-system,sans-serif}body{margin:0;background:#0b0f14;color:#e8edf2}main{max-width:760px;margin:0 auto;padding:32px 20px 48px}h1{font-size:28px;margin:0 0 6px}.sub{color:#96a3b3;margin:0 0 24px}.card{background:#131a22;border:1px solid #263241;border-radius:14px;padding:18px;margin:14px 0}.status{display:flex;align-items:center;gap:10px;font-weight:700}.dot{width:11px;height:11px;border-radius:50%;background:#f0b429;box-shadow:0 0 12px currentColor}.dot.ready{background:#3ddc97}.dot.active{background:#4da3ff}.dot.fault{background:#ff5c6c}dl{display:grid;grid-template-columns:1fr auto;gap:10px 18px;margin:18px 0 0}dt{color:#96a3b3}dd{margin:0;font-variant-numeric:tabular-nums}.actions{display:grid;grid-template-columns:repeat(3,1fr);gap:12px;margin-top:18px}button{border:0;border-radius:11px;padding:15px 18px;font-size:16px;font-weight:800;cursor:pointer}button:disabled{cursor:not-allowed;opacity:.42}#run{background:#3ddc97;color:#06130d}#retreat{background:#78b7ff;color:#06111b}#stop{background:#ffcf5c;color:#1b1300}.warning{color:#ffcf5c;line-height:1.45}pre{white-space:pre-wrap;overflow-wrap:anywhere;color:#b8c4d1;font-size:12px;max-height:230px;overflow:auto}.vision{position:relative;width:100%;aspect-ratio:4/3;background:#080b0f;border-radius:10px;overflow:hidden;margin-top:14px}.vision img,.vision svg{position:absolute;inset:0;width:100%;height:100%;object-fit:contain}.ideal{fill:none;stroke:#ff4fd8;stroke-width:3}.detected{fill:none;stroke:#41f0a8;stroke-width:3}.delta{stroke:#fff;stroke-width:2;stroke-dasharray:5 4}.legend{display:flex;gap:18px;color:#96a3b3;font-size:13px;margin-top:10px}.swatch{display:inline-block;width:10px;height:10px;border-radius:50%;margin-right:6px}.pink{background:#ff4fd8}.green{background:#41f0a8}.download{color:#78b7ff;font-size:13px}
</style></head><body><main><h1>G1 Stage 2 Policy</h1><p class="sub">Exact 316-input → 15-action GRU · 40 Hz policy contract · maximum 1,200 steps</p>
<section class="card"><div class="status"><span id="dot" class="dot"></span><span id="headline">Connecting…</span></div><dl><dt>Active policy</dt><dd id="policy">—</dd><dt>Policy steps</dt><dd id="steps">—</dd><dt>Effective rate</dt><dd id="hz">—</dd><dt>Step limit</dt><dd id="stepLimit">—</dd><dt>Tracking limit</dt><dd id="trackingLimit">—</dd><dt>DDS writes</dt><dd id="writes">—</dd><dt>State skew</dt><dd id="skew">—</dd></dl><div class="actions"><button id="run" disabled>Play policy</button><button id="retreat" disabled>Safe retreat</button><button id="stop" disabled>Controlled release</button></div></section>
<section class="card"><strong>Can alignment</strong><div class="vision"><img id="camera" alt="Live Coke segmentation"><svg viewBox="0 0 320 240" preserveAspectRatio="xMidYMid meet"><line id="delta" class="delta" visibility="hidden"/><circle id="ideal" class="ideal" r="10" visibility="hidden"/><path id="cross" class="ideal" d="M-15 0H15M0-15V15" visibility="hidden"/><circle id="detected" class="detected" r="8" visibility="hidden"/></svg></div><div class="legend"><span><i class="swatch pink"></i>Ideal nominal can</span><span><i class="swatch green"></i>Detected can</span></div><p id="alignmentText" class="sub">Waiting for live RGB-D projection…</p><a id="alignmentDownload" class="download" hidden>Download this run's alignment trace</a></section>
<section class="card warning">Only press Run while the G1 is harnessed, the workspace is clear, and the safety observer is ready. A fault holds the measured pose at SDK weight 1 until Controlled stop / release is pressed.</section><section class="card"><strong>Last event</strong><pre id="event">Waiting for state…</pre></section>
</main><script>
const el=id=>document.getElementById(id);let current=null,activeCommand=null,requestBusy=false;const commandId=()=>`operator-ui-${Date.now()}`;
function mark(id,point){const node=el(id);if(!point){node.setAttribute('visibility','hidden');if(id==='ideal')el('cross').setAttribute('visibility','hidden');return}node.setAttribute('visibility','visible');node.setAttribute('cx',point[0]);node.setAttribute('cy',point[1]);if(id==='ideal')el('cross').setAttribute('transform',`translate(${point[0]} ${point[1]})`),el('cross').setAttribute('visibility','visible')}
async function refreshAlignment(){const q=current?.live?.q_43||[];if(q.length!==43)return;const base=`http://${location.hostname}:8117`;el('camera').src=`http://${location.hostname}:8003/preview.jpg?t=${Date.now()}`;try{const url=`${base}/retarget/project?yaw=${encodeURIComponent(q[12])}&roll=${encodeURIComponent(q[13])}&pitch=${encodeURIComponent(q[14])}`;const value=await(await fetch(url,{cache:'no-store'})).json(),ideal=value.ideal_pixel_xy,detected=value.detected_pixel_xy;mark('ideal',ideal);mark('detected',detected);const line=el('delta');if(ideal&&detected){line.setAttribute('x1',ideal[0]);line.setAttribute('y1',ideal[1]);line.setAttribute('x2',detected[0]);line.setAttribute('y2',detected[1]);line.setAttribute('visibility','visible')}else line.setAttribute('visibility','hidden');const distance=value.distance_to_optimal_cm;el('alignmentText').textContent=detected&&distance!=null?`${Number(distance).toFixed(1)} cm from optimal · ${Number(value.pixel_displacement_px||0).toFixed(0)} px · ${Number(value.detection_age_ms||0).toFixed(0)} ms old`:'Ideal shown; no fresh Coke mask';const download=el('alignmentDownload');if(activeCommand){download.href=`/api/reference-residual/can-alignment?command_id=${encodeURIComponent(activeCommand)}`;download.hidden=false}}catch(error){el('alignmentText').textContent=`Alignment overlay unavailable: ${error}`}}
async function refresh(){try{const response=await fetch('/state',{cache:'no-store'});current=await response.json();const p=current.policy_runtime||{},io=current.io||{},policy=p.active_policy||p.camera?.active_policy||{},retreat=p.safe_retreat||{};activeCommand=p.command_id||activeCommand;const fault=Boolean(p.last_error),holding=Boolean(p.holding_after_fault),running=Boolean(p.running),inferenceReady=p.camera?.inference_healthy!==false,ready=current.healthy&&inferenceReady&&!running&&!holding&&!io.publishers_armed;el('dot').className=`dot ${holding||fault?'fault':running?'active':ready?'ready':''}`;el('headline').textContent=retreat.active?`Retreating ${retreat.waypoints_completed||0} / ${retreat.waypoints_total||0}`:holding?'Fault hold active — retreat or release':running?'Policy running':ready?'Ready to run':'Not ready';el('policy').textContent=policy.candidate_id?`${policy.candidate_id} · ${String(policy.checkpoint_sha256||'').slice(0,12)}`:'—';el('steps').textContent=`${p.policy_steps_completed||0} / ${p.policy_steps_total||1200}`;el('hz').textContent=`${Number(p.effective_policy_hz||0).toFixed(2)} Hz`;el('stepLimit').textContent=`${Number(p.maximum_policy_step_rad||0).toFixed(3)} rad`;el('trackingLimit').textContent=`${Number(p.maximum_policy_tracking_error_rad||0).toFixed(3)} rad`;el('writes').textContent=io.dds_writes_accepted??0;el('skew').textContent=`${(Number(current.live?.state_skew_s||0)*1000).toFixed(2)} ms`;el('run').disabled=requestBusy||!ready;el('retreat').disabled=requestBusy||!holding||!retreat.available||retreat.active;el('stop').disabled=requestBusy||!(running||holding||io.publishers_armed);if(p.last_error&&!retreat.active)el('event').textContent=p.last_error;else if(!requestBusy)el('event').textContent=retreat.active?'Reversing the commanded path; SDK weight remains 1':running?`Running ${activeCommand}`:'No active fault';await refreshAlignment()}catch(error){el('headline').textContent='G1 wrapper unreachable';el('dot').className='dot fault';el('run').disabled=true;el('retreat').disabled=true;el('stop').disabled=true;el('event').textContent=String(error)}}
el('run').onclick=async()=>{requestBusy=true;el('event').textContent='Checking robot, inference, active contract, and camera…';await refresh();try{const checkResponse=await fetch('/api/reference-residual/preflight',{cache:'no-store'}),check=await checkResponse.json();if(!checkResponse.ok||check.accepted!==true)throw new Error(check.error||JSON.stringify(check));const label=check.active_policy?.candidate_id||'active policy',checkpoint=check.active_policy?.checkpoint_sha256;if(!checkpoint)throw new Error('Preflight did not pin an active checkpoint');if(!confirm(`Preflight passed for ${label}. Confirm the G1 is harnessed, clear, and supervised. Start one bounded policy run?`))return;activeCommand=commandId();el('event').textContent=`Starting ${activeCommand}…`;const maximumSteps=Math.min(1200,Number(check.reference_frames||1200));const response=await fetch('/api/reference-residual/run-policy',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({schema:'wendy.g1.reference-residual-physical-run-request.v1',command_id:activeCommand,operator_confirmed:true,maximum_policy_steps:maximumSteps,expected_checkpoint_sha256:checkpoint})});const value=await response.json();el('event').textContent=JSON.stringify(value,null,2);if(!response.ok)throw new Error(value.error||`Run rejected (${response.status})`)}catch(error){el('event').textContent=String(error)}finally{requestBusy=false;refresh()}};
el('retreat').onclick=async()=>{if(!activeCommand)return;requestBusy=true;try{if(!confirm('Reverse the actual commanded path slowly while keeping SDK weight at 1? It will hold at the entry pose and will not auto-release.'))return;const response=await fetch('/api/reference-residual/retreat',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({schema:'wendy.g1.reference-residual-safe-retreat-request.v1',command_id:activeCommand,operator_confirmed:true})});const value=await response.json();el('event').textContent=JSON.stringify(value,null,2);if(!response.ok)throw new Error(value.error||`Retreat rejected (${response.status})`)}catch(error){el('event').textContent=String(error)}finally{requestBusy=false;refresh()}};
el('stop').onclick=async()=>{if(!activeCommand)return;requestBusy=true;try{const response=await fetch('/api/reference-residual/stop',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({schema:'wendy.g1.reference-residual-physical-stop-request.v1',command_id:activeCommand,operator_confirmed:true})});el('event').textContent=JSON.stringify(await response.json(),null,2)}catch(error){el('event').textContent=String(error)}finally{requestBusy=false;refresh()}};refresh();setInterval(refresh,1000);
</script></body></html>"""


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


class PhysicalProbeRuntime:
    """Subscribe to live Unitree state without constructing command publishers."""

    def __init__(self, io: ExactPolicyUnitreeIO | None = None) -> None:
        reference_path = BUNDLE / "reference-contract.npz"
        if _sha256(reference_path) != REFERENCE_SHA256:
            raise ValueError("reference contract SHA-256 mismatch")
        with np.load(reference_path, allow_pickle=False) as values:
            self.reference_frame0 = values["reference_joint_targets_43"][0].astype(float)
        motion_enabled = os.environ.get("G1_MOTION_ENABLED", "false").lower() == "true"
        self.motion_enabled = motion_enabled
        self.interface = os.environ.get("G1_DDS_INTERFACE", "").strip()
        self.fsm_bridge_url = os.environ.get("G1_FSM_BRIDGE_URL", "").rstrip("/")
        allowed_mode = os.environ.get("G1_ALLOWED_MODE_MACHINE", "").strip()
        watchdog_s = float(os.environ.get("G1_COMMAND_WATCHDOG_S", "0.075"))
        maximum_state_skew_s = float(
            os.environ.get("G1_MAXIMUM_STATE_SKEW_S", "0.020")
        )
        self.maximum_policy_tracking_error_rad = float(
            os.environ.get("G1_MAXIMUM_POLICY_TRACKING_ERROR_RAD", "0.10")
        )
        self.io = io or ExactPolicyUnitreeIO(IoConfig(
            interface=self.interface,
            motion_enabled=motion_enabled,
            verified_hand_mapping_id=HAND_MAPPING_ID if motion_enabled else "",
            allowed_mode_machine=(int(allowed_mode),) if allowed_mode else (),
            watchdog_timeout_s=watchdog_s,
            maximum_state_skew_s=maximum_state_skew_s,
        ))
        self.started_at_unix_ns = time.time_ns()
        self.lock = threading.Lock()
        self.last_error: str | None = None
        self.command_lock = threading.Lock()
        self.command_results: dict[str, dict] = {}
        self.policy_runner: Any | None = None
        self.io.start()

    def attach_policy_runner(self, runner: Any) -> None:
        """Attach the GPU policy seam without importing torch in probe mode."""
        if self.policy_runner is not None:
            raise RuntimeError("physical policy runner is already attached")
        self.policy_runner = runner

    def run_integrated_policy(
        self,
        command_id: str,
        *,
        maximum_policy_steps: int,
        expected_checkpoint_sha256: str | None = None,
        execution_hz: float | None = None,
    ) -> dict:
        if self.policy_runner is None:
            raise InterlockError("exact physical policy runner is not loaded")
        return self.policy_runner.run(
            command_id,
            maximum_policy_steps=maximum_policy_steps,
            expected_checkpoint_sha256=expected_checkpoint_sha256,
            execution_hz=execution_hz,
        )

    @staticmethod
    def _validated_command_id(command_id: str) -> str:
        if COMMAND_ID_PATTERN.fullmatch(command_id) is None:
            raise ValueError("invalid command id")
        return command_id

    def can_alignment_runs(self) -> dict[str, Any]:
        RUN_LOG_DIR.mkdir(parents=True, exist_ok=True)
        runs = []
        for path in RUN_LOG_DIR.glob("*.can-alignment.jsonl"):
            command_id = path.name.removesuffix(".can-alignment.jsonl")
            if COMMAND_ID_PATTERN.fullmatch(command_id) is None:
                continue
            stat = path.stat()
            runs.append(
                {
                    "command_id": command_id,
                    "bytes": stat.st_size,
                    "updated_at_unix_ns": stat.st_mtime_ns,
                    "download_path": (
                        "/api/reference-residual/can-alignment?command_id=" + command_id
                    ),
                }
            )
        runs.sort(key=lambda value: value["updated_at_unix_ns"], reverse=True)
        return {"schema": "wendy.g1.can-alignment-run-index.v1", "runs": runs}

    def can_alignment_trace(self, command_id: str) -> str:
        command_id = self._validated_command_id(command_id)
        path = RUN_LOG_DIR / f"{command_id}.can-alignment.jsonl"
        if not path.is_file():
            raise FileNotFoundError("can-alignment trace not found")
        if path.stat().st_size > 16 * 1024 * 1024:
            raise ValueError("can-alignment trace exceeds download limit")
        return path.read_text(encoding="utf-8")

    def preflight_integrated_policy(self) -> dict:
        """Run the complete motion-zero Play-policy admission check."""
        if self.policy_runner is None:
            raise InterlockError("exact physical policy runner is not loaded")
        return self.policy_runner.preflight()

    def stop_integrated_policy(self, command_id: str) -> dict:
        """Request a pose-latched release without a high-level arm action."""
        if self.policy_runner is None:
            raise InterlockError("exact physical policy runner is not loaded")
        return self.policy_runner.request_stop(command_id)

    def retreat_integrated_policy(self, command_id: str) -> dict:
        """Request reversal of the sent joint path while SDK ownership is held."""
        if self.policy_runner is None:
            raise InterlockError("exact physical policy runner is not loaded")
        return self.policy_runner.request_retreat(command_id)

    @staticmethod
    def command_profile() -> CommandProfile:
        payload = {
            "source": "PROD-wendylabs.g1-demo-recorder-0.9.136-physically-exercised-canary",
            "upper_kp": [60.0] * 17,
            "upper_kd": [1.5] * 17,
            "hand_kp": [2.0] * 7,
            "hand_kd": [0.1] * 7,
            "arm_sdk_weight": 1.0,
        }
        digest = hashlib.sha256(
            json.dumps(payload, sort_keys=True, separators=(",", ":")).encode()
        ).hexdigest()
        return CommandProfile(
            profile_id=payload["source"],
            profile_sha256=digest,
            hardware_qualified=True,
            upper_kp=tuple(payload["upper_kp"]),
            upper_kd=tuple(payload["upper_kd"]),
            hand_kp=tuple(payload["hand_kp"]),
            hand_kd=tuple(payload["hand_kd"]),
            arm_sdk_weight=1.0,
        )

    def _rpc(self, operation: str) -> dict:
        if self.fsm_bridge_url:
            if operation == "read-fsm":
                with urllib.request.urlopen(self.fsm_bridge_url + "/state", timeout=5.0) as response:
                    value = json.load(response)
                if value.get("healthy") is not True:
                    raise RuntimeError(f"FSM bridge is unhealthy: {value}")
                return {
                    "code": 0,
                    "fsm_id": int(value["fsm_id"]),
                    "fsm_mode": int(value["fsm_mode"]),
                }
            if operation == "stop-move":
                value = self._bridge_post(
                    "/api/stop",
                    "wendy.g1.fsm-stop-request.v1",
                    f"stop-{time.time_ns()}",
                )
                return {"code": 0, "bridge": value}
        if operation == "read-fsm":
            fsm_id, fsm_mode = self.io.read_fsm()
            return {"code": 0, "fsm_id": fsm_id, "fsm_mode": fsm_mode}
        if operation == "stop-move":
            self.io.stop_move()
            return {"code": 0}
        raise ValueError(f"unsupported Unitree RPC operation: {operation}")

    def _bridge_post(self, path: str, schema: str, command_id: str) -> dict:
        payload = json.dumps(
            {"schema": schema, "command_id": command_id, "operator_confirmed": True},
            separators=(",", ":"),
        ).encode()
        request = urllib.request.Request(
            self.fsm_bridge_url + path,
            method="POST",
            data=payload,
            headers={"Content-Type": "application/json"},
        )
        with urllib.request.urlopen(request, timeout=45.0) as response:
            value = json.load(response)
        if value.get("accepted") is not True:
            raise RuntimeError(f"FSM bridge rejected request: {value}")
        return value

    @staticmethod
    def _controlled(values) -> np.ndarray:
        return np.asarray(values, dtype=float)[np.asarray(COMMAND_POLICY_INDICES)]

    @staticmethod
    def _entry_velocity_status(values) -> tuple[bool, float, float]:
        speeds = np.abs(np.asarray(values, dtype=float))
        body_max = float(np.max(speeds[np.asarray((*range(12, 22), *range(29, 36)))]))
        hand_max = float(np.max(speeds[np.asarray(tuple(range(36, 43)))]))
        settled = (
            body_max <= ENTRY_SPEED_LIMIT_RAD_S
            and hand_max <= ENTRY_HAND_SPEED_LIMIT_RAD_S
        )
        return settled, body_max, hand_max

    def _live_guard(
        self,
        *,
        expected_remote_sequence: int | None = None,
        require_settled: bool = False,
    ) -> dict:
        state = self.io.snapshot()
        if state["remote"]["neutral"] is not True:
            raise InterlockError("wireless remote is not neutral")
        # ``button_sequence`` is retained as evidence, but a completed remote
        # pulse no longer aborts motion after the remote is neutral again.
        # Current non-neutral input remains an immediate interlock above.
        if state["mode_machine"] not in self.io.config.allowed_mode_machine:
            raise InterlockError(f"mode_machine changed to {state['mode_machine']}")
        if any(
            any(int(value) != 0 for value in state[key])
            for key in (
                "body_errors", "left_hand_errors", "right_hand_errors",
                "left_hand_system_errors", "right_hand_system_errors",
            )
        ):
            raise InterlockError("motor or Dex3 state reports an error")
        settled, body_speed, hand_speed = self._entry_velocity_status(state["dq_43"])
        if require_settled and not settled:
            raise InterlockError(
                "commanded joints are not settled: "
                f"body={body_speed:.6f} rad/s, hand={hand_speed:.6f} rad/s"
            )
        return state

    @staticmethod
    def _timing_gap(exc: BaseException) -> bool:
        text = str(exc)
        return "stale" in text or "state skew" in text

    def _live_guard_with_one_timing_retry(
        self, *, expected_remote_sequence: int | None = None
    ) -> dict:
        try:
            return self._live_guard(expected_remote_sequence=expected_remote_sequence)
        except InterlockError as exc:
            if not self._timing_gap(exc):
                raise
            # Do not advance the target on a timing gap. One 40 Hz tick remains
            # inside the independent 75 ms command watchdog; a second failure
            # propagates and triggers the normal release path.
            time.sleep(TIMING_RETRY_DELAY_S)
            return self._live_guard(expected_remote_sequence=expected_remote_sequence)

    def _wait_fsm(self, target: int, allowed_sources: set[int]) -> tuple[int, int]:
        deadline = time.monotonic() + FSM_TRANSITION_TIMEOUT_S
        while time.monotonic() < deadline:
            self._live_guard()
            fsm_id, fsm_mode = self.io.read_fsm()
            if fsm_id == target:
                return fsm_id, fsm_mode
            if fsm_id not in allowed_sources:
                raise InterlockError(
                    f"FSM departed expected source while targeting {target}: {fsm_id}"
                )
            time.sleep(FSM_TRANSITION_POLL_S)
        raise InterlockError(f"FSM {target} was not confirmed before timeout")

    def _wait_settled_state(self, timeout_s: float = ENTRY_SETTLE_TIMEOUT_S) -> dict:
        good_since: float | None = None
        deadline = time.monotonic() + timeout_s
        while time.monotonic() < deadline:
            try:
                state = self._live_guard()
            except InterlockError as exc:
                if not any(
                    marker in str(exc)
                    for marker in ("stale", "waiting for valid", "state skew")
                ):
                    raise
                good_since = None
                time.sleep(TICK_S)
                continue
            settled, _, _ = self._entry_velocity_status(state["dq_43"])
            now = time.monotonic()
            if settled:
                good_since = now if good_since is None else good_since
                if now - good_since >= ENTRY_SETTLE_S:
                    return state
            else:
                good_since = None
            time.sleep(TICK_S)
        raise InterlockError("commanded joints did not reach a settled dwell")

    def _publish_until_settled(
        self,
        owner: str,
        step: int,
        target: list[float],
        *,
        expected_remote_sequence: int,
        timeout_s: float = ENTRY_SETTLE_TIMEOUT_S,
    ) -> tuple[int, dict]:
        """Keep the watchdog fed while establishing a measured-pose baseline."""
        good_since: float | None = None
        deadline = time.monotonic() + timeout_s
        while time.monotonic() < deadline:
            live = self._live_guard_with_one_timing_retry(
                expected_remote_sequence=expected_remote_sequence
            )
            settled, _, _ = self._entry_velocity_status(live["dq_43"])
            now = time.monotonic()
            if settled:
                good_since = now if good_since is None else good_since
            else:
                good_since = None
            step = self._publish(owner, step, target, 1.0)
            if good_since is not None and now - good_since >= ENTRY_SETTLE_S:
                return step, live
        raise InterlockError("commanded joints did not reach an armed settled dwell")

    def enter_running_fsm(self) -> dict:
        if not self.motion_enabled:
            raise InterlockError("physical motion is disabled")
        if self.fsm_bridge_url:
            self._live_guard()
            before = self._rpc("read-fsm")
            if before["fsm_id"] == RUNNING_FSM:
                if before["fsm_mode"] not in READY_FSM_MODES:
                    raise InterlockError(
                        f"required FSM mode is not ready: {before['fsm_mode']}"
                    )
                self._rpc("stop-move")
                self._wait_settled_state()
                return {
                    "before": dict(before),
                    "after": dict(before),
                    "transitions": [],
                    "preserved_operator_selected_fsm": True,
                }
            if RUNNING_FSM != 801:
                raise InterlockError(
                    f"required execution FSM {RUNNING_FSM} is not selected; "
                    f"current={before['fsm_id']}"
                )
            if before["fsm_id"] != 500:
                raise InterlockError(
                    f"FSM entry requires source 500 or ready 801; current={before['fsm_id']}"
                )
            value = self._bridge_post(
                "/api/arm-running",
                "wendy.g1.fsm-arm-running-request.v1",
                f"arm-{time.time_ns()}",
            )
            self._wait_settled_state()
            return value
        before_state = self._live_guard()
        remote_sequence = int(before_state["remote"]["button_sequence"])
        before_id, before_mode = self.io.read_fsm()
        if before_id == RUNNING_FSM:
            if before_mode not in READY_FSM_MODES:
                raise InterlockError(f"RUNNING FSM mode is not ready: {before_mode}")
            self.io.stop_move()
            self._wait_settled_state()
            return {
                "before": {"fsm_id": before_id, "fsm_mode": before_mode},
                "after": {"fsm_id": before_id, "fsm_mode": before_mode},
                "transitions": [],
            }
        if RUNNING_FSM != 801:
            raise InterlockError(
                f"required execution FSM {RUNNING_FSM} is not selected; current={before_id}"
            )
        if before_id != 500:
            raise InterlockError(f"FSM entry requires source 500 or ready 801; current={before_id}")
        self.io.stop_move()
        self._wait_settled_state()
        ai_mode = self.io.select_ai_mode()
        transitions: list[dict] = []
        current = before_id
        for target, allowed in (
            (DAMP_FSM, {500}),
            (LOCK_STAND_FSM, {DAMP_FSM}),
        ):
            self._live_guard(expected_remote_sequence=remote_sequence)
            self.io.set_fsm_id(target)
            current, mode = self._wait_fsm(target, allowed)
            transitions.append({"fsm_id": current, "fsm_mode": mode})
        settle_deadline = time.monotonic() + LOCK_STAND_SETTLE_S
        while time.monotonic() < settle_deadline:
            self._live_guard(expected_remote_sequence=remote_sequence)
            time.sleep(TICK_S)
        self.io.set_fsm_id(RUNNING_FSM)
        try:
            current, mode = self._wait_fsm(RUNNING_FSM, {LOCK_STAND_FSM})
        except InterlockError:
            current, _ = self.io.read_fsm()
            if current != LOCK_STAND_FSM:
                raise
            self.io.stop_move()
            self.io.select_ai_mode()
            time.sleep(LOCK_STAND_SETTLE_S)
            self.io.set_fsm_id(RUNNING_FSM)
            current, mode = self._wait_fsm(RUNNING_FSM, {LOCK_STAND_FSM})
        transitions.append({"fsm_id": current, "fsm_mode": mode})
        if current != RUNNING_FSM or mode not in READY_FSM_MODES:
            raise InterlockError(f"RUNNING FSM readback rejected: {current}/{mode}")
        self._live_guard(expected_remote_sequence=remote_sequence)
        return {
            "before": {"fsm_id": before_id, "fsm_mode": before_mode},
            "after": {"fsm_id": current, "fsm_mode": mode},
            "transitions": transitions,
            "ai_mode": ai_mode,
        }

    @staticmethod
    def _minimum_jerk(u: float) -> float:
        return (10.0 * u**3) - (15.0 * u**4) + (6.0 * u**5)

    @staticmethod
    def _trim_actuation_target(
        desired: np.ndarray,
        actuation: np.ndarray,
        measured: np.ndarray,
        indices: np.ndarray,
    ) -> np.ndarray:
        """Bounded static-bias trim; desired remains the qualification target."""
        updated = actuation.copy()
        residual = desired[indices] - measured[indices]
        offsets = actuation[indices] - desired[indices]
        correction = np.where(
            np.abs(residual) > ENTRY_STATIC_COMPENSATION_DEADBAND_RAD,
            np.clip(
                residual,
                -ENTRY_STATIC_COMPENSATION_STEP_RAD,
                ENTRY_STATIC_COMPENSATION_STEP_RAD,
            ),
            0.0,
        )
        offsets = np.clip(
            offsets + correction,
            -ENTRY_STATIC_COMPENSATION_LIMIT_RAD,
            ENTRY_STATIC_COMPENSATION_LIMIT_RAD,
        )
        for offset, index in zip(offsets, indices, strict=True):
            updated[index] = np.clip(
                desired[index] + offset,
                COMMAND_LOWER_BY_INDEX[int(index)],
                COMMAND_UPPER_BY_INDEX[int(index)],
            )
        return updated

    def run_reference_entry(self, command_id: str) -> dict:
        if not self.motion_enabled:
            raise InterlockError("physical motion is disabled")
        if command_id in self.command_results:
            return {**self.command_results[command_id], "duplicate_request": True}
        if not self.command_lock.acquire(blocking=False):
            raise InterlockError("another physical command is active")
        owner = f"u2525-reference-entry-{command_id}"
        step = 0
        armed = False
        started_ns = time.time_ns()
        maximum_tracking_error = 0.0
        try:
            fsm = self.enter_running_fsm()
            before = self._wait_settled_state()
            remote_sequence = int(before["remote"]["button_sequence"])
            start = np.asarray(before["q_43"], dtype=float)
            goal = start.copy()
            indices = np.asarray(COMMAND_POLICY_INDICES, dtype=int)
            goal[indices] = self.reference_frame0[indices]
            maximum_delta = float(np.max(np.abs(goal[indices] - start[indices])))
            duration_s = min(
                ENTRY_MAXIMUM_DURATION_S,
                max(
                    ENTRY_MINIMUM_DURATION_S,
                    1.875 * maximum_delta / ENTRY_MAXIMUM_VELOCITY_RAD_S,
                ),
            )
            sample_count = int(np.ceil(duration_s / TICK_S))
            previous_target = start.copy()
            self.io.arm(owner, self.command_profile())
            armed = True
            for index in range(1, ENTRY_OWNERSHIP_RAMP_STEPS + 1):
                step = self._publish(
                    owner,
                    step,
                    start.tolist(),
                    index / ENTRY_OWNERSHIP_RAMP_STEPS,
                )
            for sample in range(1, sample_count + 1):
                u = sample / sample_count
                blend = self._minimum_jerk(u)
                target = start.copy()
                target[indices] = start[indices] + blend * (goal[indices] - start[indices])
                maximum_step = float(np.max(np.abs(target[indices] - previous_target[indices])))
                if maximum_step > ENTRY_MAXIMUM_STEP_RAD:
                    raise InterlockError(
                        f"entry planner exceeded maximum step: {maximum_step:.6f} rad"
                    )
                live = self._live_guard_with_one_timing_retry(
                    expected_remote_sequence=remote_sequence
                )
                tracking_errors = np.abs(
                    self._controlled(live["q_43"]) - previous_target[indices]
                )
                tracking_slot = int(np.argmax(tracking_errors))
                tracking = float(tracking_errors[tracking_slot])
                maximum_tracking_error = max(maximum_tracking_error, tracking)
                if tracking > ENTRY_TRACKING_ABORT_RAD:
                    tracking_joint = int(COMMAND_POLICY_INDICES[tracking_slot])
                    raise InterlockError(
                        "entry tracking error exceeded limit: "
                        f"joint={tracking_joint}, error={tracking:.6f} rad, "
                        f"limit={ENTRY_TRACKING_ABORT_RAD:.6f} rad"
                    )
                step = self._publish(owner, step, target.tolist(), 1.0)
                previous_target = target
            good_since: float | None = None
            final_deadline = time.monotonic() + ENTRY_SETTLE_TIMEOUT_S
            after = None
            final_errors = np.full(len(indices), np.inf, dtype=float)
            final_speed = float("inf")
            actuation_goal = goal.copy()
            while time.monotonic() < final_deadline:
                live = self._live_guard_with_one_timing_retry(
                    expected_remote_sequence=remote_sequence
                )
                final_errors = np.abs(self._controlled(live["q_43"]) - goal[indices])
                error = float(np.max(final_errors))
                settled, final_body_speed, final_hand_speed = self._entry_velocity_status(
                    live["dq_43"]
                )
                final_speed = max(final_body_speed, final_hand_speed)
                maximum_tracking_error = max(maximum_tracking_error, error)
                now = time.monotonic()
                if error <= ENTRY_POSITION_LIMIT_RAD and settled:
                    good_since = now if good_since is None else good_since
                    if now - good_since >= ENTRY_SETTLE_S:
                        after = live
                        break
                else:
                    good_since = None
                actuation_goal = self._trim_actuation_target(
                    goal,
                    actuation_goal,
                    np.asarray(live["q_43"], dtype=float),
                    indices,
                )
                step = self._publish(owner, step, actuation_goal.tolist(), 1.0)
            if after is None:
                worst = sorted(
                    zip(indices.tolist(), final_errors.tolist(), strict=True),
                    key=lambda item: item[1],
                    reverse=True,
                )[:5]
                detail = ",".join(f"{index}:{error:.6f}" for index, error in worst)
                raise InterlockError(
                    "reference frame 0 did not attain a settled dwell: "
                    f"maximum_position_error={float(np.max(final_errors)):.6f} rad, "
                    f"maximum_speed={final_speed:.6f} rad/s, worst={detail}"
                )
            settled_hold_error = float(
                np.max(np.abs(self._controlled(after["q_43"]) - goal[indices]))
            )
            for index in range(RAMP_STEPS - 1, -1, -1):
                step = self._publish(
                    owner,
                    step,
                    actuation_goal.tolist(),
                    index / RAMP_STEPS,
                )
            self.io.disarm("requested_stop")
            armed = False
            self._rpc("stop-move")
            fsm_after = self._rpc("read-fsm")
            after_id, after_mode = fsm_after["fsm_id"], fsm_after["fsm_mode"]
            if after_id != RUNNING_FSM or after_mode not in READY_FSM_MODES:
                raise InterlockError(f"FSM changed during reference entry: {after_id}/{after_mode}")
            final = self.io.snapshot()
            final_error = float(
                np.max(np.abs(self._controlled(final["q_43"]) - goal[indices]))
            )
            result = {
                "schema": "wendy.g1.reference-residual-entry-result.v1",
                "accepted": True,
                "qualified": final_error <= ENTRY_POSITION_LIMIT_RAD,
                "command_id": command_id,
                "started_at_unix_ns": started_ns,
                "finished_at_unix_ns": time.time_ns(),
                "fsm": fsm,
                "maximum_start_delta_rad": maximum_delta,
                "planned_duration_s": duration_s,
                "trajectory_samples": sample_count,
                "maximum_tracking_error_rad": maximum_tracking_error,
                "settled_hold_error_rad": settled_hold_error,
                "entry_attained_while_owned": settled_hold_error <= ENTRY_POSITION_LIMIT_RAD,
                "maximum_static_compensation_rad": float(
                    np.max(np.abs(actuation_goal[indices] - goal[indices]))
                ),
                "final_error_after_release_rad": final_error,
                "io": self.io.status(),
                "meaning": "harnessed reference-frame0 entry observed after release; not policy execution",
            }
            self.command_results[command_id] = result
            return result
        except Exception:
            if armed:
                try:
                    self.io.disarm("reference_entry_fault")
                except Exception:
                    pass
            try:
                self._rpc("stop-move")
            except Exception:
                pass
            raise
        finally:
            self.command_lock.release()

    def _publish(self, owner: str, step: int, target: list[float], weight: float) -> int:
        try:
            self.io.publish_policy_target(
                owner,
                step=step,
                target_q_43=target,
                arm_sdk_weight=weight,
            )
        except InterlockError as exc:
            if not self._timing_gap(exc):
                raise
            time.sleep(TIMING_RETRY_DELAY_S)
            self.io.publish_policy_target(
                owner,
                step=step,
                target_q_43=target,
                arm_sdk_weight=weight,
            )
        time.sleep(TICK_S)
        return step + 1

    def run_waist_canary(self, command_id: str) -> dict:
        if not self.motion_enabled:
            raise InterlockError("physical motion is disabled")
        if command_id in self.command_results:
            return {**self.command_results[command_id], "duplicate_request": True}
        if not self.command_lock.acquire(blocking=False):
            raise InterlockError("another physical command is active")
        owner = f"u2525-waist-canary-{command_id}"
        step = 0
        armed = False
        events: list[dict] = []
        started_ns = time.time_ns()
        try:
            fsm_before = self._rpc("read-fsm")
            if (
                fsm_before.get("code") != 0
                or fsm_before.get("fsm_id") != RUNNING_FSM
                or fsm_before.get("fsm_mode") not in READY_FSM_MODES
            ):
                raise InterlockError(f"FSM preflight rejected: {fsm_before}")
            stopped = self._rpc("stop-move")
            if stopped.get("code") != 0:
                raise InterlockError(f"StopMove rejected: {stopped}")
            events.append({"event": "stop_move_acknowledged", "at_unix_ns": time.time_ns()})
            before = self._wait_settled_state()
            remote_sequence = int(before["remote"]["button_sequence"])
            start_target = list(before["q_43"])
            arm = self.io.arm(owner, self.command_profile())
            armed = True
            events.append({"event": "publishers_armed", "at_unix_ns": time.time_ns(), "receipt": arm})
            for index in range(1, ENTRY_OWNERSHIP_RAMP_STEPS + 1):
                step = self._publish(
                    owner,
                    step,
                    start_target,
                    index / ENTRY_OWNERSHIP_RAMP_STEPS,
                )
            step, held = self._publish_until_settled(
                owner,
                step,
                start_target,
                expected_remote_sequence=remote_sequence,
            )
            events.append({"event": "measured_pose_hold_engaged", "at_unix_ns": time.time_ns()})

            direction = 1.0 if start_target[12] <= 2.618 - CANARY_DELTA_RAD else -1.0
            canary_target = list(start_target)
            canary_target[12] += direction * CANARY_DELTA_RAD
            held_q = np.asarray(held["q_43"], dtype=float)
            during = held
            best_directional_delta = 0.0
            maximum_other_commanded_delta = 0.0
            upper_all = np.asarray((*range(12, 22), *range(29, 36)))
            upper_other = np.asarray(
                [index for index in upper_all if index != 12]
            )
            commanded_other = np.asarray(
                [index for index in COMMAND_POLICY_INDICES if index != 12]
            )
            maximum_other_upper_body_delta = 0.0
            for _ in range(CANARY_HOLD_STEPS):
                step = self._publish(owner, step, canary_target, 1.0)
                sample = self._live_guard_with_one_timing_retry(
                    expected_remote_sequence=remote_sequence
                )
                sample_q = np.asarray(sample["q_43"], dtype=float)
                directional_delta = float((sample_q[12] - held_q[12]) * direction)
                if directional_delta > best_directional_delta:
                    best_directional_delta = directional_delta
                    during = sample
                maximum_other_upper_body_delta = max(
                    maximum_other_upper_body_delta,
                    float(np.max(np.abs(sample_q[upper_other] - held_q[upper_other]))),
                )
                maximum_other_commanded_delta = max(
                    maximum_other_commanded_delta,
                    float(
                        np.max(
                            np.abs(sample_q[commanded_other] - held_q[commanded_other])
                        )
                    ),
                )
            for _ in range(CANARY_HOLD_STEPS):
                step = self._publish(owner, step, start_target, 1.0)
            step, returned = self._publish_until_settled(
                owner,
                step,
                start_target,
                expected_remote_sequence=remote_sequence,
            )
            events.append({"event": "waist_canary_returned", "at_unix_ns": time.time_ns()})
            for index in range(RAMP_STEPS - 1, -1, -1):
                step = self._publish(owner, step, start_target, index / RAMP_STEPS)
            self.io.disarm("requested_stop")
            armed = False
            stop_after = self._rpc("stop-move")
            if stop_after.get("code") != 0:
                raise InterlockError(f"final StopMove rejected: {stop_after}")
            fsm_after = self._rpc("read-fsm")

            during_q = np.asarray(during["q_43"], dtype=float)
            returned_q = np.asarray(returned["q_43"], dtype=float)
            waist_delta = float(during_q[12] - held_q[12])
            upper_return_error = float(
                np.max(np.abs(returned_q[upper_all] - held_q[upper_all]))
            )
            return_error = float(
                np.max(
                    np.abs(
                        returned_q[np.asarray(COMMAND_POLICY_INDICES)]
                        - held_q[np.asarray(COMMAND_POLICY_INDICES)]
                    )
                )
            )
            qualified = bool(
                waist_delta * direction >= 0.001
                and maximum_other_upper_body_delta <= 0.003
                and upper_return_error <= 0.003
                and fsm_after.get("code") == 0
                and fsm_after.get("fsm_id") == RUNNING_FSM
                and fsm_after.get("fsm_mode") in READY_FSM_MODES
            )
            result = {
                "schema": "wendy.g1.reference-residual-waist-canary.v1",
                "accepted": True,
                "qualified": qualified,
                "command_id": command_id,
                "started_at_unix_ns": started_ns,
                "finished_at_unix_ns": time.time_ns(),
                "requested_delta_rad": direction * CANARY_DELTA_RAD,
                "observed_waist_delta_rad": waist_delta,
                "maximum_other_upper_body_delta_rad": maximum_other_upper_body_delta,
                "maximum_other_commanded_joint_delta_rad": maximum_other_commanded_delta,
                "maximum_upper_body_return_error_rad": upper_return_error,
                "maximum_return_error_rad": return_error,
                "fsm_before": fsm_before,
                "fsm_after": fsm_after,
                "events": events,
                "io": self.io.status(),
                "meaning": "bounded physical command-path test; not policy qualification",
            }
            self.command_results[command_id] = result
            return result
        except Exception:
            if armed:
                try:
                    self.io.disarm("canary_fault")
                except Exception:
                    pass
            try:
                self._rpc("stop-move")
            except Exception:
                pass
            raise
        finally:
            self.command_lock.release()

    def state(self) -> dict:
        try:
            live = self.io.snapshot()
            q = np.asarray(live["q_43"], dtype=float)
            dq = np.asarray(live["dq_43"], dtype=float)
            indices = np.asarray(COMMAND_POLICY_INDICES, dtype=int)
            errors = np.abs(q[indices] - self.reference_frame0[indices])
            speeds = np.abs(dq[indices])
            velocity_settled, body_speed, hand_speed = self._entry_velocity_status(dq)
            alignment = {
                "current_state_aligned": bool(
                    errors.max() <= ENTRY_POSITION_LIMIT_RAD
                    and velocity_settled
                ),
                "within_entry_limits": bool(errors.max() <= ENTRY_POSITION_LIMIT_RAD),
                "maximum_position_error_rad": float(errors.max()),
                "maximum_speed_rad_s": float(speeds.max()),
                "position_limit_rad": ENTRY_POSITION_LIMIT_RAD,
                "speed_limit_rad_s": ENTRY_SPEED_LIMIT_RAD_S,
                "maximum_body_speed_rad_s": body_speed,
                "body_speed_limit_rad_s": ENTRY_SPEED_LIMIT_RAD_S,
                "maximum_hand_speed_rad_s": hand_speed,
                "hand_speed_limit_rad_s": ENTRY_HAND_SPEED_LIMIT_RAD_S,
                "per_joint_error_rad": {
                    str(int(index)): float(error)
                    for index, error in zip(indices, errors, strict=True)
                },
                "detail": "live commanded-joint state compared with exact reference target frame 0",
            }
            with self.lock:
                self.last_error = None
            policy_status = (
                self.policy_runner.status() if self.policy_runner is not None else None
            )
            active_policy = (policy_status or {}).get("active_policy") or {}
            return {
                "schema": "wendy.g1.reference-residual-physical-probe.v1",
                "healthy": True,
                "motion_capability": self.motion_enabled,
                "physical_commands_sent": self.io.status()["dds_writes_accepted"],
                "integrated_policy_loaded": self.policy_runner is not None,
                "policy_runtime": policy_status,
                "checkpoint_sha256": active_policy.get("checkpoint_sha256", CHECKPOINT_SHA256),
                "reference_contract_sha256": active_policy.get(
                    "reference_contract_sha256", REFERENCE_SHA256
                ),
                "hand_mapping_id": HAND_MAPPING_ID,
                "command_watchdog_s": self.io.config.watchdog_timeout_s,
                "maximum_state_skew_s": self.io.config.maximum_state_skew_s,
                "maximum_policy_tracking_error_rad": self.maximum_policy_tracking_error_rad,
                "entry_tracking_abort_rad": ENTRY_TRACKING_ABORT_RAD,
                "started_at_unix_ns": self.started_at_unix_ns,
                "observed_at_unix_ns": time.time_ns(),
                "live": {
                    **live,
                    "q_43": list(live["q_43"]),
                    "dq_43": list(live["dq_43"]),
                    "body_q_29": list(live["body_q_29"]),
                    "right_hand_wire_q_7": list(live["right_hand_wire_q_7"]),
                    "body_errors": list(live["body_errors"]),
                    "left_hand_errors": list(live["left_hand_errors"]),
                    "right_hand_errors": list(live["right_hand_errors"]),
                    "left_hand_system_errors": list(live["left_hand_system_errors"]),
                    "right_hand_system_errors": list(live["right_hand_system_errors"]),
                },
                "alignment": alignment,
                "io": self.io.status(),
                "last_error": None,
            }
        except Exception as exc:
            error = f"{type(exc).__name__}: {exc}"
            with self.lock:
                self.last_error = error
            policy_status = (
                self.policy_runner.status() if self.policy_runner is not None else None
            )
            active_policy = (policy_status or {}).get("active_policy") or {}
            return {
                "schema": "wendy.g1.reference-residual-physical-probe.v1",
                "healthy": False,
                "motion_capability": self.motion_enabled,
                "physical_commands_sent": self.io.status()["dds_writes_accepted"],
                "integrated_policy_loaded": self.policy_runner is not None,
                "policy_runtime": policy_status,
                "checkpoint_sha256": active_policy.get("checkpoint_sha256", CHECKPOINT_SHA256),
                "reference_contract_sha256": active_policy.get(
                    "reference_contract_sha256", REFERENCE_SHA256
                ),
                "maximum_policy_tracking_error_rad": self.maximum_policy_tracking_error_rad,
                "entry_tracking_abort_rad": ENTRY_TRACKING_ABORT_RAD,
                "observed_at_unix_ns": time.time_ns(),
                "io": self.io.status(),
                "last_error": error,
            }

    def close(self) -> None:
        self.io.close()


def make_server(runtime: PhysicalProbeRuntime, host: str = "0.0.0.0", port: int = PORT):
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_args):
            return

        def do_GET(self):
            parsed = urlsplit(self.path)
            path = parsed.path
            if path == "/":
                return self.reply_html(200, CONTROL_UI_HTML)
            if path == "/api/reference-residual/preflight":
                try:
                    return self.reply(200, runtime.preflight_integrated_policy())
                except (ValueError, InterlockError) as exc:
                    return self.reply(
                        409,
                        {"accepted": False, "error": f"{type(exc).__name__}: {exc}"},
                    )
                except Exception as exc:
                    return self.reply(
                        503,
                        {"accepted": False, "error": f"{type(exc).__name__}: {exc}"},
                    )
            if path == "/api/reference-residual/can-alignment-runs":
                return self.reply(200, runtime.can_alignment_runs())
            if path == "/api/reference-residual/can-alignment":
                try:
                    args = parse_qs(parsed.query, strict_parsing=True)
                    if set(args) != {"command_id"} or len(args["command_id"]) != 1:
                        raise ValueError("command_id is required exactly once")
                    command_id = args["command_id"][0]
                    return self.reply_jsonl(
                        200,
                        runtime.can_alignment_trace(command_id),
                        command_id,
                    )
                except FileNotFoundError as exc:
                    return self.reply(404, {"error": str(exc)})
                except ValueError as exc:
                    return self.reply(400, {"error": str(exc)})
            if path not in ("/state", "/health"):
                return self.reply(404, {"error": "not found"})
            value = runtime.state()
            self.reply(200 if value["healthy"] else 503, value)

        def do_POST(self):
            path = self.path.split("?", 1)[0]
            if path not in (
                "/api/reference-residual/waist-canary",
                "/api/reference-residual/prepare-entry",
                "/api/reference-residual/run-policy",
                "/api/reference-residual/retreat",
                "/api/reference-residual/stop",
            ):
                return self.reply(404, {"error": "not found"})
            try:
                length = int(self.headers.get("Content-Length", "0"))
                if length <= 0 or length > 32768:
                    raise ValueError("invalid request length")
                body = json.loads(self.rfile.read(length))
                is_policy = path.endswith("run-policy")
                is_retreat = path.endswith("retreat")
                is_stop = path.endswith("stop")
                expected_fields = {"schema", "command_id", "operator_confirmed"}
                if is_policy:
                    expected_fields.add("maximum_policy_steps")
                allowed_fields = (
                    expected_fields | {"expected_checkpoint_sha256", "execution_hz"}
                    if is_policy
                    else expected_fields
                )
                if not expected_fields.issubset(body) or not set(body).issubset(allowed_fields):
                    raise ValueError("unexpected request fields")
                expected_schema = (
                    "wendy.g1.reference-residual-waist-canary-request.v1"
                    if path.endswith("waist-canary")
                    else (
                        "wendy.g1.reference-residual-physical-run-request.v1"
                        if is_policy
                        else (
                            "wendy.g1.reference-residual-safe-retreat-request.v1"
                            if is_retreat
                            else (
                                "wendy.g1.reference-residual-physical-stop-request.v1"
                                if is_stop
                                else "wendy.g1.reference-residual-entry-request.v1"
                            )
                        )
                    )
                )
                if body["schema"] != expected_schema:
                    raise ValueError("unexpected request schema")
                if body["operator_confirmed"] is not True:
                    raise ValueError("operator confirmation required")
                command_id = str(body["command_id"])
                if COMMAND_ID_PATTERN.fullmatch(command_id) is None:
                    raise ValueError("invalid command id")
                if is_stop:
                    value = runtime.stop_integrated_policy(command_id)
                elif is_retreat:
                    value = runtime.retreat_integrated_policy(command_id)
                elif is_policy:
                    maximum_policy_steps = body["maximum_policy_steps"]
                    if isinstance(maximum_policy_steps, bool) or not isinstance(
                        maximum_policy_steps, int
                    ):
                        raise ValueError("maximum_policy_steps must be an integer")
                    expected_checkpoint = body.get("expected_checkpoint_sha256")
                    if expected_checkpoint is not None and (
                        not isinstance(expected_checkpoint, str)
                        or len(expected_checkpoint) != 64
                        or any(character not in "0123456789abcdef" for character in expected_checkpoint)
                    ):
                        raise ValueError("expected_checkpoint_sha256 must be lowercase SHA-256")
                    execution_hz = body.get("execution_hz")
                    if (
                        isinstance(execution_hz, bool)
                        or execution_hz not in {None, 15, 25, 30, 40}
                    ):
                        raise ValueError("execution_hz must be 15, 25, 30, or 40")
                    value = runtime.run_integrated_policy(
                        command_id,
                        maximum_policy_steps=maximum_policy_steps,
                        expected_checkpoint_sha256=expected_checkpoint,
                        execution_hz=execution_hz,
                    )
                else:
                    value = (
                        runtime.run_waist_canary(command_id)
                        if path.endswith("waist-canary")
                        else runtime.run_reference_entry(command_id)
                    )
                self.reply(200, value)
            except (ValueError, InterlockError) as exc:
                self.reply(409, {"accepted": False, "error": f"{type(exc).__name__}: {exc}"})
            except Exception as exc:
                self.reply(500, {"accepted": False, "error": f"{type(exc).__name__}: {exc}"})

        def reply(self, status: int, value: dict):
            body = json.dumps(value, allow_nan=False, separators=(",", ":")).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def reply_html(self, status: int, value: str):
            body = value.encode()
            self.send_response(status)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(body)

        def reply_jsonl(self, status: int, value: str, command_id: str):
            body = value.encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/x-ndjson")
            self.send_header(
                "Content-Disposition",
                f'attachment; filename="{command_id}.can-alignment.jsonl"',
            )
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(body)

    return ThreadingHTTPServer((host, port), Handler)


def main() -> None:
    runtime = PhysicalProbeRuntime()
    server = make_server(runtime)
    try:
        server.serve_forever()
    finally:
        server.server_close()
        runtime.close()


if __name__ == "__main__":
    main()
