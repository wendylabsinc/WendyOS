# Package-Specific Installation

For most users, the recommended installation method is the install script documented in [README.md](README.md). The instructions below are for users who prefer to install via a system package manager.

## CLI

### macOS (Homebrew)

```sh
brew tap wendylabsinc/tap
brew install wendy
```

For the nightly (prerelease) version:

```sh
brew tap wendylabsinc/tap
brew install wendy-nightly
```

To update:

```sh
brew upgrade wendy
```

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

### Windows (Winget)

```powershell
winget install WendyLabs.Wendy
```

To update:

```powershell
winget upgrade WendyLabs.Wendy
```

## Pre-built Binaries

Pre-built CLI binaries for Linux, macOS, and Windows, and agent binaries for Linux, are available on the [Releases](https://github.com/wendylabsinc/wendy-agent/releases) page.
