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

Application records honor the requested pre-trigger buffer. Camera and ROS 2
adapters currently begin at the trigger and record their achieved source offset
in the manifest; deployment prints this limitation when it applies.
