# Wendy Agent — Threat Model

**Version:** 1.1  
**Date:** 2026-06-28  
**Scope:** `wendy-agent` daemon, `wendy` CLI, embedded OCI registry, OTEL receivers, BLE stack

---

## 1. Overview

Wendy Agent is a containerised edge-computing platform that runs on IoT/embedded Linux devices (Raspberry Pi, NVIDIA Jetson, etc.). It exposes gRPC APIs for remote container management, WiFi configuration, Bluetooth connectivity, hardware access, and OS updates. It integrates with Wendy Cloud for certificate-based device provisioning and tunnelled remote access.

This document identifies assets, trust boundaries, threat actors, and STRIDE-categorised threats, together with existing mitigations and recommended controls.

---

## Changelog

### v1.1 — 2026-06-28
Reconciled against merged code. The following controls moved from "recommended" to
"existing mitigations": D-Bus hard-fail on missing proxy (TM-E-01), `/run/containerd/*`
mount deny (TM-E-02), `CAP_NET_ADMIN` separated from host networking (TM-E-03), baseline
seccomp profile (TM-I-05), default CPU/memory/PID limits (TM-D-02), hook command-injection
fix, debugpy loopback gating, CLI cert/org pinning, and pre-provisioning surface reduction
(TM-S-01 / TM-I-03). Open HIGH/CRITICAL items remain explicitly marked.

---

## 2. Architecture Overview

```
┌──────────────────────────────────────────────────────────┐
│  Wendy Device (Edge)                                     │
│                                                          │
│  ┌─────────────┐    gRPC mTLS     ┌──────────────────┐  │
│  │  wendy CLI  │◄────────────────►│  wendy-agent     │  │
│  │  (user)     │                  │  (root daemon)   │  │
│  └─────────────┘                  │                  │  │
│                                   │  ┌─────────────┐ │  │
│  ┌─────────────┐    BLE L2CAP     │  │ containerd  │ │  │
│  │  Mobile App │◄────────────────►│  │ (containers)│ │  │
│  │  (user)     │   mTLS (post)    │  └─────────────┘ │  │
│  └─────────────┘                  │                  │  │
│                                   │  ┌─────────────┐ │  │
│                                   │  │ OCI Registry│ │  │
│                                   │  │ :5000       │ │  │
│                                   └──────────────────┘  │
│                                          │               │
│                          ┌───────────────┼──────────────┐│
│                          │D-Bus          │gRPC TLS      ││
│                   ┌──────▼──────┐ ┌─────▼────────────┐ ││
│                   │NetworkMgr / │ │ Wendy Cloud       │ ││
│                   │BlueZ        │ │ (pki-core, tunnel)│ ││
│                   └─────────────┘ └──────────────────┘ ││
└──────────────────────────────────────────────────────────┘
```

### Network Ports

| Port | Protocol | Auth | Active When |
|------|----------|------|-------------|
| 50051 | gRPC plaintext | None | Unprovisioned devices only; all services available (intentional design — provisioning is the mitigation; see TM-S-01) |
| 50052 | gRPC mTLS | Client cert | Post-provisioned |
| 4317 | OTEL gRPC | None | Always (loopback-bound) |
| 4318 | OTEL HTTP | None | Always (loopback-bound) |
| 5000 | OCI Registry HTTP/HTTPS | None / TLS | HTTP pre-prov, HTTPS post-prov |
| BLE L2CAP | BLE L2CAP mTLS | Client cert | Post-provisioned |
| `/run/wendy/agent.sock` | gRPC (unix socket) | None | Always; gated by `admin` entitlement |

---

## 3. Assets

| Asset | Confidentiality | Integrity | Availability |
|-------|----------------|-----------|--------------|
| Device private key (`device-key.pem`) | Critical | Critical | High |
| Provisioning state (`provisioning.json`) | High | Critical | High |
| Enrollment token (in-flight) | Critical | Critical | Medium |
| Container images | Medium | High | High |
| App volume data | Varies (app-defined) | High | High |
| WiFi credentials (NetworkManager) | High | Medium | Medium |
| Telemetry / observability data | Low | Medium | Medium |
| Agent binary | Low | Critical | High |
| Cloud CA certificate | Low | Critical | High |

---

## 4. Trust Boundaries

| Boundary | Description |
|----------|-------------|
| **TB-1** | Network perimeter — Internet / LAN to device ports |
| **TB-2** | Provisioning state — unprovisioned (no auth) vs. provisioned (mTLS) |
| **TB-3** | Container isolation — host root context vs. container `appuser` |
| **TB-4** | D-Bus proxy — container app vs. system D-Bus (xdg-dbus-proxy filtered) |
| **TB-5** | Volume scope — per-app named volume vs. shared-name volumes |
| **TB-6** | Cloud trust — device cert chain signed by Wendy CA |

---

## 5. Threat Actors

| Actor | Motivation | Capability |
|-------|-----------|-----------|
| **Remote unauthenticated attacker** | RCE, crypto-mining, pivot | Network access to device ports |
| **Malicious container app** | Privilege escalation, data theft, lateral movement | Runs inside container as `appuser` |
| **Compromised CLI operator** | Exfiltrate data, backdoor device | Valid mTLS client cert |
| **Physical attacker** | Key/credential theft, persistent firmware implant | Physical access to device |
| **Supply chain attacker** | Persistent backdoor at scale | Control over container registry or dependency |
| **Network attacker (MITM)** | Credential interception, session hijack | On-path between device and cloud/CLI |
| **Insider / rogue cloud operator** | Revoke certs, enumerate devices | Wendy Cloud admin access |

---

## 6. Threat Catalogue

Severity scale: **CRITICAL > HIGH > MEDIUM > LOW > INFO**

### 6.1 Spoofing

#### TM-S-01 — Unauthenticated access on pre-provisioned port
- **Severity:** CRITICAL (inherent — accepted by design with provisioning as the mitigation; see framing note)
- **Status:** 📋 Planned (additional hardening; WDY-1006 / WDY-1092)
- **Component:** Port 50051 (gRPC plaintext)
- **Description:** Unprovisioned devices use a plaintext channel by design — provisioning is the mitigation. Before provisioning completes, port 50051 accepts gRPC connections from any host on the network with no authentication. This is an intentional, documented, time-bounded state: the pre-provisioning window is warned to operators, and provisioning establishes mTLS for all subsequent communication. The `wendy tools install` flow offers provisioning during setup to minimise the window. Production deployments should use pre-enrollment at imaging time to skip the post-boot window entirely.
- **Existing mitigations:** Port is shut down immediately after provisioning completes. Pre-enrollment at imaging time eliminates the post-boot plaintext window. Post-provisioned devices receive no traffic on port 50051.
- **Recommended controls:**
  - Default to requiring pre-enrollment for production deployments (skip post-boot provisioning window).
  - Reduce services registered on the plaintext server to `ProvisioningService` only (WDY-1006).
  - If post-boot provisioning is used, bind port 50051 to localhost or a provisioning VLAN only, and enforce a short provisioning timeout with automatic shutdown.
  - Protect enrollment tokens with ECDH or TLS-bootstrapped channel in transit (WDY-1092).

#### TM-S-02 — Spoofed cloud endpoint during provisioning
- **Severity:** HIGH
- **Status:** 📋 Planned (WDY-1086)
- **Component:** Cloud gRPC dialer, pki-core
- **Description:** The cloud host is derived from CLI arguments or a config file. An attacker who can influence the config (e.g., via an evil captive portal during initial WiFi setup) could redirect the enrollment request to a rogue CA, causing the device to trust attacker-controlled certificates.
- **Existing mitigations:** TLS 1.2+ is enforced for cloud connections on standard ports. CLI pins the cloud host on the first connection to a device (WDY-1149).
- **Recommended controls:**
  - Pin or hard-code the expected cloud CA in the firmware image so it cannot be overridden by config (WDY-1086).
  - Validate the cloud host against an allowlist before dialling.

#### TM-S-03 — Impersonation via stolen mTLS client certificate
- **Severity:** HIGH
- **Status:** 📋 Planned (WDY-1087, WDY-1192)
- **Component:** CLI / gRPC mTLS (port 50052)
- **Description:** A stolen or leaked certificate grants authenticated access until the certificate expires (maximum 2 years). Without revocation, the window of exposure is bounded but not eliminated.
- **Existing mitigations:** Certificates are stored in `~/.wendy/` with user-level permissions. A maximum certificate lifetime of 2 years is enforced at the TLS layer. The 2-year ceiling is a deliberate accommodation for devices that operate without internet connectivity (air-gapped or intermittently connected) and therefore cannot rely on frequent online renewal; connected deployments can use shorter lifetimes. CLI pins device org and cloud host on the first connection to a device so a cert from another org is rejected (WDY-1149).
- **Recommended controls:**
  - Implement certificate revocation (OCSP or CRL served by Wendy Cloud) (WDY-1087).
  - For connected deployments, shorten certificate lifetime with automatic renewal to reduce the exposure window; retain the longer ceiling for offline devices that cannot renew online.
  - For offline devices, require a special offline-capable, short-lived *operator* certificate to interact with the device. The operator handshake carries an OCSP-stapled revocation list, so the device can enforce revocation of operator and peer certificates without needing its own online OCSP/CRL path. This bounds the exposure of the long-lived device cert without requiring device connectivity (WDY-1087, WDY-1192).
  - Embed operator-specific permissions (scopes/roles) into developer certificates, so the agent authorizes each RPC against the caller cert's claims rather than treating any valid cert as fully privileged. This limits the blast radius of a stolen or over-broad certificate and enables privileged-operation separation (e.g. the dedicated high-privilege role for `UpdateOS` in TM-T-03).

---

### 6.2 Tampering

#### TM-T-01 — Malicious container image execution
- **Severity:** HIGH
- **Status:** 🛠 In progress (WDY-1088, Phase 1)
- **Component:** Container service, OCI registry
- **Description:** Container images are sourced from a user-supplied registry. There is no image signature verification. A compromised image or a MITM between the registry and the device could result in execution of malicious code inside a container.
- **Existing mitigations:** Containers run as `appuser` (UID 1000) with namespace isolation and capability dropping. Baseline seccomp profile restricts dangerous syscalls (WDY-1012).
- **Recommended controls:**
  - Enforce OCI image signature verification (Cosign / Notary v2) before `RunContainer` (WDY-1088).
  - Pin images by digest (SHA256) in deployment manifests.

#### TM-T-02 — Agent binary tampering via `UpdateAgent`
- **Severity:** HIGH
- **Status:** 🛠 In progress (WDY-1089, Phase 1, depends on WDY-1001)
- **Component:** `UpdateAgent` RPC
- **Description:** `UpdateAgent` downloads a new binary from a URL specified in the RPC call and verifies only SHA256. An attacker with CLI access (valid mTLS cert) could supply a malicious binary URL with a matching hash if the hash itself is attacker-controlled.
- **Existing mitigations:** SHA256 hash verification, mTLS required for the RPC.
- **Recommended controls:**
  - Sign agent binaries; verify the signature against a pinned public key embedded in the current binary (WDY-1089, WDY-1001).
  - Require the update URL to match an allowlisted domain.
  - Enforce that only binaries issued by Wendy's build pipeline are accepted (e.g., sigstore transparency log).

#### TM-T-03 — OS artifact tampering (`UpdateOS`)
- **Severity:** HIGH
- **Status:** 🛠 In progress (WDY-1090, Phase 1)
- **Component:** `UpdateOS` RPC, `wendyos-update`
- **Description:** OS updates are delivered by the in-house `wendyos-update` tool, which has replaced Mender. The artifact source is supplied to the RPC; until artifact code-signing is enforced, a MITM or DNS attack against the download path could deliver a malicious OS image.
- **Existing mitigations:** OS delivery now runs through the in-house `wendyos-update` tool (deployed), replacing Mender and its optional-per-deployment signature model.
- **Recommended controls:**
  - Enforce HTTPS-only artifact downloads with strict TLS validation (WDY-1090).
  - Verify an OS artifact code signature against a pinned key before install. This is being delivered as part of a coordinated code-signing rollout, alongside `wendy-agent` binary signing (TM-T-02) and cosign image-signature verification for containers (TM-T-01).
  - Restrict `UpdateOS` to callers with a dedicated high-privilege role (separate from regular operator cert).

#### TM-T-04 — Volume data tampering between containers
- **Severity:** MEDIUM
- **Status:** 📋 Planned
- **Component:** Volume entitlement (`persist`)
- **Description:** Volumes are scoped by name, not by app identity. Two apps with the same `persist` name share data. A malicious or compromised app could corrupt or exfiltrate data belonging to another app sharing the same volume name.
- **Existing mitigations:** Volume access requires an explicit entitlement declaration.
- **Recommended controls:**
  - Namespace volumes by a unique app identifier (not just user-supplied name) by default.
  - Document the sharing model explicitly; require explicit opt-in for cross-app sharing.

### Software Bill of Materials & provenance

Releases include SPDX SBOMs for each shipped binary plus Swift and whole-repo
source SBOMs, and Sigstore-backed SLSA build provenance and SBOM attestations.
See `security/VERIFICATION.md` for verification steps.

---

### 6.3 Repudiation

#### TM-R-01 — Insufficient audit trail for privileged operations
- **Severity:** MEDIUM
- **Status:** 📋 Planned (WDY-1096, Phase 3)
- **Component:** Agent RPC handlers
- **Description:** There is no structured audit log for privileged operations (RunContainer, UpdateOS, UpdateAgent, WiFi credential changes). If an incident occurs, it may be impossible to determine which authenticated principal performed a given action.
- **Existing mitigations:** Logs are written to stdout/journald; debug logs via `WENDY_DEBUG`.
- **Recommended controls:**
  - Emit a structured, tamper-evident audit event for every state-changing RPC, including: timestamp, RPC name, caller certificate CN, parameters (sanitised), and outcome (WDY-1096).
  - Forward audit events to a remote log sink that the device cannot modify.

---

### 6.4 Information Disclosure

#### TM-I-01 — OTEL receivers accept unauthenticated data from any source
- **Severity:** MEDIUM
- **Status:** 📋 Planned
- **Component:** OTEL gRPC (4317), OTEL HTTP (4318)
- **Description:** OpenTelemetry receivers listen on loopback only (`127.0.0.1` and `[::1]`) with no authentication. Any local process can submit arbitrary logs, metrics, and traces, polluting the telemetry stream with false data or extracting device observability data.
- **Existing mitigations:** Receivers are bound to loopback interfaces only (both IPv4 and IPv6); remote network access is blocked.
- **Recommended controls:**
  - Require mTLS or a bearer token for OTEL submissions from local sources.

#### TM-I-02 — Private key at rest in `/etc/wendy-agent/`
- **Severity:** HIGH
- **Status:** 🛠 In progress (HSM/TPM-backed root of trust)
- **Component:** Provisioning state, key material
- **Description:** The device private key (`device-key.pem`) and full provisioning state (including cert PEM) are stored in `/etc/wendy-agent/` at mode 0o600. Root access to the device (e.g., via physical UART, compromised container escape, or OS vulnerability) directly exposes the key.
- **Existing mitigations:** Files are mode 0o600, root-owned. The config partition copy is deleted after first boot.
- **Recommended controls:**
  - Use a hardware security module (HSM) or Trusted Platform Module (TPM) to protect the private key; only the agent process can exercise the key.
  - If HSM/TPM is unavailable, store the key in the kernel keyring (non-exportable) rather than a plain file.

#### TM-I-03 — Enrollment token intercepted in transit
- **Severity:** HIGH
- **Status:** 📋 Planned (WDY-1092, Phase 1, depends on WDY-1006)
- **Component:** Provisioning flow, plaintext port 50051
- **Description:** The CLI sends the enrollment token to the device over the pre-provisioning plaintext gRPC channel. An on-path attacker on the same network segment can intercept the token and enroll a different device in the same org.
- **Existing mitigations:** The token is single-use (invalidated after first use on cloud side). Pre-enrollment at imaging time eliminates the in-transit token exposure window entirely.
- **Recommended controls:**
  - Confirm enrollment tokens are single-use and short-lived (< 15 minutes) at the cloud.
  - For post-boot provisioning, use a provisioning-time ECDH key exchange to protect the token in transit even over plaintext gRPC, or move provisioning to a TLS-bootstrapped channel (WDY-1092).

#### TM-I-04 — BLE command interception and replay
- **Severity:** MEDIUM
- **Status:** 📋 Planned (WDY-1098, Phase 3)
- **Component:** BLE L2CAP stack
- **Description:** Before provisioning, BLE commands may be accepted without mTLS. Post-provisioning, the L2CAP channel is mTLS-protected, but if the channel is not bound to a unique session nonce, replayed mTLS records from a passive BLE sniffer could replay commands.
- **Existing mitigations:** Post-provisioning mTLS provides authentication and encryption. Server certificate verification with org ID matching prevents MITM attacks from devices in other organizations. SPKI pinning (trust-on-first-use) provides additional protection: the first BLE connection pins the device's certificate fingerprint, and subsequent connections verify the fingerprint matches.
- **Recommended controls:**
  - Verify that TLS session resumption is disabled on BLE channels (disable session tickets / stateless resumption) (WDY-1098).
  - Confirm pre-provisioning BLE behaviour: document and restrict which commands are available before mTLS is established.

#### TM-I-05 — Container escape exposing host filesystem
- **Severity:** MEDIUM
- **Status:** ✅ Mitigated (WDY-1012)
- **Component:** Container runtime, capability set
- **Description:** Containers are granted a broad capability set when device entitlements are enabled (including `CAP_SYS_PTRACE`, `CAP_SYS_CHROOT`, `CAP_NET_ADMIN`). A container vulnerability combined with these capabilities could allow a container escape to the host.
- **Existing mitigations:** Containers run as non-root `appuser` (UID 1000). `CAP_SYS_ADMIN` is not granted. A baseline seccomp profile is applied to all containers, blocking dangerous syscalls including `ptrace`, `unshare`, and `clone` with `CLONE_NEWUSER` (WDY-1012). `CAP_NET_ADMIN` is no longer granted by host networking alone; it requires a separate explicit entitlement (WDY-1094). Note: full user-namespace UID remapping (container root → high host UID) is not yet implemented (WDY-1011, 📋 Planned), so the seccomp `CLONE_NEWUSER` block is the current primary defense against userns-based escapes.
- **Recommended controls:**
  - Audit each entitlement's capability grants; remove `CAP_SYS_PTRACE` and `CAP_SYS_CHROOT` unless specifically required.
  - Enable AppArmor or SELinux profiles for containers.

---

### 6.5 Denial of Service

#### TM-D-01 — Resource exhaustion via unauthenticated OTEL receivers
- **Severity:** MEDIUM
- **Status:** 📋 Planned
- **Component:** OTEL gRPC/HTTP receivers
- **Description:** Unauthenticated OTEL endpoints on ports 4317/4318 accept arbitrary payloads. A local process could flood the device with telemetry data, exhausting memory or CPU.
- **Existing mitigations:** In-memory broadcaster is the only sink; data is not persisted. Receivers are bound to loopback only, so remote network attackers cannot reach them.
- **Recommended controls:**
  - Apply rate limiting and maximum payload size to OTEL receivers.

#### TM-D-02 — Container resource exhaustion
- **Severity:** MEDIUM
- **Status:** ✅ Mitigated (WDY-1101)
- **Component:** Container service, cgroup limits
- **Description:** If a container's `wendy.json` does not declare CPU/memory limits, it runs without resource constraints. A runaway or malicious container can starve the host and other containers.
- **Existing mitigations:** A default PID limit (4096) is applied to every container regardless of manifest declarations, providing a fork-bomb / runaway-task guard (WDY-1101). CPU and memory limits are applied when declared in the app manifest.
- **Recommended controls:**
  - Apply conservative default CPU and memory limits for all containers when not declared (WDY-1729, coordination required for backward compatibility).
  - Alert (via telemetry) when container resource usage approaches limits.

#### TM-D-03 — Tunnel broker reconnection flood
- **Severity:** LOW
- **Status:** 📋 Planned
- **Component:** Tunnel broker client
- **Description:** The tunnel broker client retries with exponential backoff (max 90 seconds). If the broker is unreachable (e.g., intentional disruption), repeated connection attempts could produce unnecessary traffic and battery drain on constrained devices.
- **Existing mitigations:** Exponential backoff is implemented with a maximum interval of 90 seconds (`brokerMaxBackoff`).
- **Recommended controls:**
  - Cap total retry attempts over a configurable window before alerting or entering a degraded-mode sleep.

---

### 6.6 Elevation of Privilege

#### TM-E-01 — Container escape via D-Bus proxy bypass
- **Severity:** HIGH
- **Status:** ✅ Mitigated (WDY-1093)
- **Component:** D-Bus sandboxing (xdg-dbus-proxy)
- **Description:** xdg-dbus-proxy is used to filter container D-Bus access to the entitlement-allowed methods only. If xdg-dbus-proxy is not installed, a container with the `bluetooth` entitlement could call any D-Bus method on the system bus (e.g., NetworkManager, systemd).
- **Existing mitigations:** Container start is refused with a hard error when a D-Bus entitlement is declared and `xdg-dbus-proxy` is absent — there is no silent unfiltered fallback (WDY-1093). When the proxy is unavailable, the raw host D-Bus socket is never mounted into the container.
- **Recommended controls:**
  - Ensure `xdg-dbus-proxy` is included in all production OS images as a required dependency.

#### TM-E-02 — Privilege escalation via writable containerd socket
- **Severity:** MEDIUM
- **Status:** ✅ Mitigated (WDY-1102)
- **Component:** containerd socket `/run/containerd/containerd.sock`
- **Description:** The agent accesses containerd with root privileges. If a container can write to the containerd socket (e.g., via a path traversal or misbound mount), it can spawn privileged containers or modify host namespaces.
- **Existing mitigations:** containerd socket is not mounted into containers. Any bind mount whose source resolves into `/run/containerd` or `/var/run/containerd` is explicitly denied in the container spec builder (WDY-1102).
- **Recommended controls:**
  - Periodically audit container specs for bind mounts to sensitive host paths.

#### TM-E-03 — Escalation via `CAP_NET_ADMIN` in host-networked containers
- **Severity:** HIGH
- **Status:** ✅ Mitigated (WDY-1094)
- **Component:** Network entitlement (`host`), capabilities
- **Description:** Host-networked containers previously received `CAP_NET_ADMIN`. Combined with host networking, a container can reconfigure host network interfaces, add routing rules, intercept traffic, or disrupt connectivity for the device.
- **Existing mitigations:** Host networking requires explicit `network: host` entitlement. `CAP_NET_ADMIN` is no longer granted by host networking alone; it requires a separate explicit entitlement declaration (WDY-1094). This separates network visibility from the capability to reconfigure the network.
- **Recommended controls:**
  - Document `network: host` combined with explicit `CAP_NET_ADMIN` as a high-risk combination requiring reviewer sign-off.

---

## 7. Roadmap Hardening (Planned & In Progress)

Cross-cutting hardening not yet fully shipped (per-threat hardening is tracked in the catalogue above):

- **Codesigning, PKI, and certificate improvements — 🛠 in progress, targeted for Q3 2026.** This cluster includes cosign image-signature verification (TM-T-01), `wendy-agent` binary signing (TM-T-02), OS artifact signing (TM-T-03), cloud-CA pinning (TM-S-02), certificate revocation via OCSP/CRL with OCSP-stapled offline operator certificates (TM-S-03), and operator-specific permissions embedded in developer certificates (TM-S-03, 🛠 in progress).
- **Physical tamper-resistance of the hardware platform:**
  - 🛠 In progress: hardware-backed protection of key material (HSM/TPM) establishing a root of trust, so the device private key cannot be extracted via physical access — see TM-I-02.
  - 📋 Planned: on supported platforms, **fuse burning** (one-time-programmable eFuses) to lock secure-boot configuration and anchor the boot chain so an attacker with physical access cannot flash unsigned firmware or downgrade the bootloader.

---

## 8. Out of Scope

- Wendy Cloud backend infrastructure (separate threat model)
- Upstream containerd / Linux kernel vulnerabilities
- End-user devices running the `wendy` CLI (treated as a trusted principal once cert is issued)

---

## 9. Review Schedule

This threat model should be reviewed and updated:
- When a new external-facing service or protocol is added.
- When the provisioning flow changes.
- When container entitlements are added or modified.
- At minimum, every 6 months.

Last reviewed: 2026-06-28
