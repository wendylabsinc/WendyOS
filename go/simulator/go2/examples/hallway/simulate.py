"""Run the production controller with pinned Go2 dynamics and real MuJoCo raycasts."""
import argparse
import json
import math
from pathlib import Path
import sys
import threading
from types import SimpleNamespace as NS

from admission import HallwayApp
from go2_io import sport_request
from world import hallway_model


def root_path(value=None):
    root = Path(value).resolve() if value else Path(__file__).resolve().parents[2]
    if not (root / "go2_sim/simulation.py").is_file():
        raise ValueError("Pass --sim-root pointing to go/simulator/go2")
    sys.path.insert(0, str(root))
    return root


def run(layout="straight", width=1.4, seconds=45, *, sim_root=None, fault=None, prefer="left"):
    root = root_path(sim_root)
    import mujoco
    import numpy as np
    from go2_sim.simulation import Simulation, TIMESTEP
    from go2_sim.sensors import PhysicsSampler, LIDAR_POSITION
    from go2_sim.lidar import Lidar
    clock = [100.0]
    sim = Simulation(model=hallway_model(root / "assets", layout, width), monotonic=lambda: clock[0])
    runtime = NS(sim=sim, lock=threading.RLock())
    sampler, lidar = PhysicsSampler(sim), Lidar(seed=7)
    app = HallwayApp(max_seconds=min(300, max(5, seconds)), prefer=prefer)
    for _ in range(1000):
        clock[0] += TIMESTEP
        sim.step()
    token = sim.arm()
    origin = sim.data.qpos[:2].copy()
    odom_position = sim.data.qpos[:3].copy()
    trace, contacts, transitions = [], [], []
    started = False
    fault_at = None
    initial = clock[0]
    previous_odom = clock[0]
    def request():
        return NS(header=NS(identity=NS(), policy=NS()))
    for step in range(round((seconds + 1) / TIMESTEP)):
        now, elapsed = clock[0], clock[0] - initial
        if step % 10 == 0:
            sampler.capture(runtime)
            state = sampler.state()
            wall_ns = round(now * 1e9)
            stamp = NS(sec=wall_ns//1_000_000_000, nanosec=wall_ns%1_000_000_000)
            rotation = sampler.data.xmat[sim.body_id].reshape(3, 3)
            odom_position += rotation @ (state["linear_velocity_body"] + [.002, -.001, 0]) * (now-previous_odom)
            previous_odom = now
            w, x, y, z = map(float, state["quaternion_wxyz"])
            pose = NS(header=NS(frame_id="odom", stamp=stamp), child_frame_id="base_link",
                      pose=NS(pose=NS(position=NS(**dict(zip("xyz", map(float, odom_position)))),
                                      orientation=NS(x=x, y=y, z=z, w=w))))
            if not (fault == "odom" and elapsed > 4):
                app.odom(pose, wall_ns=wall_ns, now=now)
            if step % 50 == 0 and not (fault == "scan" and elapsed > 4):
                cloud = lidar.sample(sampler)
                xyz = cloud["xyz"].astype(np.float64) + np.array(LIDAR_POSITION)
                payload = xyz.astype('<f4').tobytes()
                message = NS(header=NS(frame_id="base_link", stamp=stamp), height=1, width=len(xyz),
                             point_step=12, row_step=len(payload), data=payload, is_bigendian=False,
                             fields=[NS(name=n, offset=i*4, datatype=7, count=1) for i,n in enumerate("xyz")])
                app.cloud(message, wall_ns=wall_ns, now=now)
        if step % 25 == 0:
            if not started and app.controller.observation_error(now) is None:
                started = app.controller.start(now)
            velocity = app.controller.tick(now)
            # Exercise the native Move request serialization before applying its
            # decoded velocities to the simulator's existing ownership boundary.
            wire = sport_request(request, velocity[0], 0.0, velocity[1])
            decoded = json.loads(wire.parameter)
            assert wire.header.identity.api_id == 1008
            sim.command_velocity(decoded['x'], decoded['y'], decoded['z'], token)
            label = (app.controller.state, app.controller.reason)
            if not transitions or tuple(transitions[-1]["state"]) != label:
                transitions.append({"at": round(elapsed, 3), "state": label})
            if started and not app.controller.active and fault_at is None:
                fault_at = elapsed
            if step % 250 == 0:
                trace.append({"t": round(elapsed, 3), "x": float(sim.data.qpos[0]),
                              "y": float(sim.data.qpos[1]), "yaw": app.controller.pose[2],
                              "command": velocity})
        for contact in sim.data.contact:
            names = [mujoco.mj_id2name(sim.model, mujoco.mjtObj.mjOBJ_GEOM, int(g)) or ""
                     for g in (contact.geom1, contact.geom2)]
            if any(n.startswith("hallway_") for n in names):
                contacts.append({"at": elapsed, "geoms": names})
        if sim.mode in ("fallen", "fault"):
            raise AssertionError(f"Physics failed: {sim.mode}")
        if fault_at is not None and elapsed - fault_at > .7:
            break
        clock[0] += TIMESTEP
        sim.step()
    sim.stop(token)
    for _ in range(350):
        clock[0] += TIMESTEP
        sim.step()
    result = {"layout": layout, "width_m": width, "started": started,
              "elapsed_s": round(clock[0]-initial, 3), "wall_contacts": len(contacts),
              "displacement_m": float(np.linalg.norm(sim.data.qpos[:2]-origin)),
              "final_position": sim.data.qpos[:3].tolist(), "mode": sim.mode,
              "stopped_at_s": fault_at, "fault": fault, "status": app.status(clock[0]),
              "transitions": transitions, "trace": trace,
              "scope": "MuJoCo dynamics, pinned walking policy, raycast clouds, native serialization; no DDS or hardware"}
    return result


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--sim-root")
    parser.add_argument("--layout", choices=("straight", "corner", "junction", "blocked"), default="corner")
    parser.add_argument("--width", type=float, default=1.4)
    parser.add_argument("--seconds", type=float, default=45)
    parser.add_argument("--fault", choices=("scan", "odom"))
    parser.add_argument("--prefer", choices=("left", "right"), default="left")
    parser.add_argument("--output", type=Path, default=Path("result.json"))
    args = parser.parse_args()
    result = run(args.layout, args.width, args.seconds, sim_root=args.sim_root, fault=args.fault, prefer=args.prefer)
    args.output.write_text(json.dumps(result, indent=2, allow_nan=False) + "\n")
    print(json.dumps({k:v for k,v in result.items() if k != "trace"}, indent=2, allow_nan=False))
    if not result["started"] or result["wall_contacts"]:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
