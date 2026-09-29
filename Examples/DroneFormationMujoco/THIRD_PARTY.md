# Third-party files

These files are not stored in the repository. `python -m drone_formation.assets`
downloads each one from an immutable commit URL and checks it against the SHA-256
in `assets.lock.json`. The Dockerfile runs the same step, so the image never
contains an unverified copy.

## Bitcraze Crazyflie 2 model

- Source: [MuJoCo Menagerie](https://github.com/google-deepmind/mujoco_menagerie),
  commit `71f066ad0be9cd271f7ed58c030243ef157af9f4`, directory `bitcraze_crazyflie_2/`.
- Files: `cf2.xml`, its 39 OBJ meshes and `LICENSE`, saved under
  `models/bitcraze_crazyflie_2/`.
- License: MIT. Menagerie converted the model from Bitcraze's `crazyflie_description` URDF.
- The files are used unmodified. `drone_formation/model.py` loads `cf2.xml`, copies the
  airframe once per drone, and replaces the upstream thrust and body-moment actuators
  with four rotor actuators per drone.

## three.js

- Source: [three.js r180 (0.180.0)](https://github.com/mrdoob/three.js/releases/tag/r180),
  commit `0af9729d0c143a86a1d725d6e2c3ad83301f3f34`.
- Files: `build/three.module.js`, `build/three.core.js`,
  `examples/jsm/controls/OrbitControls.js` and `LICENSE`, saved under
  `drone_formation/static/three/`.
- License: MIT.
- The files are used unmodified. `index.html` maps the `three` and `three/addons/`
  imports onto them, so the viewer needs no package install and no CDN.
