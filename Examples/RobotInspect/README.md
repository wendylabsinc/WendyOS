# RobotInspect

Runs a read-only robot inspection **on the robot** and prints what it found: what the
robot declares about itself beside what it was observed doing, with the disagreements
named. It commands nothing — only probes that cannot actuate are ever planned.

## Why this runs on the device

DDS discovery is multicast. A participant on a laptop cannot see a robot's ROS 2 graph
across a routed link or a cloud tunnel, however reachable the device itself is. So the
probe joins the graph from inside the robot's own network, which is what the
`network: host` entitlement is for.

The same probes run from the CLI when you are on the robot's LAN:

```sh
wendy device robot inspect --device <robot> --settle 10s
```

## Deploy

```sh
./build.sh arm64          # or amd64
wendy run --device <robot>
```

The image is `scratch` plus one static binary — about 2.6 MB — so it needs no ROS
installation on the device and leaves nothing behind that could reach an actuator.

## Reading the output

Every value carries its kind, its qualifiers and where it came from:

```
camera.color.resolution.width
  declared  640   (camerainfo: topic:/camera/color/camera_info)
  measured  848   (stream-sample: topic:/camera/color/image_raw, 25 samples over 5s)
  ! disagree: declared 640 vs measured 848
```

A field of view is always reported per axis and carries the resolution its intrinsics
were calibrated at, because a horizontal angle quoted against a vertical measurement is
the error this command was written to catch.

`--json` emits the same content as a `wendy.robot.inspection.v1` document, which is the
form to store and diff between two robots.

## Flags

| Flag | Meaning |
| -- | -- |
| `-domain` | ROS_DOMAIN_ID to join (default 0) |
| `-interface` | Network interface to bind discovery to |
| `-settle` | How long to let discovery run before reading (default 10s) |
| `-duration` | Sampling window for measured values (default 5s) |
| `-device`, `-kind` | Labels recorded in the document |
| `-json` | Emit the document instead of the report |
