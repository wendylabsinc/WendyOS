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

## Related

- `wendy device unpin <host>` — clear a device identity pin keyed by the old name
