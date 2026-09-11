# Wendy Data campaign

A campaign is a durable, device-local flight-recorder plan. Deploying the file
validates it and arms its application event and model-uncertainty triggers:

```sh
wendy data campaign deploy campaign.yaml
wendy data campaign list
wendy data campaign inspect forklift-failures
```

To exercise the plan without waiting for an application trigger:

```sh
wendy data campaign trigger forklift-failures --reason commissioning
wendy data episodes
wendy data inspect <episode-id>
```

Camera selectors match a stable source ID, device path, or an unambiguous name
from `wendy data sources`. `front` and `default` select the only healthy camera
when a device has exactly one. ROS 2 topic entries select the device's healthy
ROS graph recorders; requested topics are retained separately in the Episode
manifest.

Application records and camera streams honor the requested pre-trigger buffer:
a buffered camera arms a standby subscription and keeps a keyframe-aligned ring
of encoded frames, so the clip reaches back before the trigger. Audio and ROS 2
adapters still begin at the trigger. Every source records the offset it
actually achieved in the manifest, which is shorter than the request whenever a
ring's byte cap was reached first, and deployment warns when a campaign asks for a
buffer on a source that cannot honor it.
