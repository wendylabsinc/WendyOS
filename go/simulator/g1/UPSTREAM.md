# G1 model and locomotion policy

Bundle `g1-29dof-velocity-v0-4960b847-v1` pairs the full floating 29-DoF G1
model with Unitree's pretrained velocity controller. `assets.lock.json` pins
every downloaded byte; `python3 tools/fetch_assets.py --check` verifies it offline.

* Model and floor scene: [unitree_mujoco](https://github.com/unitreerobotics/unitree_mujoco/tree/ae6a8403e272733e9996ef59990880330496177f/unitree_robots/g1),
  commit `ae6a8403e272733e9996ef59990880330496177f`, BSD-3-Clause.
  The XML and all 36 referenced STL files are unmodified upstream assets.
  The same mesh bytes are present in the local G1FruitNinjaMujoco example;
  each reused mesh was verified against the pinned upstream Git blob ID.
  This runtime does not use Fruit Ninja's supported-pelvis scene or scripted arms.
* Policy and deployment configuration: [unitree_rl_lab](https://github.com/unitreerobotics/unitree_rl_lab/tree/4960b84732b0c2ec593dccbfe963fda1bcd7b1e3/deploy/robots/g1_29dof/config/policy/velocity/v0),
  commit `4960b84732b0c2ec593dccbfe963fda1bcd7b1e3`, Apache-2.0.
  Paths are `exported/policy.onnx` and `params/deploy.yaml`. Repository license
  is named `LICENCE`; it is preserved as `licenses/unitree_rl_lab.LICENSE`.
  The upstream README demonstrates this bundle with the 29-DoF MuJoCo model,
  including walking after disabling its setup elastic band.

Model XML SHA-256: `423e28bd718b19f7a65cda539b6f794ddbb268b4b9bdbd85f4bd982b30729617`.
Policy SHA-256: `610c27e463a8f666aa50a06346678c00b4df3859f10b54bcc1f817c28251406f`.
Deployment YAML SHA-256: `64b04c0596a7010f39f8ac6e9ec46dc750141063cdcf2d42c83ecc444e57bc63`.

## Controller contract

MuJoCo advances at 500 Hz (2 ms); policy inference runs at 50 Hz (20 ms).
ONNX Runtime CPU receives `obs`, float32 `[1,480]`, and produces `actions`,
float32 `[1,29]`. No GPU, Isaac Lab, Torch, or recurrent state is required.
The local controller follows the pinned upstream C++ deployment's observation,
history and action conventions; it does not synthesize a root trajectory.

Observations concatenate these six terms, each with five frames oldest first:
pelvis angular velocity times 0.2; pelvis projected gravity; requested body
velocity `[vx,vy,wz]`; joint position minus default; joint velocity times 0.05;
and the previous raw action. History is term-major, not five whole observations.
Reset repeats the initial sample five times with zero prior action. Deployment
does not clip observations or actions. Targets equal default plus 0.25 times
raw action; actuator torque limits still apply to the physical PD controller.

The policy-to-motor permutation is
`[0,6,12,1,7,13,2,8,14,3,9,15,22,4,10,16,23,5,11,17,24,18,25,19,26,20,27,21,28]`.
Policy defaults, observations and actions follow that permutation. Stiffness
and damping in the YAML already use motor order; the upstream
`deploy/include/FSM/State_RLBase.h` assigns them directly to motor indices.
The simulation's public joint arrays and low-level commands use motor order.
The primary `imu` site on the pelvis supplies policy orientation and gyro;
the model's separate secondary IMU is not substituted for it.

Command ranges are vx [-0.5,1.0] m/s, vy [-0.3,0.3] m/s and yaw rate
[-0.2,0.2] rad/s. The simulation adds command ownership and watchdogs, physical
fall detection, and torque bounds. It does not implement a get-up policy,
supported pelvis, scripted arms, or standing/sitting posture animations.
Reset establishes the initial pose; subsequent movement uses only actuator
torque and `mj_step`.

The supplied policy has limited tracking when initiating movement at small
commands from rest: local floating-model checks observed very little movement
at vx +0.1/+0.2 m/s or pure vy +/-0.2 m/s, and weak turning in place. Forward
and backward walking at 0.3 m/s, lateral walking at +/-0.3 m/s, and turning
while walking at vx 0.3 m/s with yaw rate +/-0.2 rad/s produce clear physical
movement. These are measured properties of this pretrained bundle, not command
deadbands inserted by this runtime. Commands are passed through the documented
acceleration bound without amplification or startup kicks. Consumers must use
observed odometry to assess motion; a nonzero request does not guarantee exact
velocity tracking. The training configuration and upstream deploy observation
adapter apply no extra low-command scaling that this runtime omits.

The pinned 1.66 MB policy measured about 0.041 ms per inference on the development
host with one ONNX Runtime thread; this is not a VM performance guarantee.

## Other runtime dependencies

Native ROS interface/SDK provenance is recorded separately in `unitree.lock.json`
and the corresponding license files. MuJoCo and ONNX Runtime versions are fixed
in `requirements.txt`; the browser viewer retains its Three.js MIT license.
