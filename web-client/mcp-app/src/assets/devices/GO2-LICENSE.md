# Unitree Go2 display model

`go2.web.glb` is the homepage hero's Go2 (311 KB, Draco-compressed, about 21k unique triangles instanced across the legs).

## Provenance

- Source: [MuJoCo Menagerie `unitree_go2`](https://github.com/google-deepmind/mujoco_menagerie/tree/main/unitree_go2), `go2.xml` plus the 16 OBJ meshes in `assets/`.
- Menagerie derives that model from Unitree's public URDF, [`unitree_ros/robots/go2_description`](https://github.com/unitreerobotics/unitree_ros/tree/master/robots/go2_description). We use the Menagerie OBJ meshes because Blender 5 no longer imports the URDF's Collada (`.dae`) meshes.

## Rebuild

```sh
Blender -b --factory-startup --python scripts/build-go2-glb.py -- <unitree_go2 dir> public/models/unitree-go2/go2.web.glb
```

The script mirrors the MJCF body tree as named empties (`go2 > base > {FL,FR,RL,RR}_hip > *_thigh > *_calf > *_foot`), decimates each source mesh once (ratios in the script), reuses it for every leg that shares it, and exports a Y-up GLB with Draco geometry.

In the GLB, +X is forward, +Y is up, and the robot's left is -Z. Abduction joints turn about +X. Thigh and knee joints turn about -Z, so a MuJoCo joint angle `q` becomes `rotation.z = -q`. `*_foot` marks the centre of the 22 mm foot contact sphere. Materials keep the MJCF names (`black`, `white`, `gray`, `metal`); the site restyles them by name.

## License

The Go2 description is distributed under Unitree's BSD 3-Clause license:

```text
Copyright (c) 2016-2022 HangZhou YuShu TECHNOLOGY CO.,LTD. ("Unitree Robotics")
All rights reserved.

Redistribution and use in source and binary forms, with or without
modification, are permitted provided that the following conditions are met:

* Redistributions of source code must retain the above copyright notice, this
  list of conditions and the following disclaimer.

* Redistributions in binary form must reproduce the above copyright notice,
  this list of conditions and the following disclaimer in the documentation
  and/or other materials provided with the distribution.

* Neither the name of the copyright holder nor the names of its
  contributors may be used to endorse or promote products derived from
  this software without specific prior written permission.

THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS"
AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE
IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE
DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE
FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL
DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR
SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER
CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY,
OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.
```
