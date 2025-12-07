# PR Stack Context - Download Validation & Security Hardening

Last updated: 2025-12-07

## PR Stack Structure

```
main
  └── wdy-578-download-validation-security-hardening (PR #190)
       └── wdy-579-archive-extraction-security (PR #188)
            └── wdy-580-temp-directory-resource-management (no active PR)
                 └── wdy-581-observability-configuration (PR #195)
```

## Branch Status

### wdy-578-download-validation-security-hardening (PR #190)
- **Targets**: main
- **PR**: https://github.com/wendylabsinc/wendy-agent/pull/190
- **Status**: ✅ Ready for review with fixes applied

**Recent Commits**:
- `c488463` - Remove Platform.current() fallback in downloadLatestRelease
- `681e001` - Fix platform detection bug in device update command
- `b8f3e64` - Support for Musl

**Changes**:
- Fixed critical platform detection bug where CLI would download macOS binaries for Linux devices
- Added `--platform` flag to `wendy device update` command
- Changed default platform from `Platform.current()` to `.linuxAarch64`
- Support for multiple platform string formats (linux-aarch64, aarch64, arm64, etc.)

**Files Modified**:
- `Sources/Wendy/cli/commands/DeviceCommand.swift` - Added platform flag and fixed defaults
- `Sources/Wendy/utils/FetchReleases.swift` - Changed default platform to linuxAarch64

### wdy-579-archive-extraction-security (PR #188)
- **Targets**: wdy-578-download-validation-security-hardening
- **PR**: https://github.com/wendylabsinc/wendy-agent/pull/188
- **Status**: ✅ Ready for review with subprocess migration complete

**Recent Commits**:
- `518d950` - Replace Foundation Process with swift-subprocess for tar operations

**Changes**:
- Migrated from Foundation `Process` to modern `swift-subprocess` for tar operations
- Simplified error handling with built-in stdout/stderr capture
- Removed manual Pipe management
- Both tar listing and extraction now use async/await subprocess API

**Files Modified**:
- `Sources/Wendy/utils/FetchReleases.swift` - Replaced Process with Subprocess.run()

### wdy-580-temp-directory-resource-management
- **Targets**: wdy-579-archive-extraction-security
- **PR**: No active PR
- **Status**: ⚠️ Needs rebasing onto updated wdy-579

**Known Issues**:
- Will have merge conflicts in `FetchReleases.swift` due to subprocess changes in wdy-579
- Needs rebase before creating PR

### wdy-581-observability-configuration (PR #195)
- **Targets**: wdy-580-temp-directory-resource-management
- **PR**: https://github.com/wendylabsinc/wendy-agent/pull/195
- **Status**: ⚠️ Needs rebasing after wdy-580 is rebased

## Review Feedback Status

### PR #190 Comments

#### ✅ Addressed
- **Line 12** (Import order): Musl/Glibc imports are correct
- **Line 98** (Platform detection bug): Fixed in commits 681e001 and c488463
  - Added explicit platform parameter to DeviceCommand
  - Changed default from Platform.current() to .linuxAarch64
  - Added --platform flag for flexibility
- **Line 125** (Permissions): Already correct - all setExecutablePermissions() calls properly specify permissions parameter

#### ⚠️ Deferred
- **Line 258** (Streaming downloads): Suggested as future enhancement, not blocking

### PR #188 Comments

#### ✅ Addressed
- **Line 311** (swift-subprocess): Fixed in commit 518d950
  - Replaced Foundation Process with Subprocess.run()
  - Simplified error handling
  - Used async/await subprocess API

#### ⚠️ Not Yet Addressed
- **Line 346** (Path traversal): Suggestion to check for bare ".." in addition to "../" and "/.."
  - Current code checks: `components.contains("..") || components.contains("/")`
  - Suggestion: Also check for bare ".." in path components
  - Impact: Low priority, existing checks already cover most cases

## Technical Details

### Platform Detection Fix

**Problem**: `downloadLatestRelease()` defaulted to `Platform.current()` which returns macOS when CLI runs on Mac, but agent binaries only exist for Linux platforms.

**Solution**:
1. Added `--platform` flag to `wendy device update` command:
   ```swift
   @Option(
       help: "Target platform for the agent binary (linux-aarch64 or linux-x86_64). Defaults to linux-aarch64."
   )
   var platform: String?
   ```

2. Changed default in `downloadLatestRelease()`:
   ```swift
   // Before: let targetPlatform = try platform ?? Platform.current()
   // After:  let targetPlatform = platform ?? .linuxAarch64
   ```

3. Platform string normalization accepts multiple formats:
   - `linux-aarch64`, `aarch64`, `arm64` → `.linuxAarch64`
   - `linux-x86_64`, `x86_64`, `amd64` → `.linuxX86_64`

### Swift-subprocess Migration

**Before** (Foundation Process):
```swift
let process = Process()
process.executableURL = URL(fileURLWithPath: "/usr/bin/env")
process.arguments = ["tar", "-tzf", url.path]
let pipe = Pipe()
process.standardOutput = pipe
try process.run()
process.waitUntilExit()
```

**After** (swift-subprocess):
```swift
let result = try await Subprocess.run(
    Subprocess.Executable.name("tar"),
    arguments: Subprocess.Arguments(["-tzf", url.path]),
    output: .string(limit: .max),
    error: .string(limit: .max)
)
```

## Next Steps for Resuming

### Immediate Actions
1. **Rebase wdy-580** onto updated wdy-579:
   ```bash
   git checkout wdy-580-temp-directory-resource-management
   git rebase wdy-579-archive-extraction-security
   # Resolve conflicts in FetchReleases.swift (subprocess vs logging changes)
   git push --force-with-lease
   ```

2. **Rebase wdy-581** onto updated wdy-580:
   ```bash
   git checkout wdy-581-observability-configuration
   git rebase wdy-580-temp-directory-resource-management
   git push --force-with-lease
   ```

3. **Update PR base branches** on GitHub if needed

### Optional Improvements
- Address line 346 suggestion: Add bare ".." check in path traversal validation
- Consider streaming download implementation (line 258 suggestion)

## Commit Guidelines

All commits in this stack are:
- ✅ Free of Claude Code footer
- ✅ Follow conventional commit format
- ✅ Have descriptive commit messages
- ✅ Are atomic and focused on single concerns

## Testing Checklist

Before merging each PR:
- [ ] Test `wendy device update` on macOS CLI targeting Linux devices
- [ ] Test `--platform linux-aarch64` flag
- [ ] Test `--platform linux-x86_64` flag
- [ ] Test platform string variations (aarch64, arm64, x86_64, amd64)
- [ ] Verify tar extraction works with subprocess implementation
- [ ] Test error handling for invalid binaries
- [ ] Verify backup and restore functionality during updates

## Related Issues

- Platform detection bug causing "noAsset" errors when running CLI on macOS
- Code modernization: Foundation Process → swift-subprocess
- Download validation and security hardening
- Archive extraction security improvements
