# Native thermal campaign

Wendy Data reads the maximum measured Celsius temperature and records an episode
when it stays above the configured threshold for the dwell time. It records the
triggering temperature, source, sensor coordinates, measurement identity and
conversion provenance in `events.jsonl`. It does not record a continuous temperature
grid. The camera video uses the existing capture adapter and pre-roll ring.

Select the camera using `wendy --json data sources --device <device>`. Replace both
`/dev/video0` selectors in `campaign.yaml` with the camera's stable selector where
available. A selector must resolve to exactly one local raw camera. Generic UVC
brightness and raw counts without a verified Celsius conversion are refused.
Supported measurements are the original TC001 temperature plane and HIK TOPDON
256x392 / 96x200 camera-reported temperature summaries. HIK summary temperatures
are not per-pixel Celsius calibration.

For an ESP32/MLX90642 served by the existing USB thermal viewer, replace
`thermal.camera` with `thermal.mlx90642_url: http://127.0.0.1:PORT/PATH`, using its
actual frame JSON endpoint. This reads the viewer's 32x24 Celsius frames, not the
ESP32's rendered video. Keep the desired video camera in `sources`, or select
`telemetry: true` to record device health and the trigger event. The viewer must
already be running. Only literal loopback HTTP URLs are accepted; redirects,
proxies, responses over 64KiB, invalid dimensions and stale frames are refused.

```sh
wendy data campaign deploy campaign.yaml --device <device> --skip-cloud-registration
wendy data campaign inspect topdon-overheating --device <device>
wendy data campaign disarm topdon-overheating --device <device>
```

`thermal_status` reports waiting, running or an error. A new connection waits for
measurement advancement; repeated readings cannot satisfy a dwell indefinitely.
Missing data or a gap over three seconds breaks the dwell but does not clear an
already raised alarm. Only a fresh reading below `clear_below_celsius` rearms it.
The latch is in memory: restarting the agent, disarming/rearming or changing the
campaign revision begins a new observation session.

Each campaign has one sensor and its own latch. Use separate campaigns for TOPDON
and ESP32. Existing campaigns without `thermal` retain their behavior and revision.
Thermal campaigns cannot also configure model inference or `notify`; ThermalNotify
continues to handle notifications independently. No Cloud grant is requested by
this example; upload is manual.

Pre-roll is requested, not guaranteed: startup and camera availability can shorten
it. Inspect the finalized manifest and checksums to verify actual capture. A
latched trigger that encounters a recording error is reported in `thermal_status`;
check storage and camera health before rearming. Do not use an artificially low
test threshold as the production overheating threshold.
