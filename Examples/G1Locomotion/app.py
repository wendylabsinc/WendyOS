"""Finite Unitree SDK locomotion demo for Wendy's managed G1 MuJoCo profile."""

import argparse
from dataclasses import dataclass
import signal
import sys
import threading
import time


PERIOD = 0.05
LEASE = 0.2
DENIED = 3205


@dataclass(frozen=True)
class Step:
    name: str
    seconds: float
    vx: float = 0.0
    vy: float = 0.0
    yaw: float = 0.0


STEPS = (
    Step("Settle", 1.0),
    Step("Walk forward", 4.0, vx=0.3),
    Step("Walk a left arc", 4.0, vx=0.3, yaw=0.2),
    Step("Walk a right arc", 4.0, vx=0.3, yaw=-0.2),
    Step("Walk forward", 2.0, vx=0.3),
    Step("Stop", 2.0),
)


def check(code, operation):
    if code != 0:
        hint = " Control was revoked; restart the demo and grant it again." if code == DENIED else ""
        raise RuntimeError(f"{operation} failed with SDK code {code}.{hint}")


def run(client, stop, grant_timeout=120.0, *, clock=time.monotonic):
    """Wait for ownership using zero velocity, then stream one finite sequence."""
    try:
        print("Select this sport publisher in the simulator and click Give app control.", flush=True)
        deadline = clock() + grant_timeout
        while not stop.is_set():
            code = client.SetVelocity(0.0, 0.0, 0.0, LEASE)
            if code == 0:
                break
            if code != DENIED:
                check(code, "Waiting for simulator control")
            if clock() >= deadline:
                raise RuntimeError("Timed out waiting for Give app control in the simulator.")
            stop.wait(PERIOD)
        if stop.is_set():
            return
        code, fsm = client.GetFsmId()
        check(code, "GetFsmId")
        if fsm != 500:
            raise RuntimeError(f"Expected standing locomotion FSM 500, got {fsm}. Reset the simulator.")

        for step in STEPS:
            if stop.is_set():
                break
            print(f"{step.name}: {step.seconds:g}s, vx={step.vx:g}, vy={step.vy:g} m/s, yaw={step.yaw:g} rad/s", flush=True)
            deadline = clock() + step.seconds
            while not stop.is_set() and clock() < deadline:
                started = clock()
                check(client.SetVelocity(step.vx, step.vy, step.yaw, LEASE), step.name)
                stop.wait(max(0.0, min(PERIOD - (clock() - started), deadline - clock())))
    finally:
        # Use SetVelocity directly: the SDK's StopMove wrapper discards status.
        try:
            code = client.SetVelocity(0.0, 0.0, 0.0, LEASE)
            if code != 0:
                print(f"Final stop returned SDK code {code}; simulator watchdog remains active.", file=sys.stderr)
        except Exception as exc:
            print(f"Final stop failed: {exc}; simulator watchdog remains active.", file=sys.stderr)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--dry-run", action="store_true", help="Print the sequence without loading the SDK or connecting")
    args = parser.parse_args()
    if args.dry_run:
        for step in STEPS:
            print(f"{step.name}: {step.seconds:g}s, velocity=({step.vx:g}, {step.vy:g}, {step.yaw:g})")
        return 0

    stop = threading.Event()
    for sig in (signal.SIGINT, signal.SIGTERM):
        signal.signal(sig, lambda *_: stop.set())
    try:
        from unitree_sdk2py.core.channel import ChannelFactoryInitialize
        from unitree_sdk2py.g1.loco.g1_loco_client import LocoClient

        # Run inside the simulator VM's host network namespace.
        ChannelFactoryInitialize(0, "lo")
        client = LocoClient()
        client.SetTimeout(5.0)
        client.Init()
        code, _ = client.GetFsmId()
        check(code, "Discovering the simulator locomotion service")
        client.SetTimeout(0.15)
        run(client, stop)
    except Exception as exc:
        print(f"Demo failed: {exc}", file=sys.stderr)
        return 1
    print("Demo stopped." if stop.is_set() else "Demo complete.", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
