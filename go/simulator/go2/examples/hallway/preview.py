"""Open an isolated hallway world in the existing MuJoCo browser sandbox."""
import argparse
import signal
import threading

from simulate import root_path
from world import hallway_model


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--sim-root")
    parser.add_argument("--layout", choices=("straight", "corner", "junction", "blocked"), default="corner")
    parser.add_argument("--width", type=float, default=1.4)
    parser.add_argument("--port", type=int, default=8895)
    parser.add_argument("--ros", action="store_true")
    args = parser.parse_args()
    root = root_path(args.sim_root)
    from go2_sim.simulation import Simulation
    from go2_sim.runtime import Runtime
    from go2_sim.server import Server, Handler
    import mujoco
    sim = Simulation(model=hallway_model(root / "assets", args.layout, args.width))
    contacts = [0]
    original_step = sim.step
    def step():
        original_step()
        for contact in sim.data.contact:
            if any((mujoco.mj_id2name(sim.model, mujoco.mjtObj.mjOBJ_GEOM, int(g)) or "").startswith("hallway_")
                   for g in (contact.geom1, contact.geom2)):
                contacts[0] += 1
    sim.step = step
    runtime = Runtime(simulation=sim, render=False, ros=args.ros)
    original_status = runtime.status
    def status():
        return {**original_status(), "hallway": {"layout": args.layout, "width_m": args.width,
                                                  "wall_contacts": contacts[0]}}
    runtime.status = status
    server = Server(("127.0.0.1", args.port), Handler)
    server.runtime = runtime
    runtime.start()
    def shutdown(*_):
        threading.Thread(target=server.shutdown, daemon=True).start()
    signal.signal(signal.SIGTERM, shutdown)
    signal.signal(signal.SIGINT, shutdown)
    print(f"Hallway sandbox: http://127.0.0.1:{args.port}", flush=True)
    try:
        server.serve_forever(poll_interval=.2)
    finally:
        runtime.close()
        server.server_close()


if __name__ == "__main__":
    main()
