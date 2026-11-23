# `wendy device keys`

Manages the keys a **Closed** device trusts (see [`wendy device access`](access.md)). The key that closed the device, or that `wendy install --close` seeded, is its first owner. Every command needs an owner key.

```sh
wendy device keys list
wendy device keys add <public-key-file|-> --role <owner|operator|viewer> [--label <name>]
wendy device keys remove <fingerprint>
```

## Roles

| Role | May |
| --- | --- |
| `owner` | everything, plus managing keys and the access shape |
| `operator` | deploy and run apps |
| `viewer` | read only |

Each key has exactly one role; to change it, remove the key and add it again. The last owner key cannot be removed.

## Adding another computer

1. On the other computer: `wendy auth local-key show > alice.pub`.
2. On an owner's computer: `wendy device keys add alice.pub --role operator --label alice`.

`add` reads the first PEM `PUBLIC KEY` block in the file, so the whole output of `wendy auth local-key show` works as is; `-` reads stdin. A private key is refused. `list` marks this computer's own key.

## How the CLI connects to a Closed device

The CLI tries your Wendy Cloud certificates first, then this computer's local key over TLS on the device's port + 1. The device key is pinned on first contact under the name you connect with, like an SSH host key; a different key later is refused (after a deliberate reflash run `wendy device unpin <name>`).

- Your key is not on the device: if its owner turned keyless apps on, the CLI uses the keyless set; otherwise it prints your key's fingerprint and asks you to have an owner run `wendy device keys add`.
- Image pushes go through a loopback proxy in the CLI that holds your key, so the key never reaches BuildKit or another build tool. The device registry needs an `operator` or `owner` key.
