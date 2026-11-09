# `wendy auth prepare-build-delivery`

Prepare an operator deployment credential for one target before disconnecting
from Cloud:

```sh
wendy auth prepare-build-delivery --device camera-one
```

The command obtains a PKI certificate containing your deployment permissions for
that exact device. Its key stays in your local credential store. The printed
admission deadline is at most 15 minutes away: create your remote build job before
then, while its target is reachable. Admission records the signed job durably on
the target. Later delivery and retries need neither Cloud nor the operator
connection, and can continue after the signing certificate expires, up to the
job's 24-hour delivery deadline.

This prepares authorization, not a job queue. The current streamed build still
cancels if its CLI connection closes.

Use `--skip-cloud-registration` when starting a build without Cloud. Ensure base
images and build dependencies are also available on the isolated network.

A normal `wendy run --build-host <builder>` prepares this credential automatically
when it needs one. This command refreshes it explicitly in preparation for an
airgap. `wendy auth logout` deletes cached delivery signing credentials; it does
not revoke jobs already admitted by a target.

See [build-host authorization](../device/build-host.md#operator-authorization)
for the builder key binding, publication policy and upgrade requirements.
