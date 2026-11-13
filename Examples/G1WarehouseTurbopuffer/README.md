# Warehouse G1 with turbopuffer memory: MuJoCo with Wendy

A Unitree G1 walks a small warehouse, lifts boxes off an inbound cart and
shelves each one where similar things already live. The robot is never told
which zone a box belongs in. It asks [turbopuffer](https://turbopuffer.com),
which stores every shelved item and each zone's description as text and embeds
it natively. When the cart is empty, someone asks for "a cable for my second
monitor": the robot looks up the best match, a DisplayPort cable that was on the
shelves before the shipment arrived, walks across the warehouse and brings it
back to the cart. The loop lasts a little over three minutes.

Everything runs in one container: MuJoCo physics, the G1's walking policy, the
arm and grasp control, and a small web server that streams the warehouse to a
three.js viewer in your browser.

## Run it with Wendy

Create an API key in the turbopuffer dashboard and export it in the shell you
deploy from. Set the region too if your organization is not in
`gcp-us-central1`:

```sh
export TURBOPUFFER_API_KEY=tpuf_...
export TURBOPUFFER_REGION=gcp-us-central1
```

On your Mac, with Docker Desktop running:

```sh
wendy run --device docker
```

Open <http://localhost:8880>. On a WendyOS device:

```sh
wendy run --device <your-device>
```

`wendy.json` copies `TURBOPUFFER_API_KEY`, `TURBOPUFFER_REGION` and
`TURBOPUFFER_NAMESPACE` from your shell into the container when you deploy, so
the key is never stored in the image or in this repository. Without a key the
app still runs, using a keyword stand-in that matches words instead of meaning.
The viewer says which one is answering.

## What turbopuffer does here

The robot's memory is one namespace (`wendy-g1-warehouse` by default). Each
loop starts by clearing it and writing twelve rows: the description of each
zone, and the three items already on each rack (one on the work shelf, two on
the top shelf). Every row's `label` is plain text that
turbopuffer embeds itself (`TURBOPUFFER_EMBED_MODEL`, default
`nvidia/nemotron-3-embed-8b`), so the app never runs an embedding model. Not
every model runs in every region; check turbopuffer's model list if you change
either.

For each box it picks up, the robot sends the box's label as a vector query
(`rank_by=("label", "ANN", ("Embed", label))`), shelves the box in the zone of
the nearest row, and writes the new item back with its bay. The request at the
end is the same query, filtered to items. `warehouse/memory.py` has all of it.

The viewer shows each query, its three nearest rows with their cosine distance,
and the round-trip and server time. It outlines the matching boxes and zone
signs in the 3D view.

## What is simulated

MuJoCo steps the world at 400 Hz. The G1 is the 29-joint model with Dex3 hands
from NVIDIA's GR00T whole-body-control repository.

- **Legs and waist.** NVIDIA's GR00T Decoupled WBC policies (Balance while
  standing, Walk while moving) run at 50 Hz and drive the 15 leg and waist
  joints through PD control, as in the upstream `sim2mujoco` runner. Their
  height and torso-pitch commands let the robot crouch to the low cart and lean
  in.
- **Arms.** A damped least-squares IK moves both palms when the hands work,
  with gravity compensation and a 25 N·m torque limit per joint. With empty
  hands the arms hang by the sides and swing with the opposite leg.
- **Grasping.** The Dex3 finger joints are fixed in this model, so the hands are
  posed as the real joints allow: thumb folded, fingers a little curled. The
  fingertips close on the box's sides, then MuJoCo constraints stand in for
  finger friction: a weld to the left hand and a pin at the right palm. The left
  arm steers the box and the right palm follows it, taking part of the load.
- **Skills** (`warehouse/skills.py`):
  - walk to a stance, then crouch and pick with both hands;
  - carry, stop short of the shelf and step in, set the box down, back away.
  - If the robot drifted out of reach or missed the grasp, it steps back and
    tries again.

Run the show headless and print what happens:

```sh
python -m warehouse.simulation --seconds 240
```

## What the viewer adds

The browser only draws what the simulator streams: body poses at 50 Hz, and
what the robot is doing and what its memory answered. The robot's meshes are
simplified on a 2 mm grid to about 190,000 triangles and sent once. Keys: **H**
HUD, **C** slow cinematic orbit. URL options: `?hud=0`, `?camera=cinematic`.

## Endpoints

```text
GET  /                viewer
GET  /api/health      readiness (503 until the walking policy is running)
GET  /api/status      MuJoCo version, rates, real-time factor, memory backend
GET  /api/state       one snapshot of the warehouse
GET  /api/stream      server-sent events: the state at 50 Hz
GET  /api/scene       warehouse layout, boxes, and how the robot's meshes are laid out
GET  /api/meshes.bin  the robot's simplified meshes
POST /api/reset       restart the show
```

## Develop without Wendy

```sh
python3 -m venv .venv
. .venv/bin/activate
python -m pip install -r requirements.txt pytest
python -m warehouse.assets   # fetch the pinned G1 model, policies and three.js
python -m warehouse.server
pytest -q
```

`G1_WAREHOUSE_SPEED=2` fast-forwards the show while you tweak the viewer. The
turbopuffer test runs only when `TURBOPUFFER_API_KEY` is set, and it uses a
temporary namespace that it deletes afterwards.

## Change the show

- Items, zones, their descriptions and the request: `warehouse/catalog.py`.
- What happens, in order: `warehouse/show.py`.
- Stances, reach and grasp: the constants at the top of `warehouse/skills.py`.

## Third-party files

The G1 model and meshes come from NVlabs/GR00T-WholeBodyControl, which ships
Unitree's G1 description. The Balance and Walk policies are NVIDIA model
weights under the NVIDIA Open Model License. The viewer uses three.js r180
(MIT). None of them is stored in this repository.

`python -m warehouse.assets` downloads them from pinned commits and checks every
file against the SHA-256 in `assets.lock.json`. The Docker build runs the same
step. See `THIRD_PARTY.md`.
