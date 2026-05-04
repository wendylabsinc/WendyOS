# Wendy Agent — Threat Model

**Version:** 1.0  
**Date:** 2026-05-01  
**Scope:** `wendy-agent` daemon, `wendy` CLI, embedded OCI registry, OTEL receivers, BLE stack

---

## 1. Overview

Wendy Agent is a containerised edge-computing platform that runs on IoT/embedded Linux devices (Raspberry Pi, NVIDIA Jetson, etc.). It exposes gRPC APIs for remote container management, WiFi configuration, Bluetooth connectivity, hardware access, and OS updates. It integrates with Wendy Cloud for certificate-based device provisioning and tunnelled remote access.

This document identifies assets, trust boundaries, threat actors, and STRIDE-categorised threats, together with existing mitigations and recommended controls.

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
| 50051 | gRPC plaintext | None | Pre-provisioned only |
| 50052 | gRPC mTLS | Client cert | Post-provisioned |
| 4317 | OTEL gRPC | None | Always |
| 4318 | OTEL HTTP | None | Always |
| 5000 | OCI Registry HTTP/HTTPS | None / TLS | HTTP pre-prov, HTTPS post-prov |
| BLE L2CAP | BLE L2CAP mTLS | Client cert | Post-provisioned |

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
- **Severity:** CRITICAL
- **Component:** Port 50051 (gRPC plaintext)
- **Description:** Before provisioning completes, port 50051 accepts gRPC connections from any host on the network with no authentication. All services — including `RunContainer`, `UpdateOS`, `UpdateAgent`, WiFi control, and Bluetooth — are reachable without credentials.
- **Existing mitigations:** Port is shut down after provisioning. Pre-enrollment at imaging time skips the post-boot window entirely.
- **Recommended controls:**
  - Default to requiring pre-enrollment for production deployments (skip post-boot provisioning window).
  - If post-boot provisioning is used, bind port 50051 to localhost or a provisioning VLAN only, and enforce a short provisioning timeout with automatic shutdown.
  - Document and test the expected window duration; alert if provisioning takes longer than expected.

#### TM-S-02 — Spoofed cloud endpoint during provisioning
- **Severity:** HIGH
- **Component:** Cloud gRPC dialer, pki-core
- **Description:** The cloud host is derived from CLI arguments or a config file. An attacker who can influence the config (e.g., via an evil captive portal during initial WiFi setup) could redirect the enrollment request to a rogue CA, causing the device to trust attacker-controlled certificates.
- **Existing mitigations:** TLS 1.2+ is enforced for cloud connections on standard ports.
- **Recommended controls:**
  - Pin or hard-code the expected cloud CA in the firmware image so it cannot be overridden by config.
  - Validate the cloud host against an allowlist before dialling.

#### TM-S-03 — Impersonation via stolen mTLS client certificate
- **Severity:** HIGH
- **Component:** CLI / gRPC mTLS (port 50052)
- **Description:** Client certificates have no short TTL or revocation check implemented. A stolen or leaked certificate grants indefinite authenticated access.
- **Existing mitigations:** Certificates are stored in `~/.wendy/` with user-level permissions.
- **Recommended controls:**
  - Implement certificate revocation (OCSP or CRL served by Wendy Cloud).
  - Enforce short-lived client certificates with automatic renewal.

---

### 6.2 Tampering

#### TM-T-01 — Malicious container image execution
- **Severity:** HIGH
- **Component:** Container service, OCI registry
- **Description:** Container images are sourced from a user-supplied registry. There is no image signature verification. A compromised image or a MITM between the registry and the device could result in execution of malicious code inside a container.
- **Existing mitigations:** Containers run as `appuser` (UID 1000) with namespace isolation and capability dropping.
- **Recommended controls:**
  - Enforce OCI image signature verification (Cosign / Notary v2) before `RunContainer`.
  - Pin images by digest (SHA256) in deployment manifests.

#### TM-T-02 — Agent binary tampering via `UpdateAgent`
- **Severity:** HIGH
- **Component:** `UpdateAgent` RPC
- **Description:** `UpdateAgent` downloads a new binary from a URL specified in the RPC call and verifies only SHA256. An attacker with CLI access (valid mTLS cert) could supply a malicious binary URL with a matching hash if the hash itself is attacker-controlled.
- **Existing mitigations:** SHA256 hash verification, mTLS required for the RPC.
- **Recommended controls:**
  - Sign agent binaries; verify the signature against a pinned public key embedded in the current binary.
  - Require the update URL to match an allowlisted domain.
  - Enforce that only binaries issued by Wendy's build pipeline are accepted (e.g., sigstore transparency log).

#### TM-T-03 — Mender artifact tampering (`UpdateOS`)
- **Severity:** HIGH
- **Component:** `UpdateOS` RPC, Mender
- **Description:** The Mender artifact URL is supplied in the RPC. If the download occurs over HTTP or the HTTPS chain is not validated, a MITM or DNS attack could deliver a malicious OS image.
- **Existing mitigations:** Mender's own artifact verification (signature optional per deployment).
- **Recommended controls:**
  - Enforce HTTPS-only artifact downloads with strict TLS validation.
  - Require Mender artifact signing and verify against a pinned key before install.
  - Restrict `UpdateOS` to callers with a dedicated high-privilege role (separate from regular operator cert).

#### TM-T-04 — Volume data tampering between containers
- **Severity:** MEDIUM
- **Component:** Volume entitlement (`persist`)
- **Description:** Volumes are scoped by name, not by app identity. Two apps with the same `persist` name share data. A malicious or compromised app could corrupt or exfiltrate data belonging to another app sharing the same volume name.
- **Existing mitigations:** Volume access requires an explicit entitlement declaration.
- **Recommended controls:**
  - Namespace volumes by a unique app identifier (not just user-supplied name) by default.
  - Document the sharing model explicitly; require explicit opt-in for cross-app sharing.

---

### 6.3 Repudiation

#### TM-R-01 — Insufficient audit trail for privileged operations
- **Severity:** MEDIUM
- **Component:** Agent RPC handlers
- **Description:** There is no structured audit log for privileged operations (RunContainer, UpdateOS, UpdateAgent, WiFi credential changes). If an incident occurs, it may be impossible to determine which authenticated principal performed a given action.
- **Existing mitigations:** Logs are written to stdout/journald; debug logs via `WENDY_DEBUG`.
- **Recommended controls:**
  - Emit a structured, tamper-evident audit event for every state-changing RPC, including: timestamp, RPC name, caller certificate CN, parameters (sanitised), and outcome.
  - Forward audit events to a remote log sink that the device cannot modify.

---

### 6.4 Information Disclosure

#### TM-I-01 — OTEL receivers accept unauthenticated data from any source
- **Severity:** MEDIUM
- **Component:** OTEL gRPC (4317), OTEL HTTP (4318)
- **Description:** OpenTelemetry receivers listen on all interfaces with no authentication. Any process on the network can submit arbitrary logs, metrics, and traces, polluting the telemetry stream with false data or extracting device observability data.
- **Existing mitigations:** None.
- **Recommended controls:**
  - Bind OTEL receivers to localhost or an internal-only network interface.
  - Require mTLS or a bearer token for OTEL submissions from external sources.

#### TM-I-02 — Private key at rest in `/etc/wendy-agent/`
- **Severity:** HIGH
- **Component:** Provisioning state, key material
- **Description:** The device private key (`device-key.pem`) and full provisioning state (including cert PEM) are stored in `/etc/wendy-agent/` at mode 0o600. Root access to the device (e.g., via physical UART, compromised container escape, or OS vulnerability) directly exposes the key.
- **Existing mitigations:** Files are mode 0o600, root-owned. The config partition copy is deleted after first boot.
- **Recommended controls:**
  - Use a hardware security module (HSM) or Trusted Platform Module (TPM) to protect the private key; only the agent process can exercise the key.
  - If HSM/TPM is unavailable, store the key in the kernel keyring (non-exportable) rather than a plain file.

#### TM-I-03 — Enrollment token intercepted in transit
- **Severity:** HIGH
- **Component:** Provisioning flow, plaintext port 50051
- **Description:** The CLI sends the enrollment token to the device over the pre-provisioning plaintext gRPC channel. An on-path attacker on the same network segment can intercept the token and enroll a different device in the same org.
- **Existing mitigations:** The token is single-use (invalidated after first use on cloud side — verify this is enforced).
- **Recommended controls:**
  - Confirm enrollment tokens are single-use and short-lived (< 15 minutes) at the cloud.
  - For post-boot provisioning, use a provisioning-time ECDH key exchange to protect the token in transit even over plaintext gRPC, or move provisioning to a TLS-bootstrapped channel.

#### TM-I-04 — BLE command interception and replay
- **Severity:** MEDIUM
- **Component:** BLE L2CAP stack
- **Description:** Before provisioning, BLE commands may be accepted without mTLS. Post-provisioning, the L2CAP channel is mTLS-protected, but if the channel is not bound to a unique session nonce, replayed mTLS records from a passive BLE sniffer could replay commands.
- **Existing mitigations:** Post-provisioning mTLS provides authentication and encryption.
- **Recommended controls:**
  - Verify that TLS session resumption is disabled on BLE channels (disable session tickets / stateless resumption).
  - Confirm pre-provisioning BLE behaviour: document and restrict which commands are available before mTLS is established.

#### TM-I-05 — Container escape exposing host filesystem
- **Severity:** MEDIUM
- **Component:** Container runtime, capability set
- **Description:** Containers are granted a broad capability set when device entitlements are enabled (including `CAP_SYS_PTRACE`, `CAP_SYS_CHROOT`, `CAP_NET_ADMIN`). A container vulnerability combined with these capabilities could allow a container escape to the host.
- **Existing mitigations:** `CAP_SYS_ADMIN` is not granted. User namespaces isolate container UID from host UID.
- **Recommended controls:**
  - Audit each entitlement's capability grants; remove `CAP_SYS_PTRACE` and `CAP_SYS_CHROOT` unless specifically required.
  - Enable a seccomp profile restricting dangerous syscalls (e.g., `ptrace`, `unshare`, `clone` with `CLONE_NEWUSER`).
  - Enable AppArmor or SELinux profiles for containers.

---

### 6.5 Denial of Service

#### TM-D-01 — Resource exhaustion via unauthenticated OTEL receivers
- **Severity:** MEDIUM
- **Component:** OTEL gRPC/HTTP receivers
- **Description:** Unauthenticated OTEL endpoints on ports 4317/4318 accept arbitrary payloads. An attacker on the same network could flood the device with telemetry data, exhausting memory or CPU.
- **Existing mitigations:** In-memory broadcaster is the only sink; data is not persisted.
- **Recommended controls:**
  - Apply rate limiting and maximum payload size to OTEL receivers.
  - Bind OTEL receivers to localhost or a dedicated management interface.

#### TM-D-02 — Container resource exhaustion
- **Severity:** MEDIUM
- **Component:** Container service, cgroup limits
- **Description:** If a container's `wendy.json` does not declare CPU/memory limits, it runs without resource constraints. A runaway or malicious container can starve the host and other containers.
- **Existing mitigations:** cgroup v3 limits are applied when declared in app manifest.
- **Recommended controls:**
  - Enforce default CPU and memory limits for all containers (deny containers without declared limits, or apply a conservative default).
  - Alert (via telemetry) when container resource usage approaches limits.

#### TM-D-03 — Tunnel broker reconnection flood
- **Severity:** LOW
- **Component:** Tunnel broker client
- **Description:** The tunnel broker client retries with exponential backoff (max 5 minutes). If the broker is unreachable (e.g., intentional disruption), repeated connection attempts could produce unnecessary traffic and battery drain on constrained devices.
- **Existing mitigations:** Exponential backoff is implemented with a maximum interval of 5 minutes.
- **Recommended controls:**
  - Cap total retry attempts over a configurable window before alerting or entering a degraded-mode sleep.

---

### 6.6 Elevation of Privilege

#### TM-E-01 — Container escape via D-Bus proxy bypass
- **Severity:** HIGH
- **Component:** D-Bus sandboxing (xdg-dbus-proxy)
- **Description:** xdg-dbus-proxy is used to filter container D-Bus access to the entitlement-allowed methods only. If xdg-dbus-proxy is not installed, the agent logs a warning and grants unfiltered access to the system D-Bus. A container with the `bluetooth` entitlement could then call any D-Bus method on the system bus (e.g., NetworkManager, systemd).
- **Existing mitigations:** Warning is logged when xdg-dbus-proxy is absent.
- **Recommended controls:**
  - Treat missing xdg-dbus-proxy as a hard error when a container declares any D-Bus entitlement; refuse to start the container rather than silently granting unfiltered access.

#### TM-E-02 — Privilege escalation via writable containerd socket
- **Severity:** MEDIUM
- **Component:** containerd socket `/run/containerd/containerd.sock`
- **Description:** The agent accesses containerd with root privileges. If a container can write to the containerd socket (e.g., via a path traversal or misbound mount), it can spawn privileged containers or modify host namespaces.
- **Existing mitigations:** containerd socket is not mounted into containers; containers use namespaces and capabilities.
- **Recommended controls:**
  - Explicitly deny any mount path matching `/run/containerd/*` in the container spec builder.
  - Periodically audit container specs for bind mounts to sensitive host paths.

#### TM-E-03 — Escalation via `CAP_NET_ADMIN` in host-networked containers
- **Severity:** HIGH
- **Component:** Network entitlement (`host`), capabilities
- **Description:** Host-networked containers receive `CAP_NET_ADMIN`. Combined with host networking, a container can reconfigure host network interfaces, add routing rules, intercept traffic, or disrupt connectivity for the device.
- **Existing mitigations:** Host networking requires explicit `network: host` entitlement.
- **Recommended controls:**
  - Separate the `network: host` entitlement (visibility) from the capability to reconfigure the network (`CAP_NET_ADMIN`); deny `CAP_NET_ADMIN` unless a specific, additional entitlement is declared.
  - Consider documenting `network: host` as a high-risk entitlement requiring reviewer sign-off.

---

## 7. Out of Scope

- Wendy Cloud backend infrastructure (separate threat model)
- Physical tamper-resistance of the hardware platform
- Upstream containerd / Linux kernel vulnerabilities
- End-user devices running the `wendy` CLI (treated as a trusted principal once cert is issued)

---

## 8. Review Schedule

This threat model should be reviewed and updated:
- When a new external-facing service or protocol is added.
- When the provisioning flow changes.
- When container entitlements are added or modified.
- At minimum, every 6 months.
