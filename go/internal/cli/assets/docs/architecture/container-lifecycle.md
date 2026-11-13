# Container lifecycle identity

A start request can name an app, while its task is stored under a service
container ID such as `demo_relay`. Resolve the request through
`ResolveAppContainerIDs` before changing restart bookkeeping. Do not derive
IDs by appending or stripping a suffix: legacy single-container apps keep
bare IDs, and an exact container name wins over an app alias.

After a successful start (streamed, attached, or grouped):

- Clear the persisted stopped-by-user label on each actual container ID.
- Clear its explicit-stop marker and register its persisted or requested
  restart policy under that same ID.
- Retire the request-name monitor entry when it is only an alias.
- If resolution fails, log the failure and skip bookkeeping rather than
  introducing an unresolvable monitor entry.

The stop path resolves the same IDs before marking explicit stop, and boot
reconciliation registers persisted container IDs. Keeping those identities
consistent prevents an old app alias from remaining eligible for restart
while the CLI reports its service stopped. The `NO` restart policy must
remove the resolved registration as well as any request alias. Already queued
restart work must also refuse to start after its registration is removed.

Regression tests in `container_service_test.go` exercise start/stop using
both name forms, attach, persisted stop labels, alias retirement, failed
resolution, and disabling restarts. Existing monitor tests cover the rule
that explicit stop wins over automatic restart scheduling.
