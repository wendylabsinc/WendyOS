# SO-101 follower model provenance

The model is the official TheRobotStudio/SO-ARM100 SO-101 follower at
`a758567c3978dfeefe282ede0500085a48fe8f78`, from `Simulation/SO101/`.
https://github.com/TheRobotStudio/SO-ARM100/tree/a758567c3978dfeefe282ede0500085a48fe8f78/Simulation/SO101

`assets.lock.json` preserves the SHA-256 hashes from the WendyLabs
`demos/lerobot-sim` reference app. The build downloads only those files and
verifies every byte, including the unmodified MJCF, meshes, upstream README and
Apache-2.0 license. `licenses/SO-ARM100.LICENSE` is also shipped with the source.
`model/scene.xml` is the reference app's authored lab scene. Runtime physics,
rendering and bounded joint commands derive from that app; the source cache,
scene wire format and browser renderer follow the built-in Go2/G1 runtimes.
Three.js and OrbitControls are MIT licensed; see `licenses/three.LICENSE`.
`viewer-assets.lock.json` pins Three.js 0.180.0 to upstream commit
`0af9729d0c143a86a1d725d6e2c3ad83301f3f34` and hashes each downloaded module.
The build fetches and verifies these modules alongside the model assets, then
rewrites OrbitControls' bare import to the local Three.js module. The browser
serves these bundled files locally and does not fetch a CDN at runtime.

Python 3.12, MuJoCo 3.3.7 and CPU OSMesa rendering are used in the Linux
container on arm64 and amd64. No serial, USB, camera device or physical motor
backend is included. There is no ROS/DDS publisher or locomotion policy.
