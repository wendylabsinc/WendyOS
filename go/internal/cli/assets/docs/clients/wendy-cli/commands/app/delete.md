# `wendy app delete`

Deletes an app from Wendy Cloud: it removes the app from every device that
runs it and deletes all its releases and deployments. This cannot be undone.

```sh
wendy app delete <app>
wendy app delete <app> --detach <device> [--detach ...] --yes
```

- It asks you to confirm, then shows each deployment of the app with its
  devices and the releases that will be deleted, and asks you to type the
  app name.
- **Remove is the default.** For each device you can choose to detach it
  instead: the app keeps running there as a direct run, as if started with
  `wendy run`. Name a device with `--detach`, or answer yes when asked.
- **A device you can't reach can only be detached.** It is detached only if
  you confirm it or name it with `--detach`. Without a terminal, every such
  device must be named with `--detach`, or nothing is sent.
- A detached device keeps the app, its image and its configuration, which
  may hold secrets. The app keeps running and restarts with the device. It
  stays until it is removed there or the device is reflashed.
- A device that is offline removes the app when it next comes online.
- Once the delete has started it cannot be cancelled. If it is interrupted,
  run `wendy app delete <app>` again: it sends what is still missing, without
  asking to confirm again. Any operator who may delete the app can finish it.

| Flag | Meaning |
|------|---------|
| `--detach` | Detach this device instead of removing the app from it: its name or id (repeatable) |
| `--yes` | Delete without asking for confirmation |
| `--cloud-grpc` | Cloud gRPC endpoint of the session to use |
