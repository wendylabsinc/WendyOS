from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import sys
import urllib.error
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[1]


def parser():
    root = argparse.ArgumentParser(description="G1 Coke demo: expert simulation, learned weights, and Wendy robot runtime")
    commands = root.add_subparsers(dest="command", required=True)
    serve = commands.add_parser("serve", help="Start the ROS 2 Coke scene and operator screen")
    serve.add_argument("--host", default=os.environ.get("COKE_HOST", "127.0.0.1"))
    serve.add_argument("--port", type=int, default=int(os.environ.get("COKE_PORT", "8892")))
    serve.add_argument("--local", action="store_true", help="Local preview without ROS 2; VM deployment uses ROS 2 by default")
    serve.add_argument("--policy-url", default=os.environ.get("COKE_POLICY_URL"), help="Jetson simulation inference origin, e.g. http://192.168.1.50:8098")
    serve.add_argument("--policy-timeout", type=float, default=float(os.environ.get("COKE_POLICY_TIMEOUT", "5")), help="Timeout per HIL HTTP attempt in seconds; one retry")
    inference = commands.add_parser("hil-serve", help="Run simulation policy inference on a Jetson, without robot I/O")
    inference.add_argument("--host", default=os.environ.get("COKE_HIL_HOST", "127.0.0.1"))
    inference.add_argument("--port", type=int, default=int(os.environ.get("COKE_HIL_PORT", "8098")))
    inference.add_argument("--device", default=os.environ.get("COKE_HIL_DEVICE", "cuda"))
    inference.add_argument("--control-device", default=os.environ.get("COKE_HIL_CONTROL_DEVICE", "cpu"))
    inference.add_argument("--bundle", type=Path, default=ROOT / "bundle")
    sim = commands.add_parser("sim", help="Open the 60-second native MuJoCo expert demo")
    sim.add_argument("--skip-policy-check", action="store_true", help="Skip the learned checkpoint parity check before replay")
    headless = commands.add_parser("simulate", help="Run expert physics without a window")
    headless.add_argument("--steps", type=int, choices=range(1, 2401), metavar="1..2400", default=2400)
    commands.add_parser("verify-policy", help="Run learned weights against all 100 golden RGB-D frames")
    for name in ("status", "run-policy", "stop"):
        command = commands.add_parser(name, help=f"{name} on the physical G1 adapter")
        command.add_argument("--adapter-url", required=True, help="For example http://127.0.0.1:8098 (through an authenticated Wendy tunnel)")
        if name != "status":
            command.add_argument("--operator-confirmed", action="store_true", help="Confirm the physical robot is attended and ready for this operation")
        if name == "run-policy":
            command.add_argument("--steps", type=int, required=True, help="Bounded policy steps, 1..7722 at 40 Hz")
    return root


def robot_request(args):
    if args.command == "status":
        path, body, timeout = "/state", None, 5
    else:
        if not args.operator_confirmed:
            raise ValueError("Physical commands require --operator-confirmed")
        if args.command == "run-policy" and not 1 <= args.steps <= 7722:
            raise ValueError("Policy steps must be within 1..7722")
        action = "run" if args.command == "run-policy" else "stop"
        body = {"schema": f"wendy.g1.reference-residual-physical-{action}-request.v1", "command_id": uuid.uuid4().hex, "operator_confirmed": True}
        if action == "run":
            body["maximum_policy_steps"] = args.steps
        path = f"/api/reference-residual/{args.command}"
        timeout = max(60, 30 + args.steps / 40) if action == "run" else 30
    request = urllib.request.Request(args.adapter_url.rstrip("/") + path, data=None if body is None else json.dumps(body).encode(), headers={"Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(request, timeout=timeout) as response:
            result = json.load(response)
    except urllib.error.HTTPError as exc:
        raise RuntimeError(f"Adapter HTTP {exc.code}: {exc.read().decode()}") from exc
    if args.command == "run-policy" and result.get("policy_steps_completed") != args.steps:
        raise RuntimeError(f"Policy run did not complete: {json.dumps(result)}")
    return result


def main():
    args = parser().parse_args()
    try:
        if args.command == "serve":
            if sys.platform == "darwin":
                os.environ.setdefault("MUJOCO_GL", "glfw")
                os.environ.setdefault("PYOPENGL_PLATFORM", "darwin")
            from .service import serve
            serve(ROOT, host=args.host, port=args.port, ros_enabled=not args.local,
                  policy_url=args.policy_url, policy_timeout=args.policy_timeout,
                  policy_token=os.environ.get("COKE_HIL_TOKEN", ""))
            return 0
        if args.command == "hil-serve":
            from .hil import serve_inference
            serve_inference(args.bundle, host=args.host, port=args.port,
                            device=args.device, control_device=args.control_device,
                            token=os.environ.get("COKE_HIL_TOKEN", ""))
            return 0
        if args.command in ("status", "run-policy", "stop"):
            result = robot_request(args)
        elif args.command == "verify-policy" or (args.command == "sim" and not args.skip_policy_check):
            from .policy import verify_policy

            print("Checking update-2525 weights against their recorded RGB-D trace...", file=sys.stderr)
            result = verify_policy(ROOT / "bundle")
            print(json.dumps(result, indent=2))
            if not result["passed"]:
                return 1
            if args.command == "verify-policy":
                return 0
        if args.command in ("sim", "simulate"):
            # The original renderer defaults to Linux EGL. Choose the native
            # desktop backend before importing it on macOS.
            if sys.platform == "darwin":
                os.environ.setdefault("MUJOCO_GL", "glfw")
                os.environ.setdefault("PYOPENGL_PLATFORM", "darwin")
            from .simulation import run

            result = run(ROOT / "expert", headless=args.command == "simulate", steps=getattr(args, "steps", 2400))
        print(json.dumps(result, indent=2))
        return 0
    except (ValueError, RuntimeError, OSError) as exc:
        print(f"Coke demo: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
