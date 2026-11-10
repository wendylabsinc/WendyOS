# Installation models

These are copies of Wendy's existing assets, bundled for offline setup guidance.
Source models remain unchanged. No runtime file paths or downloads depend on the
neighboring repositories.

- `models/jetson-orin-nano.glb`: `marketing-website/public/models/quickstart/jetson-orin-nano.glb`, the existing cleaned, simplified display copy of the high-quality Orin Nano model. J14 anchors were checked against its visible single row of pins and NVIDIA SP-11324-001 v1.3, figure 1-4 and table 3-4. Looking from the rear reverses the pin order. The orange recovery bridge is a guidance overlay, not original board geometry.
- `models/dragonwing-iq-9075.glb`: `marketing-website/public/models/qualcomm-iq-9075/qualcomm-iq-9075-evk.glb`. This is Wendy's visualization reconstructed from published dimensions, photographs and product artwork, not official Qualcomm CAD. Unpublished dimensions are estimates. Original named meshes are retained so USB0, JPWR and the white SW2-3 actuator can be selected without estimating their positions. It depicts the production IQ-9075 EVK only.
- `models/raspberry-pi-5.glb`: `wendy-sandbox/image/shell/models/raspberry-pi-5.glb`. The microSD anchor is on the visible underside socket. The asset does not include an M.2 HAT; the NVMe instructions say so.

The JavaScript uses normalized coordinates for the Orin and Pi display copies.
If either model changes, recheck the camera presets and anchors visually before
updating the bundled copy. Native tests load all three assets through WebKit's
local URL scheme and attach snapshots of the pin/switch/card steps.

Hardware references:

- https://docs.nvidia.com/jetson/orin-nano-devkit/user-guide/latest/howto.html#force-recovery-mode
- https://developer.nvidia.com/downloads/assets/embedded/secure/jetson/orin_nano/docs/jetson_orin_nano_devkit_carrier_board_specification_sp.pdf
- https://github.com/qualcomm-linux/qcom-linux-skills/blob/main/skills/qcom-flash-qdl/references/entering-edl.md

The production IQ-9075 uses SW2-3; the production IQ-8275 uses S2-8. Beta EVKs
use different switches and require their revision's hardware guide. Do not reuse
the IQ-9075 mesh as an IQ-8275 location reference.

`vendor/` contains the unmodified required modules from Three.js 0.172.0, from
`marketing-website/node_modules/three`. `draco/` contains that website's decoder.
Their MIT and Apache 2.0 notices are in the app's Notices directory.
