# `wendy doctor`

Check your setup and get a local diagnostic report with suggested fixes.

```sh
wendy doctor
```

To print a sanitized JSON report that you can review and share:

```sh
wendy doctor --report
```

The report goes to standard output. Wendy does not upload it or create a report
file. To save it yourself:

```sh
wendy doctor --report > wendy-report.json
```

## Choose what to check

Doctor recognizes a project in the current directory, or you can select one:

```sh
wendy doctor --project ./my-app --report
```

It checks the target chosen by `--device`, then `WENDY_DEVICE`. To check a
particular target:

```sh
wendy doctor --device my-device.local
wendy doctor --device vm:my-running-vm --report
wendy doctor --device docker
```

Doctor does not scan for devices or start stopped VMs. Named VM checks read the
existing state and probe its recorded local endpoint. Use the actual VM name
printed by the tour. An unnamed `sim` or `simulator` target is reported as
unknown because selecting or creating a VM would change state.

## Checks and results

Doctor checks project configuration and build inputs, the Docker executable,
daemon and Buildx plugin, QEMU and ARM64 firmware, and the selected target's
connection. Project validation does not execute project commands or hooks.
Docker checks only run `docker info` and `docker buildx version`.

Docker is required for a recognized container project, the local Docker target,
or a VM/simulator target. QEMU and firmware are required for VM/simulator targets.
Without that context, these tools are optional. A recognized project or explicit
`--project` is required to pass static validation.

| Status | Meaning |
| --- | --- |
| `pass` | This check succeeded. |
| `fail` | This check found a problem. Read its suggested fix. |
| `unknown` | The check lacks enough context or evidence. |
| `not_required` | This check does not apply to the selected route. |

Each JSON check has an `id`, `status`, `required`, stable `code`, and a sanitized
`message`. Failed checks include a `remediation` when available. The report also
includes `schema_version`, CLI version, host OS and architecture, and `ok`.

The command exits with code 1 when a required check fails or the diagnostic
deadline expires. Optional missing tools and unknown context do not fail the
command. `ok: true` means no required check failed. It does not mean a device has
been installed, an app has run, or authentication has been verified.

The default overall deadline is 20 seconds. Individual subprocess and device
checks have a three-second limit. You can change the overall deadline up to one
minute:

```sh
wendy doctor --timeout 30s --report
```

## Authentication and privacy

Doctor can make a TCP connection and read-only agent version and provisioning
requests to the selected local endpoint. A reachable endpoint alone does not
verify device identity. An enrolled device may require credentials that doctor
does not use, so its agent or authentication result may remain unknown.

Cloud targets are not probed. Doctor does not use or refresh saved credentials.
Authentication is `not_required` for the local Docker target or when a responding
local agent explicitly reports that it is not enrolled. Doctor never infers that
saved authentication is valid.

Reports omit target names and addresses, local paths, project and app names,
project source, credentials, and subprocess output. Error messages and suggested
fixes come from fixed product text, not arbitrary logs or validation errors.

Doctor does not install or update software, modify configuration, deploy apps,
start VMs, or move robots. To install a missing QEMU or ARM64 firmware, run
`wendy run` or `wendy tour` in a terminal; they offer to install it. CLI
telemetry and routine update or configuration housekeeping are skipped for this
command.
