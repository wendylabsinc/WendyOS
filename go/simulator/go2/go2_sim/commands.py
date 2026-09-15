"""Admit identified ROS publishers through the same owner as browser controls.

The private Unix socket is fed by the bundled rclcpp ingress, which obtains
publisher identities from DDS metadata. It is not a public command API.
"""

import json
import math
from pathlib import Path
import re
import socket
import threading
import time

from .simulation import COMMAND_TIMEOUT, VELOCITY_LIMITS


def publisher_label(envelope):
    """Accept bounded ROS names for display, independently of command ownership."""
    name = envelope.get("node_name")
    namespace = envelope.get("node_namespace")
    token = r"[A-Za-z_][A-Za-z0-9_]*"
    if (not isinstance(name, str) or len(name) > 128 or not re.fullmatch(token, name)
            or not isinstance(namespace, str) or len(namespace) > 128
            or not re.fullmatch(r"/|(?:/" + token + r")+", namespace)):
        return {}
    return {"node_name": name, "node_namespace": namespace}


class ROSCommands:
    def __init__(self, runtime, path, *, auto_control=False,
                 monotonic_ns=time.monotonic_ns, wall_ns=time.time_ns):
        self.runtime = runtime
        self.path = Path(path)
        self.clock = monotonic_ns
        self.wall_clock = wall_ns
        self.auto_control = auto_control
        self.sources = {}
        self.blocked = set()
        self.owner = None
        self.token = None
        self.granted_ns = 0
        self.granted_wall_ns = 0
        self.accepted = 0
        self.rejected = 0
        self.last_error = None
        self.socket = None
        self.thread = None
        self.native_handler = None

    def start(self):
        self.path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        # Refuse an existing endpoint: never unlink another runtime's socket.
        self.socket = socket.socket(socket.AF_UNIX, socket.SOCK_DGRAM)
        self.socket.bind(str(self.path))
        self.path.chmod(0o600)
        self.socket.settimeout(0.1)
        self.thread = threading.Thread(target=self._receive, daemon=True)
        self.thread.start()

    def close(self):
        if self.thread is not None:
            self.thread.join(timeout=1)
        if self.socket is not None:
            self.socket.close()
            self.path.unlink(missing_ok=True)

    def revoke(self):
        """Called before reset/pause, while holding the runtime lock."""
        self.blocked.update(self.sources)
        self.owner = self.token = None

    def grant(self, gid):
        sim = self.runtime.sim
        self.runtime.ensure_running()
        if gid not in self.sources:
            raise ValueError("select a discovered ROS publisher")
        if gid in self.blocked:
            raise PermissionError("restart this ROS publisher before granting control after reset/pause")
        if self.clock() - self.sources[gid]["last_received_ns"] > 1_000_000_000:
            raise ValueError("ROS publisher is not currently sending commands")
        kind = self.sources[gid]["kind"]
        if kind == "motion_switcher":
            raise ValueError("motion-switcher mutation is outside this profile")
        if kind != "twist" and self.native_handler is None:
            raise ValueError("native Unitree adapter is not enabled")
        self.token = sim.arm(mode="lowlevel" if kind == "lowcmd" else "sport")
        self.owner = gid
        self.granted_ns = self.clock()
        self.granted_wall_ns = self.wall_clock()
        return {"publisher_gid": gid, "epoch": sim.epoch}

    def status(self):
        now = self.clock()
        return {
            "auto_control": self.auto_control,
            "owner": self.owner if self.token == self.runtime.sim.owner else None,
            "accepted": self.accepted, "rejected": self.rejected,
            "last_error": self.last_error,
            "sources": [{"publisher_gid": gid, "kind": source["kind"],
                         **publisher_label(source),
                         "age_ms": (now - source["last_received_ns"]) / 1e6,
                         "requires_restart": gid in self.blocked}
                        for gid, source in self.sources.items()],
        }

    def _auto_grant(self, gid):
        """Transfer sport control once, at discovery, under the runtime lock."""
        sim = self.runtime.sim
        if (gid in self.blocked or sim.mode not in {"standing", "moving"}
                or sim.control_mode != "sport" or getattr(self.runtime, "error", None)):
            return
        self.runtime.ensure_running()
        if sim.owner is not None:
            sim.release(sim.owner)
        self.grant(gid)

    def admit(self, envelope):
        """Validate a received datagram; caller holds the runtime lock."""
        if not isinstance(envelope, dict) or envelope.get("kind") not in {"twist", "sport", "motion_switcher", "lowcmd"}:
            raise ValueError("unsupported command envelope")
        kind = envelope["kind"]
        gid = envelope.get("publisher_gid")
        received = envelope.get("received_ns")
        source_time = envelope.get("source_timestamp_ns")
        velocity = envelope.get("velocity")
        if not isinstance(gid, str) or not re.fullmatch(r"[0-9a-f]{48}", gid):
            raise ValueError("invalid DDS publisher identity")
        if isinstance(received, bool) or not isinstance(received, int):
            raise ValueError("missing ingress timestamp")
        age = self.clock() - received
        if age < 0 or age >= int(COMMAND_TIMEOUT * 1e9):
            raise ValueError("expired ingress command")
        if isinstance(source_time, bool) or not isinstance(source_time, int):
            raise ValueError("missing DDS source timestamp")
        source_age = self.wall_clock() - source_time
        if source_age < -50_000_000 or source_age >= int(COMMAND_TIMEOUT * 1e9):
            raise ValueError("expired DDS command")
        if kind == "twist":
            if (not isinstance(velocity, list) or len(velocity) != 3 or
                    any(isinstance(v, bool) or not isinstance(v, (int, float)) for v in velocity)):
                raise ValueError("invalid velocity")
            # Check limits before isfinite, which can overflow on a huge JSON integer.
            if any(abs(v) > limit for v, limit in zip(velocity, VELOCITY_LIMITS.tolist())):
                raise ValueError(f"velocity exceeds limits {VELOCITY_LIMITS.tolist()}")
            if any(not math.isfinite(v) for v in velocity):
                raise ValueError("invalid velocity")
        if gid not in self.sources and len(self.sources) >= 4096:
            raise ValueError("publisher registry full; restart the runtime")
        previous = self.sources.get(gid)
        if previous and kind != previous["kind"]:
            raise ValueError("DDS publisher command kind changed")
        if previous and (received <= previous["last_received_ns"] or
                         source_time <= previous["source_timestamp_ns"]):
            self.rejected += 1
            return False
        label = publisher_label(envelope)
        if "node_name" not in envelope and "node_namespace" not in envelope:
            # Large native payloads may leave no room for optional metadata.
            # A previously resolved name belongs to this exact endpoint GID.
            label = publisher_label(previous or {})
        self.sources[gid] = {"kind": kind, "last_received_ns": received,
                             "source_timestamp_ns": source_time, **label}
        if previous is None and self.auto_control and kind == "twist":
            # Recording discovery consumes the attempt even when paused or unhealthy.
            # The discovery packet predates its grant and must never drive the robot.
            self._auto_grant(gid)
            self.rejected += 1
            return False
        owned = (gid == self.owner and gid not in self.blocked and self.token is not None and
                 self.token == self.runtime.sim.owner and received > self.granted_ns and
                 source_time > self.granted_wall_ns)
        if kind != "twist":
            if self.native_handler is None:
                raise ValueError("native Unitree adapter is not enabled")
            applied = self.native_handler(envelope, owned=owned)
            if applied:
                self.accepted += 1
                self.last_error = None
            elif not owned:
                self.rejected += 1
            return applied
        if not owned:
            self.rejected += 1
            return False
        self.runtime.command(velocity, self.token)
        # Preserve receipt time: queueing cannot renew an old command's lease.
        self.runtime.sim._last_received = received / 1e9
        self.accepted += 1
        self.last_error = None
        return True

    def _receive(self):
        while not self.runtime.stop_event.is_set():
            try:
                data = self.socket.recv(4097)
                if len(data) > 4096:
                    raise ValueError("command datagram too large")
                envelope = json.loads(data)
                with self.runtime.lock:
                    self.admit(envelope)
            except socket.timeout:
                continue
            except (ValueError, TypeError, PermissionError, RuntimeError) as exc:
                with self.runtime.lock:
                    self.rejected += 1
                    self.last_error = str(exc)
