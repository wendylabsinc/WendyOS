# README header

The animated header uses seconds 16–24 of the existing [G1 warehouse demo](https://wendy.dev/videos/wendy-g1-warehouse-turbopuffer-2026-09-29.mp4).
It shows the MuJoCo simulation, not a physical robot. The label stays visible throughout.
The wordmark comes from `screencast/support/wendy-logo-cream.svg` without recoloring or distortion.

To regenerate, install FFmpeg and librsvg (`rsvg-convert`), download the source video, then run:

```sh
bash .github/assets/readme/render.sh /path/to/wendy-g1-warehouse-turbopuffer-2026-09-29.mp4
```

`header.svg` supplies the static layout. `simulation-label.svg` labels the footage.
The script generates an eight-second, 960 × 400 GIF and a matching PNG poster.
The README selects the poster when the browser prefers reduced motion. Both link to the full video.
