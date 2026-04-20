# Release Process

This project uses a dual-track release system:
- **Pre-releases**: Automatically created on every push to `main` with timestamp-based tags (e.g., `2025.12.04-195108`)
- **Semver releases**: Manually promoted from pre-releases using the workflow dispatch UI

## Creating a Semver Release

### Quick Start

1. Go to **Actions** → **Create Semver Release** → **Run workflow**
2. Fill in the form:
   - **version**: Your semver version (e.g., `0.2.0`, `1.0.0-beta.1`)
   - **promote_prerelease**: The timestamp tag to promote (e.g., `2025.12.04-195108`)
3. Click **Run workflow**

### Finding Pre-release Tags

**Via GitHub UI:**
- Go to [Releases](../../releases)
- Look for pre-release entries with timestamp tags

**Via CLI:**
```bash
gh release list --limit 10
```

Example output:
```
v0.1.0        Latest    v0.0.1    2025-12-05T00:05:07Z
2025.12.04-195108    Pre-release    2025.12.04-195108    2025-12-04T20:08:54Z  ← Use this tag
2025.12.04-191348    Pre-release    2025.12.04-191348    2025-12-04T19:33:45Z
```

### Examples

**Promoting a pre-release to a stable release:**
- version: `0.2.0`
- promote_prerelease: `2025.12.04-195108`

**Creating a beta release:**
- version: `1.0.0-beta.1`
- promote_prerelease: `2025.12.04-195108`

**Creating a release candidate:**
- version: `1.0.0-rc.1`
- promote_prerelease: `2025.12.04-195108`

## Semver Guidelines

Follow [Semantic Versioning 2.0.0](https://semver.org/):

- **MAJOR.MINOR.PATCH** (e.g., `1.2.3`)
  - **MAJOR**: Incompatible API changes
  - **MINOR**: Backward-compatible new features
  - **PATCH**: Backward-compatible bug fixes

- **Pre-release versions** (e.g., `1.0.0-beta.1`, `2.0.0-rc.2`)
  - Append `-<identifier>.<number>` to the version
  - Common identifiers: `alpha`, `beta`, `rc` (release candidate)

## Workflow Details

The semver release workflow:
1. ✅ Validates the semver format
2. 📦 Downloads all build artifacts from the specified pre-release
3. 🔄 Renames assets to use the semver tag (critical for download scripts!)
4. 🚀 Creates a new GitHub release with the semver tag
5. 🏷️ Marks it as "latest" (non-pre-release)
6. 📝 Includes the original release notes + promotion note

### Asset Naming

**Pre-release assets** (timestamp-based):
```
wendy-agent-linux-arm64-2025.12.04-195108.tar.gz
wendy-cli-darwin-arm64-2025.12.04-195108.tar.gz
```

**Semver release assets** (renamed during promotion):
```
wendy-agent-linux-arm64-v0.2.0.tar.gz
wendy-cli-darwin-arm64-v0.2.0.tar.gz
```

**Download URL pattern:**
```
https://github.com/wendylabsinc/wendy-agent/releases/download/v0.2.0/wendy-agent-linux-arm64-v0.2.0.tar.gz
```

All platform builds are included:
- `wendy-agent-linux-arm64-${VERSION}.tar.gz`
- `wendy-agent-linux-amd64-${VERSION}.tar.gz`
- `wendy-agent-macos-universal-${VERSION}.zip`
- `wendy-cli-linux-arm64-${VERSION}.tar.gz`
- `wendy-cli-linux-amd64-${VERSION}.tar.gz`
- `wendy-cli-darwin-arm64-${VERSION}.tar.gz`
- `wendy-cli-darwin-amd64-${VERSION}.tar.gz`
- `wendy-cli-windows-amd64-${VERSION}.zip`
- `wendy-cli-windows-arm64-${VERSION}.zip`

## Automation

- **Pre-releases**: Fully automated on every `main` push
- **Homebrew nightly formula**: Auto-updated (direct push) for every pre-release
- **Homebrew nightly cask**: Auto-updated (direct push) for every pre-release
- **Homebrew stable formula**: Auto-updated via PR for semver releases
- **Homebrew stable cask**: Auto-updated via PR for semver releases
- **Winget**: Auto-updated for semver releases
- **Semver releases**: Manual promotion via workflow dispatch
- **Latest tag**: Auto-updated to point to the most recent pre-release

## FAQ

**Q: Can I skip the pre-release step?**
A: The workflow is designed to promote existing pre-releases. This ensures the binaries are tested before becoming official releases.

**Q: What if I need to rebuild for a semver release?**
A: Push to `main` to create a new pre-release, then promote it using the workflow.

**Q: Can I delete old pre-releases?**
A: Yes, but keep at least the last few in case you need to reference or promote them.

**Q: How do I update the Homebrew formula for a semver release?**
A: Manually create a PR in [homebrew-tap](https://github.com/wendylabsinc/homebrew-tap) or wait for the next pre-release to auto-update it, then manually adjust the version.
