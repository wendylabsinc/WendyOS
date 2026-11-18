# Self-hosted macOS CI

The `wendy-mac` group runs builds on Apple Silicon Macs. Jobs use the runner's
active Xcode installation. They do not download Xcode, select a version, change
`xcode-select`, or override `DEVELOPER_DIR`.

Install Xcode and the iOS simulator runtime on the runner before starting its
service. Complete any license and first-launch prompts as part of machine setup.
The active installation must support the SDKs and Swift features used by the
repository.

Provision base tools as the runner account in an interactive terminal:

```sh
sudo -v
bash .github/scripts/provision-macos-ci.sh --base
```

The script installs Homebrew and Command Line Tools when absent, then Git and
Python 3.12. Initial Homebrew setup can need administrator access. Ordinary
Homebrew package installation runs as the runner account. Provision machines
one at a time with their runner services stopped, then restart the service.
Build jobs install additional formulae such as libusb, xcodegen, and archive tools.

The shared `setup-macos-ci` action repairs missing base tools and exposes
Homebrew's Python. This avoids the hosted Python installer's sudo requirement
on self-hosted Macs. It restores a Git checkout if checkout initially fell back
to a source archive. It sets `HOMEBREW_NO_ASK=1` for subsequent steps so Homebrew
package installs do not stop at an interactive confirmation prompt.

To verify a provisioned machine:

```sh
bash .github/scripts/provision-macos-ci.sh --base
xcodebuild -version
xcrun --sdk macosx --show-sdk-path
xcrun simctl list runtimes
PATH="$(brew --prefix python@3.12)/libexec/bin:$(brew --prefix)/bin:$PATH" \
  sh scripts/test/plugin-launcher/run.sh
```

Dispatch `build.yml` with `publish: false` after pushing workflow changes to
verify the complete build and validation. A successful setup check alone does
not verify Swift compilation, native app tests, or release packaging.
