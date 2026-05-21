# Wendy Agent & Wendy CLI

## Installing the CLI

Install or update the `wendy` CLI on macOS or Linux (x86_64 and ARM64):

```sh
curl -fsSL https://install.wendy.sh/cli.sh | bash
```

On Windows:

```powershell
winget install WendyLabs.Wendy
```

Also available via [Homebrew, .deb, .rpm, and AUR](INSTALL.md).

## Installing the Agent

Install or update the `wendy-agent` on a Linux device (x86_64 and ARM64):

```sh
curl -fsSL https://install.wendy.sh/agent.sh | bash
```

Supports Debian/Ubuntu, Fedora/RHEL, and Arch Linux. Also available via [system packages](INSTALL.md).

## Building from Source

### CLI (Go)

The CLI is written in Go. To build from source:

```sh
cd go
make build-cli
./bin/wendy --version
```

On Windows, the binary must have the `.exe` suffix:

```powershell
cd go
go build -o bin\wendy.exe .\cmd\wendy
.\bin\wendy.exe --version
```

On macOS, CGO is required (for CoreBluetooth). It is enabled by default when
using the standard Go toolchain, but if you have explicitly disabled it:

```sh
cd go
CGO_ENABLED=1 make build-cli
```

### Agent (Go)

To build and run the agent locally:

```sh
cd go
go build -o wendy-agent ./cmd/wendy-agent
```

### Local Developer Tips

#### Repository-local Wendy CLI via direnv

The repository includes a `.envrc` that creates a tiny local `wendy` shim in
`.direnv/bin`. After allowing it once, `wendy` commands run from this checkout
rebuild and execute the Go CLI without overwriting an installed `wendy`:

```sh
direnv allow
wendy run
wendy discover --json
```

Outside of this checkout, your normal installed `wendy` remains unchanged.

You can still run the agent directly while developing it:

```sh
wendy-agent-dev() {
  (cd /path/to/wendy-agent/go && go run ./cmd/wendy-agent "$@")
}
```

#### Setup script one-liners

> **Warning**
> These setup scripts are mostly intended for throw-away development, test, and
> CI machines. The defaults avoid security-sensitive changes, so they can be run
> on personal machines if you carefully review the plan and choose only the
> options you actually want.

To download and run the interactive setup script directly:

macOS:

```sh
tmp="$(mktemp)" && trap 'rm -f "$tmp"' EXIT && curl -fsSL https://raw.githubusercontent.com/wendylabsinc/wendy-agent/main/utilities/set-up-macos.sh -o "$tmp" && bash "$tmp"
```

Ubuntu:

```sh
tmp="$(mktemp)" && trap 'rm -f "$tmp"' EXIT && curl -fsSL https://raw.githubusercontent.com/wendylabsinc/wendy-agent/main/utilities/set-up-ubuntu.sh -o "$tmp" && bash "$tmp"
```

Windows 11, from an elevated PowerShell session:

```powershell
$script = "$env:TEMP\set-up-windows.ps1"; iwr -UseBasicParsing https://raw.githubusercontent.com/wendylabsinc/wendy-agent/main/utilities/set-up-windows.ps1 -OutFile $script; powershell -ExecutionPolicy Bypass -File $script; Remove-Item $script
```

#### macOS setup script

For macOS development machines, this repository includes an interactive Homebrew
setup script:

```sh
./utilities/set-up-macos.sh
```

It installs common development packages including Git and Xcode Command Line
Tools, configures Bonjour/mDNS, installs the Swift toolchain requested by
`.swift-version`, prints manual steps for Xcode, SSH, Screen Sharing, and
automatic login, and optionally configures `direnv`, the Wendy CLI, the macOS
agent app, and a local clone of this repository.

#### Ubuntu setup script

For Ubuntu development devices, this repository includes an interactive setup
script:

```sh
./utilities/set-up-ubuntu.sh
```

It installs common development packages including Git, configures mDNS, installs
the Swift toolchain requested by `.swift-version`, and optionally configures SSH,
automatic login, `direnv`, the Wendy CLI, `wendy-agent`, and a local clone of
this repository.

#### Windows setup script

For Windows 11 development machines, run the interactive setup script from an
elevated PowerShell session:

```powershell
cd .\utilities
powershell -ExecutionPolicy Bypass -File .\set-up-windows.ps1
```

It installs common development packages including Git and GNU Make, configures
local discovery, and optionally configures SSH, automatic sign-in, `direnv`, the
Swift toolchain, and a local clone of this repository.

## Setting Up the Device

The device needs to run the `wendy-agent`. We provide pre-built [WendyOS](https://wendy.sh) images for the Raspberry Pi and the NVIDIA Jetson Orin Nano. These are preconfigured for remote debugging and have the wendy-agent preinstalled.

### Network Manager Support

WendyAgent supports both NetworkManager and ConnMan for WiFi configuration. The agent will automatically detect which network manager is available on the system:

- **ConnMan** is preferred for embedded/IoT devices due to its lighter resource usage
- **NetworkManager** is supported for desktop and server environments
- The agent will automatically detect and use the available network manager

#### Configuration

You can configure the network manager preference using the `WENDY_NETWORK_MANAGER` environment variable on the agent:

```sh
# Auto-detect (default)
export WENDY_NETWORK_MANAGER=auto

# Prefer ConnMan if available, fall back to NetworkManager
export WENDY_NETWORK_MANAGER=connman

# Prefer NetworkManager if available
export WENDY_NETWORK_MANAGER=networkmanager

# Force ConnMan (will fail if not available)
export WENDY_NETWORK_MANAGER=force-connman

# Force NetworkManager (will fail if not available)
export WENDY_NETWORK_MANAGER=force-networkmanager
```

If no environment variable is set, the agent will auto-detect the available network manager.

#### Manual Setup

The `wendy` CLI communicates with a `wendy-agent`. The agent uses containerd for running your apps.
On a Debian (or Ubuntu) based OS, you can do the following:

```sh
# Install containerd
sudo apt install containerd
# Start containerd and keep running across reboots
sudo systemctl start containerd
sudo systemctl enable containerd
```

Then install the agent using the install script above, or download a build from the [releases page](https://github.com/wendylabsinc/wendy-agent/releases).

## Examples

### Hello, world!

```sh
cd Examples/HelloWorld
wendy run
```

### Hello HTTP

A more advanced example demonstrating HTTP server capabilities:

```sh
cd Examples/HelloHTTP
wendy run
```

### Debugging

To debug an app, use the `--debug` flag:

```sh
wendy run --debug
```

This enables host networking for remote debugger access. For Python apps, `debugpy` is automatically injected and listens on port `5678`.

## Analytics

The Wendy CLI includes privacy-first anonymous usage analytics to help improve the developer experience. Analytics helps us understand which commands are used most, identify common errors, and prioritize improvements.

### What's Collected

- Command names and success/failure status
- Sanitized error types (no sensitive data)
- CLI version and operating system
- Anonymous identifier (UUID)

We **never** collect file paths, hostnames, project names, code, or any personally identifiable information.

### Managing Analytics

Check current analytics status:
```bash
wendy analytics status
```

Disable analytics:
```bash
wendy analytics disable
# Or set environment variable
export WENDY_ANALYTICS=false
```

Re-enable analytics:
```bash
wendy analytics enable
```

Analytics is automatically disabled in CI environments.
