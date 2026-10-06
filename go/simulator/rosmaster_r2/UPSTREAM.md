# Sources and model limits

Yahboom's [ROSMASTER R2 repository](https://github.com/YahboomTechnology/ROSMASTER-R2)
describes the Ackermann steering chassis, rear drive motors, lidar and camera.
Its [product page](https://category.yahboom.net/products/rosmaster-r2) advertises
a maximum speed of 1.8 m/s. Reviewed on 2026-09-29.

The simulator's chassis geometry and steering/acceleration limits are authored
approximations, not measured specifications. The visual model uses original
procedural geometry based on the black decks,
red wheel rims, exposed boards and sensor housings visible in the repository
photo. It represents the Nano configuration without the optional display.
Tire tread, mounting hardware, circuit-board details and lens materials are
authored visual approximations. Concrete grain, labels and reflection lighting
are generated in the browser. The visual detail does not change the kinematic
model. No Yahboom code, meshes, product images or firmware
are redistributed. This runtime does not claim factory driver compatibility.

The planar bicycle model integrates rear-axle velocity with
`yaw_rate = speed * tan(steering) / wheelbase`. Left and right front wheels use
their respective Ackermann angles. Wheel speeds follow each wheel's turning
radius. An oriented rectangular footprint stops at obstacles and room walls.

Three.js and OrbitControls are copied from the repository's existing G1 viewer
vendor bundle. Their MIT license is in `licenses/three.LICENSE`.
The immutable ROS base image matches the existing Go2 and G1 runtimes.
The DDS isolation bootstrap follows those runtimes with separately owned R2
firewall rules. The legacy fallback matches RTPS bytes anywhere in UDP packets.

The camera uses ideal pinhole rays against the same boxes, walls and floor as
the world. Field of view, mounting, 320×240 resolution, and 0.2–4 m range are
simulator choices, not factory HP60C calibration. Braking is an authored 2.4 m/s²
limit; tire and drivetrain dynamics remain unmodeled.
