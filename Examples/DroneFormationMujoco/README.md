# Drone formation in changing wind: MuJoCo with Wendy

Twelve Bitcraze Crazyflie 2 quadrotors fly a looping formation show through
changing wind. The swarm takes off from its pads, holds a grid in a steady
breeze, turns into a rotating ring as the wind shifts and gust fronts roll
through, flies a chevron into a crosswind while a vortex drifts across it,
spells a W, and lands. The loop lasts 93 seconds.

Everything runs in one container: MuJoCo physics, the flight controllers and a
small web server that streams the swarm to a three.js viewer in your browser.

## Run it with Wendy

On your Mac, with Docker Desktop running:

```sh
wendy run --device docker
```

`wendy run` builds the image, starts it with `compose.yml`, and streams the
container's logs. Open <http://localhost:8879>. Press Ctrl-C to stop it.

On a WendyOS device:

```sh
wendy run --device <your-device>
```

`wendy.json` asks for host networking and declares port 8879 as the app's HTTP
port, so `wendy run` waits for the server to come up, prints the app's address
and opens it in your browser.

## What is simulated

MuJoCo integrates every airframe with RK4 at 500 Hz. Each drone feels:

- four rotor thrusts, each with a 20 ms first-order motor lag;
- each rotor's drag torque, which is how the drone yaws;
- gravity and floor contact;
- aerodynamic drag from the wind: quadratic body drag plus rotor drag that
  grows with thrust.

The wind field (`drone_formation/wind.py`) is deterministic and changes
pattern over the show: calm, a steady breeze, a 90° wind shift, travelling gust
fronts, a crosswind with turbulence, a drifting vortex, then easing back to
calm. Wind speed falls off near the floor like a real surface layer.

Each drone's controller (`drone_formation/control.py`) runs at 250 Hz on its
own simulated sensors: an IMU and a motion-capture position, the way indoor
Crazyflie swarms fly. A PID position loop feeds a geometric attitude
controller, and a mixer turns thrust and torque into four rotor commands. The
controller never sees the wind; its integral term is what holds each drone in
place against it. Formation changes use minimum-jerk transitions, Hungarian
slot assignment and vertical layering so paths never cross.

Run the whole show headless and print how far the wind pushed the drones:

```sh
python -m drone_formation.simulation
```

## What the viewer adds

The browser only draws what the simulator streams (poses, rotor thrusts and a
sampled wind grid at 60 Hz and 15 Hz). Wind streaks, windsocks, the soft
marker light under each drone and the lines that link a formation are
visualisations of that data. Propeller spin is drawn from each rotor's thrust.

Keys: **H** HUD, **W** wind streaks, **F** formation links, **L** marker
lights, **T** trails, **G** target slots, **C** slow cinematic orbit. URL
options: `?hud=0`, `?camera=cinematic`, `?particles=2000`.

## Endpoints

```text
GET  /              viewer
GET  /api/health    readiness (503 until the simulation is stepping)
GET  /api/status    MuJoCo version, rates, real-time factor, current phase
GET  /api/state     one snapshot of the swarm
GET  /api/stream    server-sent events: state at 60 Hz, wind grid at 15 Hz
GET  /api/scene     Crazyflie meshes for the viewer
POST /api/reset     restart the show
```

## Develop without Wendy

```sh
python3 -m venv .venv
. .venv/bin/activate
python -m pip install -r requirements.txt pytest
python -m drone_formation.assets   # fetch the pinned model and three.js
python -m drone_formation.server
pytest -q
```

`DRONE_FORMATION_SPEED=2` fast-forwards the show while you tweak the viewer.

## Change the show

- Wind phases: `PHASES` in `drone_formation/wind.py`.
- Formations and timing: `_plan()` in `drone_formation/choreography.py`.
- Gains and limits: the constants at the top of `drone_formation/control.py`.

## Third-party files

The Crazyflie 2 model comes from MuJoCo Menagerie (MIT), which converted it from
Bitcraze's URDF. The viewer uses three.js r180 (MIT). Neither is stored in this
repository. `python -m drone_formation.assets` downloads both from pinned
commits and checks every file against the SHA-256 in `assets.lock.json`. The
Docker build runs the same step. See `THIRD_PARTY.md`.
