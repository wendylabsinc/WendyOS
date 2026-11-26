# Small Apps & Deployments HTTP fixture

This is a separate, genuinely small workload for functional tests while the
large Python fixture remains the active-upload timeout regression. Neither the
original Python source/layouts nor its run evidence is replaced.

The static Linux ARM64 Go executable runs in `scratch`. Its baked version is
linked into the binary and cannot be changed by deployment environment. The
HTTP response has the same `app`, `attempt`, `version`, `unit` and `fileOnly`
fields as the Python workload. `E2E_HTTP_PORT` sets the listener for every version.
Version `3.0.0` intentionally exits before serving. A nonempty
`E2E_FAIL_ON_BOOT_ID` matching the read-only Linux boot ID intentionally fails
only that host; another ID leaves the same image running.

From the repository root, using the installed Go toolchain and local Docker
Engine/Buildx:

```sh
python3 .github/ci-tests/apps-deployments-small/build.py \
  --output-root /absolute/new/fixture-directory
```

The output directory must not exist. The script cross-compiles with CGO off,
strips symbols, and builds `A` (`1.0.0`), `B` (`2.0.0`) and `fail` (`3.0.0`)
layouts. The same build exports OCI and loads the local engine without a push,
base image pull or network build step. Go dependencies are standard library only.

It verifies ELF ARM64 headers, OCI content hashes and sizes, image architecture,
loaded/exported filesystem equivalence, baked versions and real HTTP responses
from local containers for A and B on the same port. It verifies both intentional
failure modes and a nonmatching-host response. An env version override cannot
alter the served version. Owned verification containers are removed in `finally`;
versioned image tags and layouts remain available for reproducibility. Results,
digests, layer sizes, actual responses and compiler version are recorded in
`local-verification.json`.

Point a separate authorized official-harness run at the new `A`, `B` and `fail`
paths through `WENDY_E2E_APPS_LAYOUT_A`, `_B` and `_FAIL`. Keep the old large-image
inputs and results unchanged. No test assertion is relaxed for the small fixture.
In particular, a successful terminal receipt is not a workload response, and
the publication metadata case still requires the full ListArtifacts result.
