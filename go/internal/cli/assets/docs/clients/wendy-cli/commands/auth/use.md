# `wendy auth use`

Switches the current **auth context** — the session cloud and device commands use when several are stored and no `--cloud-grpc` flag is given.

> **Note:** The common auth flow — [`login`](../cloud/login.md),
> [`logout`](../cloud/logout.md), and [`status`](../cloud/status.md) — is
> surfaced under `wendy cloud`. Advanced context management (`use`, `rename`,
> `default`, `refresh-certs`) remains under `wendy auth`, which is hidden from
> the top-level help but fully functional.

## Contexts

Every login is stored as a named context. The first login is always named
`default` (you are never prompted to name it), so single-org users never deal
with names at all. Additional logins get a derived name (the realm, or `org-<id>`);
rename any of them with [`wendy auth rename`](./rename.md). Each context owns its
own operator certificate; switching context switches the identity every command
uses.

## Usage

```sh
wendy auth use [context]
```

## Description

With a **context** argument, `wendy auth use` makes that context current and
persists the choice in `~/.wendy/config.json`. With no argument in an
interactive terminal, a picker is shown instead.

The argument is primarily a **context name** (see [`wendy auth status`](../cloud/status.md)).
For backward compatibility it also accepts the legacy selectors:
- A plain integer — matched against the organization ID of a **legacy session's**
  certificate. A session whose certificate carries a tenant UUID (any OIDC login)
  is never matched by integer; select it by name.
- Any other string — a case-insensitive substring of the gRPC endpoint or
  dashboard URL, or an exact match against a session's tenant UUID.

An ambiguous selector (multiple matches) lists the candidates and errors. A
selector that matches nothing also errors. Contexts without certificate material
are rejected; re-run `wendy auth login` to refresh them.

## Flags

_None._

## Interactive picker keys

When the picker is shown (e.g. `wendy auth use` with no argument, or any cloud
command that reaches the multi-context branch):

| Key | Action |
|-----|--------|
| `↑` / `↓` | Navigate contexts |
| `enter` | Use this context for the current invocation only |
| `d` | Switch to the highlighted context (equivalent to `wendy auth use`); on the current context, clear it |
| `q` / `Ctrl+C` | Cancel |

The current context is marked with `✦`.

## Examples

```sh
# Switch by context name
wendy auth use acme

# The default context
wendy auth use default

# Legacy: switch by org ID or endpoint substring
wendy auth use 7
wendy auth use prod.example.com

# Interactive picker (TTY only)
wendy auth use
```

## See also

- In the Cloud tab of `wendy discover` or an interactive device picker, press
  `o` to switch organizations, including organizations without local
  credentials.
- [`wendy auth rename`](./rename.md) — rename a context
- [`wendy auth default`](./default.md) — show or clear the current context
- [`wendy auth login`](./login.md)
