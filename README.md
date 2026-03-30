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
go build -o wendy ./cmd/wendy
```

On macOS, CGO is required (for CoreBluetooth). It is enabled by default when
using the standard Go toolchain, but if you have explicitly disabled it:

```sh
cd go
CGO_ENABLED=1 go build -o wendy ./cmd/wendy
```

### Agent (Go)

```sh
cd go
go build -o wendy-agent ./cmd/wendy-agent
```

### Local Developer Tip

Add a `wendy-dev` alias to your shell profile (`~/.zshrc` or `~/.bashrc`) so you can quickly iterate on CLI changes without overwriting your installed `wendy`:

```sh
wendy-dev() {
  (cd /path/to/wendy-agent/go && go run ./cmd/wendy "$@")
}
```

Then use `wendy-dev` anywhere you'd normally use `wendy`:

```sh
wendy-dev run
wendy-dev discover --json
```

You can do the same for the agent:

```sh
wendy-agent-dev() {
  (cd /path/to/wendy-agent/go && go run ./cmd/wendy-agent "$@")
}
```

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
