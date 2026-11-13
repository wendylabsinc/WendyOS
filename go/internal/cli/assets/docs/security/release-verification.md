# Release dependency coverage and publication gate

The `release-security` job in `.github/workflows/build.yml` inventories the final
release files after packaging and signing. Missing products, missing dependency
inventories, extraction errors, scanner errors, and High/Critical findings fail
the job. GitHub releases, APT/YUM uploads, agent GCS uploads, and install-script
updates require this job to succeed. AUR and WinGet publication depend on the
GitHub release.

## Product coverage

`release-coverage.json` maps all 20 release assets to their SHA-256 hashes,
extracted payload file hashes, and inventories. `wendy-release.spdx.json` combines
these inventories and records the original archive/installer hashes. The workflow
attests this combined SPDX document against every final release asset, then uses
`gh attestation verify` to verify each subject and compare its predicate with the
attached document. Publication rechecks the archive/installer hashes.

| Product/package | Architectures | Dependency evidence |
| --- | --- | --- |
| Linux Agent tarballs | amd64, arm64 | Syft inventory of the extracted Go binary |
| Linux CLI tarballs | amd64, arm64 | Syft inventory plus build-time libusb and musl versions, matched to the binary SHA-256 |
| macOS CLI tarball | arm64 | Syft inventory plus the statically linked Homebrew libusb version, matched to the binary SHA-256 |
| Windows CLI ZIPs | amd64, arm64 | Syft inventory of the final Authenticode-signed executable |
| macOS Agent app ZIP | arm64 | The Xcode workspace's resolved Swift packages, plus an inventory of its extracted runtime initramfs; all app payload files are hashed |
| macOS runtime guest tarball | arm64 | Syft inventory of the decompressed cpio root filesystem, including Alpine packages, the kernel package and Go tools; kernel and initramfs hashes |
| Web bundle | WASM | Imported Go modules and toolchain version from the WASM build environment, matched to the decompressed module hash; JavaScript and compressed payload hashes |
| CLI and Agent DEB/RPM | amd64, arm64 | Inventories of the extracted installer payloads, native library inventories matched by executable hash, DEB control files and RPM dependency/scriptlet metadata |
| Windows MSI | amd64, arm64 | Inventory of the extracted MSI payload, after MSI and executable signature verification |

The web release contains WASM, `wasm_exec.js`, and Wendy's worker/credential
scripts. It does not ship the web-client React application or its npm dependency
tree. The source SBOM is supplementary repository evidence, not a substitute for
the release-content inventories.

The Swift inventory includes the workspace's transitive pins and can include
packages used by workspace tests. It records both versions and Git revisions;
revision-only forks retain their commit hashes. It rejects missing direct
WendyAgentCore dependencies, missing revisions, empty locks and unsupported lock
formats, and compares every pin with Xcode's recorded dependency checkouts.
A fresh resolution in a separate job cannot substitute for the lock
produced by the app build.

DEB/RPM dependencies installed from a user's distribution repositories, such as
containerd, dbus, systemd, Avahi, usbutils and ca-certificates, are not bundled
release files. Their versions depend on the target distribution and must be
inventoried on the installed system. The MSI adds the executable and a PATH entry.
The macOS runtime's Alpine dependencies are bundled, so its root filesystem is
scanned separately.

## Scan policy and retained evidence

Grype scans each inventory with `--fail-on high`. All High/Critical findings block
publication, including findings without fixes. Scanner/database failures also
block publication. Reports include the scanner version, vulnerability database
metadata and matches. Inventories and reports are retained as the
`release-security-evidence` Actions artifact for 90 days, even when scans fail.
Successful releases attach the inventories, reports, coverage map and attestation
verification record.

There is no exception or bypass input. Accepted High/Critical findings are not
supported by this gate. Any future exception mechanism requires a reviewed policy
change that records the exact vulnerability/package/version, justification,
compensating controls, expiry and a security owner's approval. Do not add broad
scanner ignores or `continue-on-error` to publish a failing release.

A successful scan means no blocking findings in the scanner's current database.
It does not establish that every dependency is vulnerability-free. Revision-only
Swift forks and native code vendored inside Swift packages still require source
review when advisories cannot map to those revisions. The pinned fork revisions
remain visible for that review.

## Native signatures and remaining verification

The macOS app build verifies its code signature, notarization staple and Gatekeeper
assessment, and retains `macos-signatures.txt` with the release ZIP's SHA-256.
Windows packaging verifies Authenticode status for both architectures' executables
and MSI installers and retains signer identity and SHA-256 evidence. These checks
run after signing. Windows SBOM generation waits for signed ZIP replacement.

Linux binaries and standalone DEB/RPM release assets do not have a native signing
step in this workflow. Their integrity evidence is the GitHub attestation; it does
not constitute an RPM/DEB signature. The macOS CLI is not Developer ID signed or
notarized by this workflow. `macos-cli-signatures.txt` records verification of its
ad-hoc signature and its binary hash. Do not treat an ad-hoc Mach-O signature as publisher
authentication.

Issue [#2825](https://github.com/wendylabsinc/WendyOS/issues/2825) remains open until
a corrected release is available for re-review and the following external work is
recorded:

- In WendyOS-Builder, select a representative full OS image and record its release
  URL, source commit, SHA-256, complete image SBOM, build-provenance verification,
  vulnerability report and any native image-signature evidence. This repository's
  Alpine macOS guest is not a full WendyOS image.
- Confirm the desired publisher-signature policy for the macOS CLI and standalone
  Linux installers, and record verification of repository signing separately.
- Re-review GH-24 against the corrected release's assets and retained evidence,
  including manual advisory review for revision-only dependencies.

Do not mark the Builder image or the corrected-release review verified based only
on this workflow change or on the earlier sampled release.
