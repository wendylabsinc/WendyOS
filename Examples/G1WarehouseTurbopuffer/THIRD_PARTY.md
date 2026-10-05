# Third-party files

These files are not stored in the repository. `python -m warehouse.assets`
downloads each one from an immutable commit URL and checks it against the SHA-256
in `assets.lock.json`. The Dockerfile runs the same step, so the image never
contains an unverified copy.

## Unitree G1 model and GR00T WBC policies

- Source: [NVlabs/GR00T-WholeBodyControl](https://github.com/NVlabs/GR00T-WholeBodyControl),
  commit `b042411fae38ee4d1af9aac82a37a1f8d14d6dd0`, directory
  `decoupled_wbc/sim2mujoco/resources/robots/g1/`.
- Files, saved under `models/g1/`:
  - `g1_gear_wbc.xml`: the 29-DoF G1 with Dex3 hands.
  - `g1_gear_wbc.yaml`: policy gains, scales and default pose.
  - The 49 STL meshes the model uses, plus `README.md`.
  - `policy/GR00T-WholeBodyControl-Balance.onnx` and `policy/GR00T-WholeBodyControl-Walk.onnx`.
  - `policy/NVIDIA Open Model License` and the repository's `LICENSE`.
- Licenses:
  - The robot description is Unitree Robotics' G1 model, which Unitree publishes
    under BSD-3-Clause (`unitree_ros`, `unitree_mujoco`). The GR00T repository
    ships it with its code under Apache-2.0.
  - The two policies are NVIDIA model weights under the NVIDIA Open Model
    License, which allows commercial use. `models/g1/NOTICE` carries the
    attribution that license requires when the weights are redistributed (for
    example in an image built from this Dockerfile): "Licensed by NVIDIA
    Corporation under the NVIDIA Open Model License".
- The files are used unmodified. At load time:
  - `warehouse/world.py` adds palm sites, grasp constraints, contact exclusions
    and the warehouse to the model, poses the Dex3 fingers within their joint
    limits (the model fixes those joints at zero), and turns off collisions for
    the Dex3 thumbs.
  - `warehouse/robot.py` runs the policies with the upstream `sim2mujoco`
    runner's observation layout and gains.

## three.js

- Source: [three.js r180 (0.180.0)](https://github.com/mrdoob/three.js/releases/tag/r180),
  commit `0af9729d0c143a86a1d725d6e2c3ad83301f3f34`.
- Files: `build/three.module.js`, `build/three.core.js`,
  `examples/jsm/controls/OrbitControls.js`,
  `examples/jsm/utils/BufferGeometryUtils.js` and `LICENSE`, saved under
  `warehouse/static/three/`.
- License: MIT.
- The files are used unmodified. `index.html` maps the `three` and
  `three/addons/` imports onto them, so the viewer needs no package install and
  no CDN.

## Python packages

`requirements.txt` pins MuJoCo (Apache-2.0), NumPy (BSD-3-Clause), ONNX Runtime
(MIT), PyYAML (MIT) and the turbopuffer Python client (MIT). pip installs
them from PyPI when the image is built.
