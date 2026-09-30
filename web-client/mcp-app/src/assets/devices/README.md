# Static device images

These images illustrate the selected device family; they do not establish hardware capabilities. Brand names and trademarks belong to their owners.

## G1 and Go2

`g1.webp` and `go2.webp` are 768×768 transparent renders of the existing repository models, with neutral lighting and a fixed camera. They contain no photography or generated geometry. The desktop workspace displays the image without loading a 3D renderer.

- G1: `Examples/G1FruitNinjaMujoco/models/unitree_g1/g1_29dof.xml` and its referenced STL meshes. The rendering script preserves the MJCF body transforms and visual mesh colors at the default joint configuration. Source: Unitree `unitree_mujoco`, commit `ae6a8403e272733e9996ef59990880330496177f`. See that directory's `UPSTREAM.md` and this directory's `G1-LICENSE.txt` (BSD 3-Clause).
- Go2: `go/internal/cli/mcp/desktop_assets/go2.glb`, preserving its existing pose, meshes, and materials. Its source is MuJoCo Menagerie's `unitree_go2`, derived from Unitree's public robot description. See `GO2-LICENSE.md` here for the upstream model provenance and BSD 3-Clause license.

Rebuild from the repository root after installing the MCP app dependencies:

```sh
npm --prefix web-client/mcp-app ci
node web-client/mcp-app/scripts/render-robot-images.mjs
```

The script uses the existing Three.js and esbuild dependencies with an isolated headless Chrome process. It serves the local model files on a temporary loopback port, writes the two WebP images, and cleans up its temporary browser profile. `CHROME_BIN` can select a different Chrome/Chromium executable; the default is the standard macOS Chrome path. Exact rasterization can vary by browser and graphics driver.

## Other platforms

The other platform images are Wendy's existing 1200×900 WebP product renders with transparency, produced from the shared marketing models. They were copied from the sibling `marketing-website/public/images/platform` directory; that directory's `README.md` records their Blender rendering provenance.
