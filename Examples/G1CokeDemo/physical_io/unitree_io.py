"""Guarded Unitree I/O for the exact update-2525 policy contract.

This module is deliberately separate from policy inference.  It may subscribe
while motion is disabled, but it does not construct either command publisher
until :meth:`ExactPolicyUnitreeIO.arm` passes its explicit interlocks.

The frozen model orders its 43 joints as body[0:22], left Dex3 channel order,
body[22:29], and right Dex3 channel order.  The u2525 export deliberately binds
both policy hand slices directly to DDS motor channels 0..6.  The mapping
identity is an arming prerequisite so a future semantic remap fails closed
instead of silently crossing two fingers.
"""

from __future__ import annotations

import copy
import concurrent.futures
import fcntl
import json
import math
import os
import struct
import threading
import time
from dataclasses import dataclass, replace
from pathlib import Path
from typing import Any, Callable, Protocol


LOW_STATE_TOPIC = "rt/lowstate"
LEFT_HAND_STATE_TOPIC = "rt/dex3/left/state"
RIGHT_HAND_STATE_TOPIC = "rt/dex3/right/state"
ARM_COMMAND_TOPIC = "rt/arm_sdk"
RIGHT_HAND_COMMAND_TOPIC = "rt/dex3/right/cmd"

# Unitree's SDK adds config-level CycloneDDS tracing to its explicit-interface
# XML.  On the Jetson Ubuntu 24.04 runtime that tracing path aborts during
# Domain construction with glibc's buffer-overflow guard.  Keep the explicit
# interface binding, but omit optional tracing; this is also the shape used by
# the SDK's autodetect configuration.
EXPLICIT_INTERFACE_DDS_CONFIG = """<?xml version="1.0" encoding="UTF-8" ?>
<CycloneDDS>
  <Domain Id="any">
    <General>
      <Interfaces>
        <NetworkInterface name="$__IF_NAME__$" priority="default" multicast="default"/>
      </Interfaces>
    </General>
  </Domain>
</CycloneDDS>"""

HAND_MAPPING_ID = "u2525-direct-dex3-channel-order-v1"
POLICY_HAND_NAMES = (
    "thumb_0", "thumb_1", "thumb_2", "middle_0", "middle_1", "index_0", "index_1"
)
LEFT_WIRE_NAMES = POLICY_HAND_NAMES
RIGHT_WIRE_NAMES = POLICY_HAND_NAMES
RIGHT_POLICY_FROM_WIRE = tuple(RIGHT_WIRE_NAMES.index(name) for name in POLICY_HAND_NAMES)
RIGHT_WIRE_FROM_POLICY = tuple(POLICY_HAND_NAMES.index(name) for name in RIGHT_WIRE_NAMES)

COMMAND_TARGET_INDICES = (*range(12, 22), *range(29, 43))
COMMAND_TARGET_LOWER_RAD = (
    -2.618, -0.52, -0.52,
    -3.0892, -1.5882, -2.618, -1.0472, -1.97222, -1.61443, -1.61443,
    -3.0892, -2.2515, -2.618, -1.0472, -1.97222, -1.61443, -1.61443,
    -1.05, -1.05, -1.75, 0.0, 0.0, 0.0, 0.0,
)
COMMAND_TARGET_UPPER_RAD = (
    2.618, 0.52, 0.52,
    2.6704, 2.2515, 2.618, 2.0944, 1.97222, 1.61443, 1.61443,
    2.6704, 1.5882, 2.618, 2.0944, 1.97222, 1.61443, 1.61443,
    1.05, 0.742, 0.0, 1.57, 1.75, 1.57, 1.75,
)

GET_FSM_ID_API = 7001
GET_FSM_MODE_API = 7002


class InterlockError(RuntimeError):
    """A command was rejected before publication."""


def _finite_vector(values: Any, size: int, label: str) -> tuple[float, ...]:
    try:
        result = tuple(float(value) for value in values)
    except (TypeError, ValueError) as exc:
        raise InterlockError(f"{label} must contain {size} finite values") from exc
    if len(result) != size or not all(math.isfinite(value) for value in result):
        raise InterlockError(f"{label} must contain {size} finite values")
    return result


def policy_hand_from_wire(side: str, values: Any) -> tuple[float, ...]:
    wire = _finite_vector(values, 7, f"{side} Dex3 state")
    if side in {"left", "right"}:
        return wire
    raise InterlockError("Dex3 side must be left or right")


def right_wire_from_policy(values: Any) -> tuple[float, ...]:
    policy = _finite_vector(values, 7, "right Dex3 policy target")
    return tuple(policy[index] for index in RIGHT_WIRE_FROM_POLICY)


def reorder_physical_state(
    body: Any, left_hand_wire: Any, right_hand_wire: Any
) -> tuple[float, ...]:
    """Return the exact 43-slot order consumed by the frozen sensor contract."""
    body_values = _finite_vector(body, 29, "Unitree body state")
    left = policy_hand_from_wire("left", left_hand_wire)
    right = policy_hand_from_wire("right", right_hand_wire)
    return (*body_values[:22], *left, *body_values[22:29], *right)


@dataclass(frozen=True)
class IoConfig:
    interface: str
    motion_enabled: bool = False
    verified_hand_mapping_id: str = ""
    state_max_age_s: float = 0.050
    maximum_state_skew_s: float = 0.020
    watchdog_timeout_s: float = 0.075
    remote_neutral_threshold: float = 0.12
    allowed_mode_machine: tuple[int, ...] = ()
    ownership_path: Path = Path("/tmp/wendy-g1-reference-residual-u002525.lock")

    def validate(self) -> None:
        if not self.interface.strip():
            raise InterlockError("an explicit DDS interface is required")
        for name, value in (
            ("state_max_age_s", self.state_max_age_s),
            ("maximum_state_skew_s", self.maximum_state_skew_s),
            ("watchdog_timeout_s", self.watchdog_timeout_s),
            ("remote_neutral_threshold", self.remote_neutral_threshold),
        ):
            if not math.isfinite(value) or value <= 0:
                raise InterlockError(f"{name} must be positive and finite")


@dataclass(frozen=True)
class CommandProfile:
    """Robot-qualified controller values; no defaults are intentionally supplied."""

    profile_id: str
    profile_sha256: str
    hardware_qualified: bool
    upper_kp: tuple[float, ...]
    upper_kd: tuple[float, ...]
    hand_kp: tuple[float, ...]
    hand_kd: tuple[float, ...]
    arm_sdk_weight: float

    def validated(self) -> "CommandProfile":
        if not self.hardware_qualified or not self.profile_id or len(self.profile_sha256) != 64:
            raise InterlockError("an identified hardware-qualified command profile is required")
        _finite_vector(self.upper_kp, 17, "upper-body kp")
        _finite_vector(self.upper_kd, 17, "upper-body kd")
        _finite_vector(self.hand_kp, 7, "right-hand kp")
        _finite_vector(self.hand_kd, 7, "right-hand kd")
        if not 0.0 < self.arm_sdk_weight <= 1.0:
            raise InterlockError("arm_sdk weight must be in (0, 1]")
        if any(value < 0 for value in (*self.upper_kp, *self.upper_kd, *self.hand_kp, *self.hand_kd)):
            raise InterlockError("command gains cannot be negative")
        return self


class UnitreeBackend(Protocol):
    def subscribe(
        self,
        low_state: Callable[[Any], None],
        left_hand: Callable[[Any], None],
        right_hand: Callable[[Any], None],
    ) -> None: ...

    def enable_publishers(self) -> None: ...

    def publish_upper_body(
        self, q: tuple[float, ...], profile: CommandProfile
    ) -> None: ...

    def publish_right_hand(
        self, q_wire: tuple[float, ...], profile: CommandProfile
    ) -> None: ...

    def publish_policy_command(
        self,
        upper_q: tuple[float, ...],
        right_hand_q_wire: tuple[float, ...],
        profile: CommandProfile,
    ) -> None: ...

    def release(self) -> None: ...

    def read_fsm(self) -> tuple[int, int]: ...

    def stop_move(self) -> None: ...

    def select_ai_mode(self) -> dict[str, Any]: ...

    def set_fsm_id(self, fsm_id: int) -> None: ...

    def close(self) -> None: ...


class SdkUnitreeBackend:
    """Thin lazy-import wrapper around the same Unitree DDS path as the recorder."""

    def __init__(self, interface: str):
        self.interface = interface
        self._subscribers: list[Any] = []
        self._arm_publisher: Any = None
        self._hand_publisher: Any = None
        self._low_command: Any = None
        self._hand_command: Any = None
        self._low_command_factory: Any = None
        self._hand_command_factory: Any = None
        self._crc: Any = None
        self._loco_client: Any = None
        self._motion_switcher: Any = None
        self._rpc_lock = threading.RLock()
        # The two command topics have independent writers and message buffers.
        # A persistent pair of workers lets DDS publish them concurrently; the
        # weight-zero prime write warms these threads before the 40 Hz clock.
        self._command_executor = concurrent.futures.ThreadPoolExecutor(
            max_workers=2, thread_name_prefix="unitree-command"
        )
        self._types: dict[str, Any] = {}
        self._channel_initialized = False

    def _initialize(self) -> None:
        if self._channel_initialized:
            return
        try:
            from unitree_sdk2py.core import channel as unitree_channel
            from unitree_sdk2py.idl.default import (
                unitree_hg_msg_dds__HandCmd_, unitree_hg_msg_dds__LowCmd_
            )
            from unitree_sdk2py.idl.unitree_hg.msg.dds_ import (
                HandCmd_, HandState_, LowCmd_, LowState_
            )
            from unitree_sdk2py.utils.crc import CRC
        except ImportError as exc:
            raise InterlockError(f"Unitree SDK is unavailable: {exc}") from exc
        unitree_channel.ChannelConfigHasInterface = EXPLICIT_INTERFACE_DDS_CONFIG
        initialized = unitree_channel.ChannelFactoryInitialize(0, self.interface)
        if initialized is False:
            raise InterlockError(f"DDS initialization failed on {self.interface}")
        self._low_command_factory = unitree_hg_msg_dds__LowCmd_
        self._hand_command_factory = unitree_hg_msg_dds__HandCmd_
        self._types = {
            "low_cmd": LowCmd_, "low_state": LowState_,
            "hand_cmd": HandCmd_, "hand_state": HandState_,
        }
        self._crc = CRC()
        self._channel_initialized = True

    def subscribe(self, low_state, left_hand, right_hand) -> None:
        self._initialize()
        if self._subscribers:
            return
        from unitree_sdk2py.core.channel import ChannelSubscriber

        for topic, kind, callback in (
            (LOW_STATE_TOPIC, self._types["low_state"], low_state),
            (LEFT_HAND_STATE_TOPIC, self._types["hand_state"], left_hand),
            (RIGHT_HAND_STATE_TOPIC, self._types["hand_state"], right_hand),
        ):
            subscriber = ChannelSubscriber(topic, kind)
            subscriber.Init(callback, 10)
            self._subscribers.append(subscriber)

    def enable_publishers(self) -> None:
        self._initialize()
        if self._arm_publisher is not None:
            return
        from unitree_sdk2py.core.channel import ChannelPublisher

        self._low_command = self._low_command_factory()
        self._hand_command = self._hand_command_factory()
        self._arm_publisher = ChannelPublisher(ARM_COMMAND_TOPIC, self._types["low_cmd"])
        self._hand_publisher = ChannelPublisher(RIGHT_HAND_COMMAND_TOPIC, self._types["hand_cmd"])
        self._arm_publisher.Init()
        self._hand_publisher.Init()

    def publish_upper_body(self, q, profile) -> None:
        if self._arm_publisher is None or self._low_command is None:
            raise InterlockError("upper-body publisher is not armed")
        for offset, index in enumerate(range(12, 29)):
            motor = self._low_command.motor_cmd[index]
            motor.q = q[offset]
            motor.dq = 0.0
            motor.kp = profile.upper_kp[offset]
            motor.kd = profile.upper_kd[offset]
            motor.tau = 0.0
        self._low_command.motor_cmd[29].q = profile.arm_sdk_weight
        self._low_command.crc = self._crc.Crc(self._low_command)
        if self._arm_publisher.Write(self._low_command) is False:
            raise InterlockError(f"{ARM_COMMAND_TOPIC} rejected command")

    def publish_right_hand(self, q_wire, profile) -> None:
        if self._hand_publisher is None or self._hand_command is None:
            raise InterlockError("right-hand publisher is not armed")
        for index in range(7):
            motor = self._hand_command.motor_cmd[index]
            motor.mode = index | (1 << 4)
            motor.q = q_wire[index]
            motor.dq = 0.0
            motor.kp = profile.hand_kp[index]
            motor.kd = profile.hand_kd[index]
            motor.tau = 0.0
        if self._hand_publisher.Write(self._hand_command) is False:
            raise InterlockError(f"{RIGHT_HAND_COMMAND_TOPIC} rejected command")

    def publish_policy_command(self, upper_q, right_hand_q_wire, profile) -> None:
        upper = self._command_executor.submit(
            self.publish_upper_body, upper_q, profile
        )
        hand = self._command_executor.submit(
            self.publish_right_hand, right_hand_q_wire, profile
        )
        # Always join both independent writes before reporting an error.  This
        # preserves the lifecycle invariant that disarm/release cannot race a
        # command write that is still in flight.
        concurrent.futures.wait((upper, hand))
        upper.result()
        hand.result()

    def release(self) -> None:
        errors: list[str] = []
        if self._arm_publisher is not None and self._low_command is not None:
            self._low_command.motor_cmd[29].q = 0.0
            self._low_command.crc = self._crc.Crc(self._low_command)
            if self._arm_publisher.Write(self._low_command) is False:
                errors.append("upper-body release rejected")
        if self._hand_publisher is not None and self._hand_command is not None:
            for index in range(7):
                motor = self._hand_command.motor_cmd[index]
                motor.mode = index | (1 << 4) | (1 << 7)
                motor.q = motor.dq = motor.kp = motor.kd = motor.tau = 0.0
            if self._hand_publisher.Write(self._hand_command) is False:
                errors.append("right-hand release rejected")
        if errors:
            raise InterlockError("; ".join(errors))

    @staticmethod
    def _rpc_pair(result: Any) -> tuple[int, Any]:
        if not isinstance(result, (tuple, list)) or len(result) != 2:
            raise InterlockError(f"unexpected Unitree RPC result: {result!r}")
        return int(result[0]), result[1]

    def _motion_clients(self) -> tuple[Any, Any]:
        self._initialize()
        if self._loco_client is None or self._motion_switcher is None:
            from unitree_sdk2py.comm.motion_switcher.motion_switcher_client import (
                MotionSwitcherClient,
            )
            from unitree_sdk2py.g1.loco.g1_loco_client import LocoClient

            self._loco_client = LocoClient()
            self._motion_switcher = MotionSwitcherClient()
            for client in (self._loco_client, self._motion_switcher):
                client.SetTimeout(3.0)
                client.Init()
        return self._loco_client, self._motion_switcher

    def read_fsm(self) -> tuple[int, int]:
        with self._rpc_lock:
            loco, _ = self._motion_clients()
            id_code, id_data = self._rpc_pair(loco._Call(GET_FSM_ID_API, "{}"))
            if id_code != 0 or not id_data:
                raise InterlockError(f"FSM ID readback failed with code {id_code}")
            mode_code, mode_data = self._rpc_pair(loco._Call(GET_FSM_MODE_API, "{}"))
            if mode_code != 0 or not mode_data:
                raise InterlockError(f"FSM mode readback failed with code {mode_code}")
            return (
                int(json.loads(id_data)["data"]),
                int(json.loads(mode_data)["data"]),
            )

    def stop_move(self) -> None:
        with self._rpc_lock:
            loco, _ = self._motion_clients()
            result = loco.StopMove()
            code = 0 if result is None else int(result)
            if code != 0:
                raise InterlockError(f"StopMove rejected with code {code}")

    def select_ai_mode(self) -> dict[str, Any]:
        with self._rpc_lock:
            _, switcher = self._motion_clients()
            before_code, before = self._rpc_pair(switcher.CheckMode())
            select_code, _ = self._rpc_pair(switcher.SelectMode("ai"))
            if select_code != 0:
                raise InterlockError(f"AI motion mode rejected with code {select_code}")
            time.sleep(0.5)
            after_code, after = self._rpc_pair(switcher.CheckMode())
            if after_code != 0 or not isinstance(after, dict) or after.get("name") != "ai":
                raise InterlockError(
                    f"AI mode readback failed: code={after_code}, mode={after!r}"
                )
            return {"before_code": before_code, "before": before, "after": after}

    def set_fsm_id(self, fsm_id: int) -> None:
        with self._rpc_lock:
            loco, _ = self._motion_clients()
            result = loco.SetFsmId(int(fsm_id))
            code = 0 if result is None else int(result)
            if code != 0:
                raise InterlockError(f"FSM {fsm_id} request rejected with code {code}")

    def close(self) -> None:
        self._command_executor.shutdown(wait=True)
        for subscriber in self._subscribers:
            subscriber.Close()
        self._subscribers.clear()


_PROCESS_OWNERS_LOCK = threading.Lock()
_PROCESS_OWNERS: set[str] = set()


class ExactPolicyUnitreeIO:
    """Exact state reorder plus single-owner, watchdog-protected publication."""

    def __init__(
        self,
        config: IoConfig,
        *,
        backend: UnitreeBackend | None = None,
        monotonic: Callable[[], float] = time.monotonic,
    ):
        config.validate()
        self.config = config
        self.backend = backend or SdkUnitreeBackend(config.interface)
        self.monotonic = monotonic
        self._lock = threading.RLock()
        # Serialize arm/disarm lifecycle changes without holding the state lock.
        # DDS feedback callbacks must remain able to update `_state` while
        # command publishers are being initialized and freshness is rechecked.
        self._arm_transition_lock = threading.RLock()
        self._state: dict[str, dict[str, Any] | None] = {
            "body": None, "left": None, "right": None
        }
        self._started = False
        self._owner: str | None = None
        self._profile: CommandProfile | None = None
        self._ownership_fd: int | None = None
        self._last_step = -1
        self._last_command_at: float | None = None
        self._commands_sent = 0
        self._fault: str | None = None
        self._armed_remote_sequence: int | None = None
        self._remote_buttons = 0
        self._remote_sequence = 0
        self._watchdog_stop = threading.Event()
        self._watchdog_thread: threading.Thread | None = None

    def start(self) -> None:
        with self._lock:
            if self._started:
                return
            self.backend.subscribe(self._on_low_state, self._on_left_hand, self._on_right_hand)
            self._started = True

    def _decode_motors(self, message: Any, count: int, label: str) -> dict[str, Any]:
        now = self.monotonic()
        try:
            motors = list(message.motor_state)
            if len(motors) < count:
                raise ValueError("short motor sequence")
            motors = motors[:count]
            return {
                "q": _finite_vector((motor.q for motor in motors), count, f"{label} q"),
                "dq": _finite_vector((motor.dq for motor in motors), count, f"{label} dq"),
                "errors": tuple(int(motor.motorstate) for motor in motors),
                "received_at_monotonic": now,
                "received_at_unix_ns": time.time_ns(),
            }
        except (AttributeError, IndexError, TypeError, ValueError, InterlockError) as exc:
            raise InterlockError(f"invalid {label} state: {exc}") from exc

    def _on_low_state(self, message: Any) -> None:
        try:
            value = self._decode_motors(message, 29, "body")
            value["mode_machine"] = int(message.mode_machine)
            data = bytes(message.wireless_remote)
            if len(data) < 24:
                raise ValueError("short wireless remote payload")
            buttons = data[2] | (data[3] << 8)
            axes = tuple(struct.unpack_from("<f", data, offset)[0] for offset in (4, 8, 12, 20))
            if not all(math.isfinite(axis) for axis in axes):
                raise ValueError("nonfinite wireless remote axis")
            with self._lock:
                rising = buttons & ~self._remote_buttons
                if rising:
                    self._remote_sequence += 1
                self._remote_buttons = buttons
                remote_sequence = self._remote_sequence
            value["remote"] = {
                "buttons": buttons,
                "axes": axes,
                "neutral": buttons == 0 and all(
                    abs(axis) <= self.config.remote_neutral_threshold for axis in axes
                ),
                "button_sequence": remote_sequence,
            }
        except (AttributeError, TypeError, ValueError, struct.error, InterlockError):
            value = None
        with self._lock:
            self._state["body"] = value

    def _on_hand(self, side: str, message: Any) -> None:
        try:
            value = self._decode_motors(message, 7, f"{side} hand")
            value["system_errors"] = tuple(int(error) for error in message.error)
        except (AttributeError, TypeError, ValueError, InterlockError):
            value = None
        with self._lock:
            self._state[side] = value

    def _on_left_hand(self, message: Any) -> None:
        self._on_hand("left", message)

    def _on_right_hand(self, message: Any) -> None:
        self._on_hand("right", message)

    def snapshot(self) -> dict[str, Any]:
        if not self._started:
            self.start()
        with self._lock:
            if any(value is None for value in self._state.values()):
                raise InterlockError("waiting for valid body and both Dex3 states")
            body = copy.deepcopy(self._state["body"])
            left = copy.deepcopy(self._state["left"])
            right = copy.deepcopy(self._state["right"])
            # Sample time after copying the protected state.  A DDS callback can
            # otherwise install a newer sample between reading ``now`` and
            # acquiring this lock, producing a negative age and a false stale
            # interlock during the 40 Hz command loop.
            now = self.monotonic()
        assert body is not None and left is not None and right is not None
        times = tuple(part["received_at_monotonic"] for part in (body, left, right))
        ages = tuple(now - stamp for stamp in times)
        if any(age < 0 or age > self.config.state_max_age_s for age in ages):
            raise InterlockError(
                "body or Dex3 state is stale: "
                + ", ".join(
                    f"{label}={age * 1000.0:.1f}ms"
                    for label, age in zip(("body", "left", "right"), ages, strict=True)
                )
            )
        if max(times) - min(times) > self.config.maximum_state_skew_s:
            raise InterlockError("body/Dex3 state skew exceeds contract")
        q = reorder_physical_state(body["q"], left["q"], right["q"])
        dq = reorder_physical_state(body["dq"], left["dq"], right["dq"])
        return {
            "schema": "wendy.g1.reference-residual-unitree-state.v1",
            "source": "unitree_dds",
            "hand_mapping_id": HAND_MAPPING_ID,
            "q_43": q,
            "dq_43": dq,
            "body_q_29": tuple(body["q"]),
            "right_hand_wire_q_7": tuple(right["q"]),
            "received_at_monotonic": max(times),
            "received_at_unix_ns": max(
                int(part["received_at_unix_ns"]) for part in (body, left, right)
            ),
            # Conservative timestamp for a fused 43-joint packet.  The oldest
            # component, not the newest one, defines when all 43 values were
            # available to the policy observation.
            "sampled_at_unix_ns": min(
                int(part["received_at_unix_ns"]) for part in (body, left, right)
            ),
            "state_skew_unix_ns": max(
                int(part["received_at_unix_ns"]) for part in (body, left, right)
            ) - min(int(part["received_at_unix_ns"]) for part in (body, left, right)),
            "state_skew_s": max(times) - min(times),
            "component_age_s": {
                label: age
                for label, age in zip(("body", "left", "right"), ages, strict=True)
            },
            "mode_machine": body["mode_machine"],
            "remote": copy.deepcopy(body["remote"]),
            "body_errors": tuple(body["errors"]),
            "left_hand_errors": tuple(left["errors"]),
            "right_hand_errors": tuple(right["errors"]),
            "left_hand_system_errors": tuple(left["system_errors"]),
            "right_hand_system_errors": tuple(right["system_errors"]),
        }

    def _acquire_ownership(self, owner: str) -> None:
        key = str(self.config.ownership_path)
        with _PROCESS_OWNERS_LOCK:
            if key in _PROCESS_OWNERS:
                raise InterlockError("another in-process motor owner is active")
            _PROCESS_OWNERS.add(key)
        fd: int | None = None
        try:
            self.config.ownership_path.parent.mkdir(parents=True, exist_ok=True)
            fd = os.open(self.config.ownership_path, os.O_RDWR | os.O_CREAT, 0o600)
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
            os.ftruncate(fd, 0)
            os.write(fd, owner.encode("utf-8"))
        except BaseException:
            if fd is not None:
                os.close(fd)
            with _PROCESS_OWNERS_LOCK:
                _PROCESS_OWNERS.discard(key)
            raise InterlockError("another process owns the policy motor path")
        self._ownership_fd = fd

    def _release_ownership(self) -> None:
        fd, self._ownership_fd = self._ownership_fd, None
        if fd is not None:
            fcntl.flock(fd, fcntl.LOCK_UN)
            os.close(fd)
        with _PROCESS_OWNERS_LOCK:
            _PROCESS_OWNERS.discard(str(self.config.ownership_path))

    def arm(self, owner: str, profile: CommandProfile) -> dict[str, Any]:
        with self._arm_transition_lock:
            return self._arm_serialized(owner, profile)

    def _arm_serialized(
        self, owner: str, profile: CommandProfile
    ) -> dict[str, Any]:
        if not self.config.motion_enabled:
            raise InterlockError("physical motion is disabled")
        if self.config.verified_hand_mapping_id != HAND_MAPPING_ID:
            raise InterlockError("exact side-specific Dex3 mapping is not verified")
        if not owner.strip():
            raise InterlockError("a nonempty motor owner is required")
        profile.validated()
        state = self.snapshot()
        if not self.config.allowed_mode_machine:
            raise InterlockError("no allowed mode_machine values are configured")
        if state["mode_machine"] not in self.config.allowed_mode_machine:
            raise InterlockError(
                f"mode_machine {state['mode_machine']} is not explicitly allowed"
            )
        if state["remote"]["neutral"] is not True:
            raise InterlockError("wireless remote must be fresh and neutral")
        if any(
            any(int(value) != 0 for value in state[key])
            for key in (
                "body_errors", "left_hand_errors", "right_hand_errors",
                "left_hand_system_errors", "right_hand_system_errors",
            )
        ):
            raise InterlockError("motor or Dex3 state reports an error")
        with self._lock:
            if self._owner is not None:
                raise InterlockError("policy motor path is already armed")
            self._acquire_ownership(owner)
        try:
            self.backend.enable_publishers()
            deadline = self.monotonic() + 2.0
            refreshed = None
            while self.monotonic() < deadline:
                try:
                    refreshed = self.snapshot()
                    break
                except InterlockError as exc:
                    if "stale" not in str(exc) and "waiting for valid" not in str(exc):
                        raise
                    time.sleep(0.010)
            if refreshed is None:
                raise InterlockError(
                    "fresh feedback did not resume after publisher initialization"
                )
            if (
                refreshed["remote"]["neutral"] is not True
                or int(refreshed["remote"]["button_sequence"])
                != int(state["remote"]["button_sequence"])
            ):
                raise InterlockError(
                    "wireless remote changed during publisher initialization"
                )
            if refreshed["mode_machine"] not in self.config.allowed_mode_machine:
                raise InterlockError("mode_machine changed during publisher initialization")
            if any(
                any(int(value) != 0 for value in refreshed[key])
                for key in (
                    "body_errors", "left_hand_errors", "right_hand_errors",
                    "left_hand_system_errors", "right_hand_system_errors",
                )
            ):
                raise InterlockError(
                    "motor or Dex3 error appeared during publisher initialization"
                )
        except BaseException:
            with self._lock:
                self._release_ownership()
            raise
        with self._lock:
            self._owner = owner
            self._profile = profile
            self._last_step = -1
            self._last_command_at = self.monotonic()
            self._fault = None
            self._armed_remote_sequence = int(refreshed["remote"]["button_sequence"])
            self._watchdog_stop.clear()
            self._watchdog_thread = threading.Thread(
                target=self._watchdog, name="g1-policy-command-watchdog", daemon=True
            )
            self._watchdog_thread.start()
        return {
            "schema": "wendy.g1.reference-residual-arm-receipt.v1",
            "owner": owner,
            "profile_id": profile.profile_id,
            "profile_sha256": profile.profile_sha256,
            "hand_mapping_id": HAND_MAPPING_ID,
            "meaning": "publishers_created_no_motion_command_sent",
        }

    def _watchdog(self) -> None:
        interval = min(0.020, self.config.watchdog_timeout_s / 4)
        while not self._watchdog_stop.wait(interval):
            with self._lock:
                deadline = self._last_command_at
                armed = self._owner is not None
            if armed and deadline is not None and self.monotonic() - deadline > self.config.watchdog_timeout_s:
                self.disarm("command_watchdog_timeout")
                return

    def _validate_target(self, target_q_43: Any) -> tuple[float, ...]:
        target = _finite_vector(target_q_43, 43, "policy target_q_43")
        for index, low, high in zip(
            COMMAND_TARGET_INDICES,
            COMMAND_TARGET_LOWER_RAD,
            COMMAND_TARGET_UPPER_RAD,
            strict=True,
        ):
            if not low <= target[index] <= high:
                raise InterlockError(
                    f"policy target joint {index}={target[index]:.6f} outside [{low}, {high}]"
                )
        return target

    def publish_policy_target(
        self,
        owner: str,
        *,
        step: int,
        target_q_43: Any,
        arm_sdk_weight: float | None = None,
    ) -> dict[str, Any]:
        target = self._validate_target(target_q_43)
        # Serialize against arm/disarm, but never hold the feedback-state lock
        # across DDS writes.  Unitree writes may briefly block; callbacks must
        # remain free to refresh the 50 ms state interlock during that time.
        with self._arm_transition_lock:
            state = self.snapshot()
            with self._lock:
                if owner != self._owner or self._profile is None:
                    raise InterlockError("caller does not own the armed policy motor path")
                if self._fault is not None:
                    raise InterlockError(f"policy motor path faulted: {self._fault}")
                if step != self._last_step + 1:
                    raise InterlockError("policy command step is not the next contiguous step")
                # A completed button pulse is historical information, not a
                # current operator override.  Requiring the arming sequence to
                # remain unchanged made an incidental press abort much later,
                # after the remote had returned to neutral.  A presently held
                # button or displaced axis still aborts immediately.
                if state["remote"]["neutral"] is not True:
                    self._fault = "wireless_remote_non_neutral"
                    fault = self._fault
                elif state["mode_machine"] not in self.config.allowed_mode_machine:
                    self._fault = "mode_machine_changed"
                    fault = self._fault
                else:
                    fault = None
                profile = self._profile
            if fault is not None:
                self.disarm(fault)
                raise InterlockError(fault)
            if arm_sdk_weight is not None:
                weight = float(arm_sdk_weight)
                if not math.isfinite(weight) or not 0.0 <= weight <= profile.arm_sdk_weight:
                    raise InterlockError(
                        "ramp weight must be finite and within [0, qualified profile weight]"
                    )
                profile = replace(profile, arm_sdk_weight=weight)
            upper = list(target[12:22]) + list(target[29:36])
            right_wire = right_wire_from_policy(target[36:43])
            try:
                self.backend.publish_policy_command(tuple(upper), right_wire, profile)
            except BaseException as exc:
                with self._lock:
                    self._fault = f"command_publish_failed: {exc}"
                    fault = self._fault
                self.disarm(fault)
                raise
            with self._lock:
                self._last_step = step
                self._last_command_at = self.monotonic()
                self._commands_sent += 2
            return {
                "schema": "wendy.g1.reference-residual-publish-receipt.v1",
                "owner": owner,
                "step": step,
                "target_q_43": list(target),
                "right_hand_wire_q_7": list(right_wire),
                "arm_sdk_weight": profile.arm_sdk_weight,
                "dds_writes_accepted": 2,
                "meaning": "publisher_accepted_not_physical_attainment",
            }

    def read_fsm(self) -> tuple[int, int]:
        return self.backend.read_fsm()

    def stop_move(self) -> None:
        self.backend.stop_move()

    def select_ai_mode(self) -> dict[str, Any]:
        return self.backend.select_ai_mode()

    def set_fsm_id(self, fsm_id: int) -> None:
        self.backend.set_fsm_id(fsm_id)

    def disarm(self, reason: str = "requested_stop") -> None:
        with self._arm_transition_lock:
            self._disarm_serialized(reason)

    def _disarm_serialized(self, reason: str) -> None:
        with self._lock:
            if self._owner is None:
                return
            self._watchdog_stop.set()
            release_error: str | None = None
            interruption: BaseException | None = None
            try:
                self.backend.release()
                self._commands_sent += 2
            except (KeyboardInterrupt, SystemExit) as exc:
                release_error = str(exc) or type(exc).__name__
                interruption = exc
            except Exception as exc:
                release_error = str(exc)
            if reason != "requested_stop" or release_error:
                self._fault = reason if release_error is None else f"{reason}; release_failed: {release_error}"
            self._owner = None
            self._profile = None
            self._armed_remote_sequence = None
            self._last_command_at = None
            self._release_ownership()
            worker = self._watchdog_thread
            self._watchdog_thread = None
        if worker is not None and worker is not threading.current_thread():
            worker.join(timeout=1.0)
        if interruption is not None:
            raise interruption
        if release_error:
            raise InterlockError(f"policy disarm was incomplete: {release_error}")

    def status(self) -> dict[str, Any]:
        with self._lock:
            return {
                "schema": "wendy.g1.reference-residual-unitree-io-status.v1",
                "motion_enabled": self.config.motion_enabled,
                "publishers_armed": self._owner is not None,
                "owner": self._owner,
                "last_step": self._last_step,
                "dds_writes_accepted": self._commands_sent,
                "fault": self._fault,
                "hand_mapping_id": HAND_MAPPING_ID,
            }

    def close(self) -> None:
        try:
            self.disarm("runtime_close")
        finally:
            self.backend.close()
