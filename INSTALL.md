# Package-Specific Installation

For most users, the recommended installation method is the install script documented in [README.md](README.md). The instructions below are for users who prefer to install via a system package manager.

## Keep the CLI current

Check your installed version against the latest stable release:

```sh
wendy update --check
```

Update with the package manager that owns the running CLI:

```sh
wendy update
```

The command asks before installing. In a script, approve explicitly with
`wendy update --yes`. Use `wendy update --check --json` for a read-only status
report. If your older CLI does not have `update`, use the package-specific
commands below once.

Homebrew, recognized WinGet installations, and packages owned by APT or RPM can
update through this command. APT/RPM updates use sudo when needed; without a
terminal, sudo must already be authorized or the update fails with instructions.
Arch/AUR and manually installed binaries receive instructions for their existing
installation method. Wendy does not silently switch package
managers, replace a nightly installation with stable, or create a second copy.
After an automatic update it checks the version of `wendy` on PATH; a package
manager reporting success alone is not enough.

Restart your Wendy MCP server or AI app after updating so it loads the new binary
and tools. Rerun `wendy mcp setup` when you also want to refresh bundled skills.

## CLI

### macOS (Homebrew)

> **Note:** The Wendy CLI for macOS requires **Apple Silicon (arm64)**. Intel (x86_64) Macs are not supported.

On Homebrew versions that support formula trust:

```sh
brew trust wendylabsinc/tap
brew trust --formula wendylabsinc/tap/wendy
brew install wendylabsinc/tap/wendy
```

On older Homebrew versions where `brew trust` is unavailable:

```sh
brew tap wendylabsinc/tap
brew install wendy
```

For the nightly (prerelease) version:

On Homebrew versions that support formula trust:

```sh
brew trust wendylabsinc/tap
brew trust --formula wendylabsinc/tap/wendy-nightly
brew install wendylabsinc/tap/wendy-nightly
```

On older Homebrew versions where `brew trust` is unavailable:

```sh
brew tap wendylabsinc/tap
brew install wendy-nightly
```

To update:

```sh
brew update && brew upgrade wendylabsinc/tap/wendy
```

If the tap is untrusted after a Homebrew update:

```sh
brew trust wendylabsinc/tap && brew upgrade wendylabsinc/tap/wendy
```

For an existing nightly installation, use
`brew update && brew upgrade wendylabsinc/tap/wendy-nightly`.

### Linux

Debian/Ubuntu (`.deb`):

```sh
sudo apt install ./wendy_<version>_<arch>.deb
```

Fedora/RHEL (`.rpm`):

```sh
sudo dnf install ./wendy-<version>.<arch>.rpm
```

Arch Linux (AUR):

```sh
yay -S wendy
```

To update a repository-managed package:

```sh
# Debian / Ubuntu
sudo apt-get update && sudo apt-get install --only-upgrade wendy

# Fedora / RHEL
sudo dnf upgrade wendy

# Arch / AUR
yay -S wendy
```

If you installed a downloaded `.deb` or `.rpm` without a Wendy package
repository, download the newer package and install it with the same command used
above. Then run `wendy --version` and `wendy update --check`.

### Windows (Winget)

```powershell
winget install WendyLabs.Wendy
```

To update:

```powershell
winget upgrade --id WendyLabs.Wendy --exact --source winget
```

## Agent

### Linux

Debian/Ubuntu (`.deb`):

```sh
sudo apt install ./wendy-agent_<version>_<arch>.deb
```

Fedora/RHEL (`.rpm`):

```sh
sudo dnf install ./wendy-agent-<version>.<arch>.rpm
```

Arch Linux (AUR):

```sh
yay -S wendy-agent
```

## Pre-built Binaries

Pre-built CLI binaries for Linux, macOS, and Windows, and agent binaries for Linux, are available on the [Releases](https://github.com/wendylabsinc/wendy-agent/releases) page.
