# Thermal alarm without an app

Deploy [campaign.yaml](campaign.yaml) to watch a TOPDON TC001 thermal camera
with no application: 40 to 50 °C shows as a yellow area on the video, 50 °C and
above as a red one, and a red area that lasts 2 seconds records the moment and
sends a notification.

```sh
wendy data streams --device <device>          # find the camera's grid: agent/camera/grid/v4l2:/dev/videoN
wendy data campaign deploy campaign.yaml --device <device>
wendy data campaign inspect thermal-watch --device <device>
```

Change `/dev/video2` in both places to the camera's node first. If the camera
reads too hot, as some TC001 units do, calibrate it before relying on the
thresholds (`wendy device camera thermal-calibration`).

What happens:

- The agent reads the camera's temperature grid on every frame, groups cells of
  one band that touch into areas, and ignores areas of fewer than 4 cells.
- Every frame's areas are boxes on
  `agent/campaign/thermal-watch/rules/hotspot-alarm/markers/v4l2:/dev/video2`,
  yellow or red, labelled with the area's highest temperature:

  ```sh
  wendy data stream --device <device> --prefix agent/campaign/thermal-watch/rules/
  ```

- `thermal_warning` is raised after 3 seconds of a yellow area and
  `thermal_alarm` after 2 seconds of a red one. The alarm stays raised until
  everything is below 48 °C, and is raised at most every 2 minutes.
- Both are events of `sh.wendy.campaign.thermal-watch`, kept in the device's
  event journal. `thermal_alarm` starts a recording of 10 seconds before it and
  30 seconds after it, with the event in the episode's `events.jsonl`, and a
  notification such as "thermal_alarm on /dev/video2: 63.1 °C area". Enable the
  campaign's notification grant in Cloud Apps for it to deliver.
- While the campaign is deployed, the agent decodes the camera even when nobody
  is watching, so the camera stays on.

`rules_status` in `campaign inspect` says whether the rule gets frames, how many
events it raised, and its last one. Nothing in the rule is about temperature:
the same steps read any camera grid in its own unit, such as a SensorLink
camera's brightness in %. See the
[rules reference](../../go/internal/cli/assets/docs/clients/wendy-cli/commands/data.md#rules).
