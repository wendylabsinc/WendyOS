# `wendy device access`

Shows and changes who may use a device that is **not enrolled in Wendy Cloud**. A device has one of three access shapes:

| Shape | Who gets in |
| --- | --- |
| Open | anyone on the device's network (the default, as before) |
| Closed | only callers proving a key the device trusts, each with one role (see [`wendy device keys`](keys.md)) |
| Enrolled | Wendy Cloud decides; the commands below are refused |

```sh
wendy device access                  # shape, keyless apps switch, your key's role
wendy device access close            # Open -> Closed; this computer's key becomes the owner
wendy device access open             # Closed -> Open; drops every key (owner only)
wendy device access keyless <on|off> # owner only
```

`wendy discover` marks a Closed device with `◐`.

## Closing a device

Anyone may close an Open device. This computer's local key ([`wendy auth local-key show`](../auth/local-key.md)) becomes its first owner, and the device restarts its agent. To install a device already Closed, use `wendy install --close`.

## Keyless apps

An owner may open one fixed feature set to callers without a key: deploying and managing apps (`wendy run`, the app commands, logs). Shell access, the post-start host command, keys and the shape always need a key. Off by default.

## Without the owner key

Run `wendy device access open` from an owner's computer. Without any owner key, reflash the device, or remove `/etc/wendy-agent/trusted-keys.json` on it as root; it comes back Open.

## Enrolling and unenrolling

Enrolling a Closed device needs the owner key. Unenrolling returns it to Closed with the same keys.
