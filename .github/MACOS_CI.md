# Self-hosted macOS CI

The `wendy-mac` group runs builds on Apple Silicon Macs. Provision each machine
as its runner account before starting the runner service. A macOS installation
alone provides Git and Python shims that cannot run without developer tools.

From a checkout of this repository, in an interactive terminal:

```sh
sudo -v
bash .github/scripts/provision-macos-ci.sh 26.3 26.5 --ios-runtime
```

The script installs Homebrew and Command Line Tools when absent, then Git and
Python 3.12. It downloads a checksum-pinned, prebuilt
[xcodes](https://github.com/XcodesOrg/xcodes) release to `~/.local/bin/xcodes` to install
Xcode 26.3 for Swift validation and 26.5 for release and native app jobs, then
downloads the iOS simulator runtime for 26.5. Xcode downloads can require an
Apple account and two-factor authentication. Complete those prompts on the
runner. `xcodes` saves the account password in that runner's login keychain;
run `~/.local/bin/xcodes signout` after provisioning to remove it.
Do not put Apple credentials into build workflow secrets. Existing
Xcode versions are reused. Xcode installation needs administrator access;
ordinary Homebrew package installation runs as the runner account.

Provision machines one at a time with their runner services stopped. Allow
enough disk space for both expanded Xcode installations, download archives,
the simulator runtime, and build outputs. Restart the runner service after
provisioning. Build jobs install their additional formulae, such as libusb,
xcodegen, and the archive tools.

The shared `setup-macos-ci` action repairs missing base tools, exposes Homebrew's
Python instead of using the hosted Python installer's `/Users/runner` prefix,
and restores a Git checkout if checkout initially fell back to a source archive.
It sets `HOMEBREW_NO_ASK=1` for subsequent steps so Homebrew 7 package installs
do not stop at an interactive confirmation prompt.
Jobs declare their Xcode version through the action input. Selection uses
`DEVELOPER_DIR` for that job, so concurrent jobs do not change each other's
global Xcode selection. Missing versions fail with the provisioning command
before compilation or package installation steps.

To verify a provisioned machine:

```sh
bash .github/scripts/provision-macos-ci.sh --base
bash .github/scripts/select-macos-ci-xcode.sh 26.3
bash .github/scripts/select-macos-ci-xcode.sh 26.5
PATH="$(brew --prefix python@3.12)/libexec/bin:$(brew --prefix)/bin:$PATH" \
  sh scripts/test/plugin-launcher/run.sh
```

Dispatch `build.yml` with `publish: false` after the workflow changes are pushed
to verify the complete build and validation. A successful setup check alone
does not verify Swift compilation, native app tests, or release packaging.
