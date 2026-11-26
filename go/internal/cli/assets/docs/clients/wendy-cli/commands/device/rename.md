Renames a device in two places: its asset name in Wendy Cloud, and its hostname
(and mDNS `.local` name) on the device itself. A single name is applied to both.

```sh
wendy device rename [name]
```

Without a `[name]` argument, an interactive prompt asks for one, prepopulated
with `wendyos-` as a starting point. The hostname is set **literally** — no
`wendyos-` prefix is added for you.

## Name rules

The name must be a valid DNS label:

- starts with a lowercase letter
- contains only lowercase letters, digits, and hyphens
- does not end with a hyphen
- is at most 63 characters

Device names are unique within a Wendy Cloud organization, compared without
regard to case. Wendy Cloud is renamed first, so a name it refuses leaves the
device's hostname unchanged. If Cloud accepts the name but the hostname cannot
be set, run the same command again to finish.

## When the Wendy Net name changes

On WendyOS agents with short device DNS support, a successful hostname change
requests a Cloud roster refresh. After the agent receives the updated name, it
publishes that name in the signed Wendy Net directory and the LAN mDNS
`cloud-name` TXT record. The mDNS `name` record follows the local rename. Other devices pick up the new `<name>.wendy.internal` alias as
their directories or rosters refresh. The old alias can still resolve while a
peer has an old binding or its app caches the previous DNS answer. Use
`device-<uuid>.wendy.internal` for app configuration that needs to survive a
rename.

To verify DNS and check the new name, run the
[short DNS checks](/docs/guides/wendy-net-services#verify-short-dns-from-the-client-app)
from an enrolled app container.

## Related

- `wendy device unpin <host>` — clear a device identity pin keyed by the old name
