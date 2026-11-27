# WendyOS markerless localization adapter

This local command compiles the existing WendyOS `SiteLocalization.cpp` into a
small process boundary. It does not copy or modify that solver. The solver matches
camera texture to 3D SIFT landmarks captured by the iPhone LiDAR site scanner.
It requires a visual reference map of the same physical space; it does not create
metric room coordinates from an arbitrary standalone camera image.

Build using the existing WendyOS OpenCV static dependency on macOS 26:

```sh
bash adapters/wendy-localization/build.sh /absolute/wendyos /private/path/world-site-localize
```

The build output and provenance manifest must stay outside this repository.

## Process contract

```sh
world-site-localize --landmarks map.f32 --image frame.pgm --mode joint
world-site-localize --landmarks map.f32 --image frame.pgm --mode estimated
world-site-localize --landmarks map.f32 --image frame.pgm --mode measured \
  --lens 'fx,fy,cx,cy,k1,k2,p1,p2,k3'
```

`assumed` also requires `--lens` and preserves estimated-lens semantics. `joint`
tries the existing joint pose/lens calibration; `estimated` uses the existing
approximate-lens search. The parent chooses any fallback and verifies independent
frames. A numeric lens supplied as `measured` must have prior calibration evidence;
the command cannot establish that provenance itself.

The landmark file is little-endian IEEE float32, 120–12,000 records. Each record
contains metric scan-world x/y/z followed by 128 SIFT descriptor floats. The image
is bounded binary PGM with exactly three newline-terminated header lines: `P5`,
`WIDTH HEIGHT`, `255`, followed by exactly WIDTH×HEIGHT grayscale bytes. Image
dimensions are 32–8192 and at most 32 million pixels. Inputs must be regular files.

Stdout is one JSON object with schema `wendy.site-localization.result.v1`:
`success`, `mode`, `lens_assumed`, `reason`, `matches`, `inliers`, `fitted_lens`, `fx`, `fy`, `cx`, `cy`,
`rms`, `distortion[5]`, and `world_from_camera[16]`. The transform is column-major
and maps OpenCV optical coordinates, x right, y down, z forward, into the original
metric scan world. Translation is at indices 12–14. The parent must bind that
world to its map revision; no geographic or site alignment is inferred here.

`rms` is held-out reprojection error in original image pixels, not metric position
uncertainty. `fitted_lens` is 0 for a supplied lens, 1 for joint fit, or 2 for
approximate fitting. Crucially, `assumed` mode can return `fitted_lens:0`, because
it keeps the supplied assumed lens. The extra `lens_assumed` field stays true in
that case; a second-frame solve must never upgrade an assumed lens to measured.
A rejected solve returns exit 0 with `success:false`
and a reason code. Invalid input or runtime failure returns exit 2 with a bounded
generic JSON error. No raw image, file path or dependency diagnostic is echoed.

## Verification

`test.sh WENDYOS_ROOT BRIDGE NEW_PRIVATE_OUTPUT` builds a fixture generator using
the same native library. Generated textured images and synthetic metric depth
create a reference map through `WSMapAdd`, then the actual command recovers the
known transform from two differently lit frames. It also checks the joint,
estimated and assumed lens paths, blank-image rejection and bounded malformed
input. Generated maps and images stay outside Git and are explicitly simulated.
