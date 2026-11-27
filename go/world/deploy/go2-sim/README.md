# Private Go2 mission app

`world-go2-lab release` prepares a reusable motion image, and `commission` creates a separate run bundle and installer carrier. The original `stage` command remains available for historical single-run packages. All use the existing managed Go2 runtime at `http://127.0.0.1:8890`. These commands do not connect to a VM, change its project, arm a controller or send a task.

The app runs a local World gateway, a private Go2 bridge and the Go2 executor. Actual `/odom`, `/scan` and TF observations constrain navigation. The imported scene supplies the commissioned flat-floor profile and identity pins, not invented sensor clearance. Supported geometry, sensing, tilt, stopping horizon and command ownership limits remain in the Go2 bridge and executor contracts. This package does not authorize physical hardware, supervised movement, teleoperation, terrain traversal or arbitrary MJCF behavior.

## Reusable release and separate run configuration

This path implements the code/configuration split. Local package and filesystem tests pass; the [VM results](../../docs/go2-tilted-reusable-results.md) record reuse of release B's exact motion OCI across two imported configurations and a separate corrected release C trial. Navigation remains incomplete. Matching source hashes, local build inputs or image tags alone does not establish the same OCI artifact.

Prepare the release once:

```sh
go run ./cmd/world-go2-lab release \
  --repo . --out .local/go2-release --arch arm64
```

The allowlisted `project/` contains `worldd`, `world-go2-executor`, the fixed offline `world-go2-lab validate-run` program, boot/installer code, bridge/native-helper sources and ROS dependencies. It contains no run ID, task ID, grants, bridge token or commissioning files. `release.json` hashes the code inventory. The image build also records the compiled sensor helper in `native-digest.json`. The external `manifest.json` binds the complete build context. The release ID is the digest of `release.json`, not an OCI digest.

Create the image through an explicitly selected existing Wendy target. The supported source route is create-only `wendy run --deploy --chunking=off`, which builds and pushes into that agent's registry without starting the release container. `wendy build` alone does not push. Record the registry's exact **platform manifest digest**, then commission with the corresponding fully qualified image reference. No registry, device or default target is discovered by the staging program.

```sh
go run ./cmd/world-go2-lab commission \
  --release .local/go2-release \
  --image localhost:5000/com.wendy.world.go2.release@sha256:REPLACE_WITH_OBSERVED_DIGEST \
  --out .local/go2-run-one \
  --binding .local/commissioned-binding.json \
  --alignment .local/commissioned-alignment.json \
  --profile .local/commissioned-profile.json \
  --site synthetic-corridor --task-id corridor-inspection-1 \
  --arch arm64 --port 18885 --bridge-port 18886 \
  --transport host-http --allow-insecure-simulation \
  --commission-synthetic-flat-floor
```

The placeholder image is intentionally refused. Supply the observed digest. The program verifies the release inventory before creating any run credentials. It validates exact binding/alignment/profile bytes through the shared controller configuration decoder, including whichever reviewed profile versions that decoder admits. It creates the original 59-minute simulation grants without retaining their issuer private key.

The commissioned output separates three roles:

- `carrier/` is a private installer image built `FROM` the selected release digest. It adds only six bounded run files and their manifest. The fixed installer comes from the release image, never from a bundle-provided executable path. Its Wendy app has `network:none` and one new named volume, with no ROS, HTTP or admin capability.
- `mission/compose.yaml` references the exact selected image with **no build directive**. The companion Wendy configuration fixes app `com.wendy.world.go2`, service `mission`, the HTTP port and ROS domain 0. Each commission uses a fresh volume and run-scoped gateway/executor identities; the assignee remains `sim-go2/navigation`. Runtime environment contains only public digests. A later scene changes configuration, not this motion image.
- `clients/` contains operator credentials and public trust outside both deployment contexts. Neither the installer carrier nor motion image receives operator signing authority.

The carrier contains short-lived lab device/executor secrets in private OCI layers. This is a recorded lab custody limitation, not a production secret store. Keep the images, registry, cache and manifests private. The motion image has no run secrets.

Deployment is an explicit two-step procedure. First run the carrier with restart disabled, inspect its `configuration_installed` event and exit code 0, and confirm that it is stopped. Only then deploy `mission/` to the same explicitly selected VM. Compose dependency order is not a completion barrier. Never retry an unsuccessful installer into its old volume, delete a launch marker, or treat a listener as proof of a completed install. Keep its partial or terminal state for diagnosis and explicitly commission a new run if needed.

### Verified single-run installer workflow

The ordinary MCP `run` path with `start: true` did not disable restarts for cloud trial P. Its carrier logged `configuration_installed`, then restarted into the occupied volume, logged `installation_refused` and entered a crash loop with exit code 1. P was stopped and retired before any mission app or task send. The first log entry alone was insufficient to admit motion.

Trial Q used a fresh conversion, credentials, bundle and volume at the same observed world epoch. Its installer completed through this sequence:

1. Build and create the commissioned `carrier/` with MCP `run`, the explicit VM selector and `start: false`. Confirm the resulting carrier is not started. This publishes the image without running the installer against the volume.
2. Read the exact carrier platform manifest digest from the selected agent's registry. Create a separate wrapper directory with the carrier's generated `wendy.json`, preserving its app identity, `network:none` entitlement and exact fresh named-volume entitlement. Do not copy its bundle or credentials into this wrapper.
3. Add the following Compose file, replacing the image placeholder with that observed carrier reference. The wrapper has no build directive. Run the wrapper once on the same VM.

```yaml
name: world-go2-install
services:
  i:
    image: localhost:5000/CARRIER_REPOSITORY@sha256:OBSERVED_PLATFORM_DIGEST
    network_mode: none
    restart: "no"
```

The short service name matters. Wendy constructs the container ID as `APP_ID_SERVICE`. Q's first wrapper used `install` and exceeded the observed 76-character container ID limit. Creation was rejected before an installer existed; container readback confirmed that outcome. With service `i`, the wrapper created successfully, logged `configuration_installed` and stopped with exit code 0. A generic deployment failure does not establish that nothing ran. Inspect the actual container state before deciding whether any correction can reuse an untouched volume.

Retain the create-only receipt, registry digest, exact wrapper, installation event and stopped/exit-code readback. Only their combined successful outcome permits the separate mission deployment. This workaround changes private deployment inputs; it does not change the public CLI or relax the installer's refusal of previously used volumes. Q's installation result is separate from its subsequent navigation result.

The registry reference must be reachable both by the selected agent and by the carrier's build process. A guest-local `localhost:5000` is not automatically the host's localhost; verify the existing forward or choose a supported build host first. Public source `compose.go` passes a no-build digest reference to `CreateContainer`. The agent looks in its local image store and can pull missing content from its local registry. The installed CLI reports `2026.11.04-081952`; help/source capability is not proof of the deployed agent's behavior. Preserve actual requested/resolved manifest evidence for both motion deployments. This package does not implement a new image-import API or silently change registry references.

### Installation and restart fences

The installer reads the entire bounded bundle, verifies every digest, then validates exact node topology, catalog, source/grant scope, key consistency and signatures in the fixed Go validator. Current original validity is required. It writes only to an empty private volume, using exclusive files and a retained installation lock; files and parent directories are synced, and `installed.json` is written last. A partial, occupied, symlinked or mismatched installation is refused rather than repaired.

The final commit acknowledgment can be ambiguous. If writing the complete final marker succeeds but its sync or process acknowledgment fails, a later reader can see a valid-looking installation. All bundle files were synced before that marker, but the marker alone does not prove exit code 0 or a received completion event. Do not start motion after an unknown or failed installer outcome. Retain that volume and its evidence; do not reinterpret a later hash check as the missing successful installation acknowledgment.

Reusable boot verifies the image's code/helper inventory, selected release/image/bundle digests and Wendy app/service configuration. Environment selectors are configuration checks, not enrolled identity or self-attestation of the OCI image. It captures the exact verified run bytes once and writes a fresh private startup snapshot outside `/data`. The gateway, bridge and executor receive only snapshot paths. They never reload commissioning bytes from the shared volume. The snapshot is removed at shutdown. Named volumes remain writable; this is a byte-verification and process isolation boundary, not a kernel read-only mount or protection against a compromised root process.

Persistent task state is fenced to all three selected identities: release inventory, image digest and bundle digest. The existing durable one-shot launch marker is consumed before motion workers exist. After the gateway is ready, the validator checks the original current grant/revocation window again using only the snapshot. Refusal consumes that launch attempt and leaves the gateway in reporting-only mode where its own authority permits. Cancellation after that blocking validation or bridge readiness cannot start the next worker. Later boots never re-arm a consumed run. Reporting and historical validation do not renew grants or bypass the gateway's current checks.

All deployment steps still require a new signed task after a confirmed executor generation. Image reuse does not complete the route, tilt calibration, partition, stop, crash/reset or signed-evidence acceptance gates.

## Legacy single-run package

Supply the explicit commissioned `tasking.Go2Binding`, alignment and profile files. Their exact file bytes must match the binding's SHA-256 fields. The binding includes the selected VM, runtime source, project revision and epoch, world package/frame/run, conversion losses and controller configuration. An unsigned status observation is not commissioning authority.

```sh
go run ./cmd/world-go2-lab stage \
  --repo . \
  --out .local/go2-mission-new \
  --binding .local/commissioned-binding.json \
  --alignment .local/commissioned-alignment.json \
  --profile .local/commissioned-profile.json \
  --site synthetic-corridor \
  --task-id corridor-inspection-1 \
  --arch arm64 \
  --port 18885 --bridge-port 18886 \
  --transport host-http --allow-insecure-simulation \
  --commission-synthetic-flat-floor
```

The output directory must not exist. The run ID comes from the supplied binding and must contain at most 32 lowercase letters, digits or hyphens. The staging command appends a random suffix to the app ID, audience and persisted volume name, isolating separate stages even when their binding run IDs match. Site and task ID are explicit. Ports must be distinct, unprivileged and different from the existing runtime's 8890 port. The pinned ROS base currently supports arm64; `--arch amd64` is refused until a separately reviewed base is available.

Staging first compiles Linux `worldd` and `world-go2-executor`, each with a five-minute build limit. It then creates a simulation-only Ed25519 issuer, 59-minute grants and a signed revocation snapshot. The root private key is never written. Compilation failure removes only the newly created output directory. Grants do not renew automatically, including on app restart.

The output is private:

- `project/` is the allowlisted build context. It contains the two binaries, the bridge's reviewed Python and rclcpp sources, commissioned configuration, a random 256-bit bridge bearer token, and only the gateway and executor keys.
- `clients/operator.json` holds the `mission`, `stop` and `read` operator grant and private key. It stays outside the build context. `clients/trust.json` contains the public root and signed revocations.
- `manifest.json` contains scope, expiry and file digests. It contains no private key or token. Keep the whole output private nonetheless.

The gateway key has only `task-status`, `replicate` and `read`, bound to the selected assignee and audience. The executor key has only `execute` and `read`, with its assignee fixed. The gateway's only task catalog entry is `wendy.go2.nav.go_to.v1`, class MOVE. Its binding must match exactly; supervision is disabled, lease maximum is 500 ms and telemetry age maximum is 250 ms. There are no configured peers in this single-gateway package. The local task relay retains signed task/result history.

The generated Dockerfile uses the dependency stage from `deploy/go2-sim/Dockerfile` and builds the fixed `world-go2-sensors` rclcpp helper. It never copies the repository wholesale. Its Wendy configuration requests host networking, the declared gateway HTTP port, one uniquely named persisted volume and ROS 2 Humble peripherals on domain 0. It requests no admin, physical camera or GPU entitlement. Host HTTP is an explicit simulation transport, not authenticated mesh transport. Requests and mission proofs still require the generated grants and signatures.

## Boot and task delivery

The entrypoint sources ROS before enabling shell nounset, then restores the reviewed loopback CycloneDDS configuration and domain 0. Boot validates the fixed staged file inventory and hashes. It creates `/data/world-go2`, syncs the parent directory, locks the state directory and persists a configuration hash. A changed configuration, ambiguous existing state, invalid permissions or failed sync refuses startup. It changes ownership only for its own state directory, lock and configuration fence, and grants traversal to its unique mounted parent. It does not recursively chown existing records. All children run after supplementary groups are cleared and UID/GID drop to 65532. Executables remain in `/opt/world`, outside the potentially `noexec` persisted volume.

The gateway starts first. After its bounded local readiness check, boot creates and syncs `launch-once.json` before starting either bridge or executor. The bridge gets a numeric loopback listener. Only after that listener opens does the executor start. TCP readiness is process/listener evidence; it does not verify sensor quality, a successful executor generation or motion permission.

Wait for the executor's `executor_ready` event with the expected run ID, selected task ID and nonzero `executor_epoch` before delivering the signed task. This event follows successful executor startup and the gateway's durable `Begin` call. Do not deliver the mission before that generation is established. The executor polls its explicit task ID; boot does not call `Begin`, sign a task or wait for a pre-existing mission. A new executor generation invalidates interrupted task execution under the existing tasking contract.

On every later app boot using the same persisted volume, the gateway alone starts so retained task state can be inspected. The launch marker prevents bridge and executor re-arming even if the first launch failed, never received a task or exited successfully. Do not remove the marker to retry. Recommission with an explicit new run/stage and unique volume, inspect the old result and sign a new task.

If the bridge or executor exits, boot terminates the other motion worker and retains the gateway for reporting. Gateway death terminates both motion workers. Shutdown sends TERM, allows a bounded cleanup interval and attempts KILL on the entire child process group, including its sensor helper. This is process cleanup and attempted zero publication, not proof of physical stopping. The existing runtime watchdog, slew behavior and measured dynamics remain separate. If a worker stays alive with a latched fault, it may continue reporting and issuing zero commands; boot never creates a replacement worker.

Boot explicitly enables `--fault-snapshot /data/world-go2/first-fault.json --fault-snapshot-log` on the executor. Its first latched fault attempts one bounded, exclusive 0600 snapshot file and one bounded JSON diagnostic event. The snapshot preserves the consumed synthetic ROS scan and controller state for diagnosis. It is not signed World evidence, independent sensor truth or a physical stopping measurement. File creation failure or a pre-existing file does not authorize a retry, rewrite, new executor generation or re-arm. The original launch marker still controls every later boot.

After grant/revocation expiry, authenticated reads and startup validation can refuse. Reporting-only restart does not extend trust, bypass expiry or provide indefinite offline operation.

The staged verification app also enables `--observation-log`. After each controller tick it can emit the latest consumed pose, body velocity, paired capture timestamps, epoch age and supplied uncertainty. It emits at most 1,200 distinct capture records. These unsigned records contain no scan payload or credentials and do not renew a capture, create evidence authority or feed an independent oracle back into control. Logging happens after the tick; the receiver watchdog still applies if process output blocks.

## Local checks

```sh
go test -race ./cmd/world-go2-lab -count=1
go vet ./cmd/world-go2-lab
python3 -B cmd/world-go2-lab/boot_test.py
python3 -B cmd/world-go2-lab/reusable_boot_test.py
```

Tests use generated synthetic credentials, temporary directories and fake child processes. They check context isolation, grant scope, commissioning digest refusal, private permissions, symlink/FIFO refusal, build failure cleanup, exact file hashes, configuration/launch persistence, parent-directory sync failure, startup order, privilege-drop order and reporting-only restart. They do not exercise ROS, create a VM or claim that a deployed robot moved safely. Deployment and actual simulator acceptance require separate, explicitly recorded runs against the selected existing VM.
