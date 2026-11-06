# `wendy registry key`

Shows or rotates this machine's registry encryption key. Encrypted releases
are wrapped to it, so they open on this machine only.

```sh
wendy registry key show
wendy registry key rotate
```

Each machine (and each browser) you sign in on holds its own key. `wendy auth
login` creates it and registers it with Wendy Cloud, signed with your operator
key; a certificate refresh retries a registration that did not go through.

If registration fails, `wendy auth login` and a certificate refresh (such as
`wendy auth refresh-certs`, or the automatic renewal before a command) print a
warning and leave the key unbound. It is registered at the next sign-in or
certificate refresh, or when you run `wendy registry key show`.

`show` asks Wendy Cloud whether it still holds the key and registers it again
if not. It prints `registered`, `re-registered`, or `unregistered` with the
reason Cloud refused it.

| Command | Description |
|---------|-------------|
| `show` | Prints the key id and registers the key if Cloud does not hold it |
| `rotate` | Replaces the key with a new one and registers it |

| Flag | Description |
|------|-------------|
| `--cloud-grpc` | Cloud gRPC endpoint of the session to use |

After a rotation, releases wrapped to the old key must be wrapped again to the
new one by another of your signed-in devices.
