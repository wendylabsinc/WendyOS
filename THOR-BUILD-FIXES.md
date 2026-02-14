# NVIDIA AGX Thor Build Fixes

**Date**: 2026-02-11
**Status**: Build Successfully Started
**Branch**: `feat/nvidia-agx-thor-support`

## Summary

All critical build blockers for NVIDIA Jetson AGX Thor (Tegra264) support have been resolved. The build now proceeds successfully past dependency resolution and begins compiling packages.

---

## Build Blockers Resolved

### 1. Missing Bootloader Build Dependencies

**Problem**:
- `hafnium_2.9-l4t-r38.2.1.bb` required `gn-native`, `lld-native`, `libcxx` from meta-clang
- `arm-trusted-firmware_2.8.16-l4t-r38.2.1.bb` required `virtual/cross-cc`
- These dependencies are complex to build and not needed when using prebuilt bootloaders

**Solution**:
1. Added meta-clang layer to provide LLVM build tools (partial solution)
2. Masked source-build recipes to force prebuilt versions (complete solution)

**Files Modified**:
- `/home/mihai/workspace-meta-wendyos-jetson/build/conf/bblayers.conf`
  ```bitbake
  BBLAYERS ?= " \
      ...
      ${TOPDIR}/../repos/meta-clang \
      ...
  ```

- `/home/mihai/workspace-meta-wendyos-jetson/build/conf/local.conf`
  ```bitbake
  # Mask source-build bootloader recipes to force prebuilt versions
  BBMASK += "meta-tegra/recipes-bsp/hafnium/hafnium_2.9-l4t-r38.2.1.bb"
  BBMASK += "meta-tegra/recipes-bsp/arm-trusted-firmware/arm-trusted-firmware_2.8.16-l4t-r38.2.1.bb"
  ```

**Result**: Build uses `hafnium-prebuilt` and `arm-trusted-firmware-prebuilt` recipes instead

---

### 2. EGL/GLES Provider Conflict

**Problem**:
- `tegra-libraries-multimedia`, `tegra-libraries-camera`, `tegra-libraries-multimedia-utils` depend on `virtual/egl` and `virtual/libgles2`
- mesa provides these but was skipped: `PREFERRED_PROVIDER_virtual/libgl set to libglvnd, not mesa`
- libglvnd was configured as the preferred provider but didn't explicitly PROVIDE the virtual interfaces

**Solution**:
Created libglvnd bbappend to explicitly provide virtual/egl, virtual/libgles1, and virtual/libgles2

**File Created**:
- `/home/mihai/workspace-meta-wendyos-jetson/meta-wendyos-jetson/recipes-graphics/libglvnd/libglvnd_%.bbappend`
  ```bitbake
  # Add virtual GL/EGL/GLES providers for wendyOS
  # libglvnd with NVIDIA's tegra-libraries-*core provides full GL/EGL/GLES stack
  # This resolves build dependency issues with tegra-libraries-multimedia packages

  # Explicitly provide virtual interfaces when appropriate PACKAGECONFIG options are enabled
  PROVIDES:append = " ${@bb.utils.contains('PACKAGECONFIG', 'egl', 'virtual/egl', '', d)}"
  PROVIDES:append = " ${@bb.utils.contains('PACKAGECONFIG', 'gles1', 'virtual/libgles1', '', d)}"
  PROVIDES:append = " ${@bb.utils.contains('PACKAGECONFIG', 'gles2', 'virtual/libgles2', '', d)}"
  ```

**Also Added to local.conf**:
```bitbake
# Fix EGL provider conflict - make libglvnd provide EGL instead of mesa
PREFERRED_PROVIDER_virtual/egl = "libglvnd"
```

**Result**: libglvnd now provides virtual/egl and virtual/libgles2, allowing tegra multimedia libraries to build

---

### 3. Missing tegra-boot-tools Dependency

**Problem**:
- `libubootenv-fake_1.0.bb` in meta-mender-tegra-common has `RDEPENDS:${PN} = "tegra-boot-tools"`
- Overrides exist for tegra234 and tegra194 to remove this dependency: `RDEPENDS:${PN}:tegra234 = ""`
- No override existed for tegra264 (Thor), causing build failure
- tegra-boot-tools doesn't exist for Thor/UEFI boot systems

**Solution**:
Created libubootenv-fake bbappend to add tegra264 override

**File Created**:
- `/home/mihai/workspace-meta-wendyos-jetson/meta-wendyos-jetson/recipes-bsp/u-boot/libubootenv-fake_%.bbappend`
  ```bitbake
  # Remove tegra-boot-tools dependency for Thor (tegra264)
  # Similar to tegra234 and tegra194, Thor uses UEFI boot and doesn't need boot-tools

  RDEPENDS:${PN}:tegra264 = ""
  ```

**Result**: libubootenv-fake no longer depends on non-existent tegra-boot-tools for Thor

---

### 4. tar-l4t-workaround-native Variable Expansion Issue

**Problem**:
- Recipe uses `S = "${UNPACKDIR}"` which causes variable expansion error
- Error: `Directory name ${@d.getVar('S') contains unexpanded bitbake variable`
- Likely a compatibility issue between scarthgap and the R38 branch

**Solution**:
Masked the recipe since it's a workaround and not essential for Thor builds

**File Modified**:
- `/home/mihai/workspace-meta-wendyos-jetson/build/conf/local.conf`
  ```bitbake
  # Mask tar-l4t-workaround-native - has UNPACKDIR variable expansion issue
  BBMASK += "meta-tegra/recipes-l4t-workarounds/tar/tar-l4t-workaround-native"
  ```

**Result**: Build proceeds without this workaround recipe

---

## Complete Configuration Changes

### New Files Created

1. **meta-wendyos-jetson/recipes-graphics/libglvnd/libglvnd_%.bbappend**
   - Adds virtual/egl and virtual/libgles* providers to libglvnd

2. **meta-wendyos-jetson/recipes-bsp/u-boot/libubootenv-fake_%.bbappend**
   - Removes tegra-boot-tools dependency for tegra264

### Files Modified

1. **build/conf/bblayers.conf**
   - Added meta-clang layer

2. **build/conf/local.conf**
   - Masked source-build hafnium and arm-trusted-firmware recipes
   - Masked tar-l4t-workaround-native recipe
   - Set PREFERRED_PROVIDER_virtual/egl = "libglvnd"

### External Repository Changes

1. **repos/meta-clang/** (NEW)
   - Cloned from https://github.com/kraj/meta-clang.git -b scarthgap
   - Provides LLVM build tools (though ultimately not used due to recipe masking)

---

## Build Status

### Parsing Statistics
- **Total recipes**: 3240 .bb files
- **Targets**: 5056
- **Skipped**: 379
- **Masked**: 65 (includes our 3 masked recipes)
- **Errors**: 0 ✅

### Task Statistics
- **Total tasks**: 8458
- **Status**: Build proceeding, compiling packages

### Version Warnings (Non-blocking)
These are expected version mismatches between pinned and available versions:
- tegra-libraries: 38.2.1% pinned, 38.2.2 available
- tensorrt-plugins: 10.13% pinned, 10.3.0 available
- nvidia-container: 1.16% pinned, 1.18.0-rc1 available

---

## Testing Next Steps

Once the build completes (estimated 2-6 hours depending on hardware), verify:

1. **Build Outputs**:
   - `wendyos-image-*.tegraflash.tar` - Flash package for Thor
   - `wendyos-image-*.mender` - OTA update artifact
   - `wendyos-image-*.dataimg` - Data partition image

2. **Flash to Hardware**:
   - Extract tegraflash package
   - Put Thor in recovery mode (USB ID 0955:7026)
   - Run `sudo ./initrd-flash.sh`

3. **Boot Validation**:
   - UEFI firmware initializes
   - Kernel 6.8 (Noble) boots
   - Login prompt appears
   - Partitions configured correctly (nvme0n1p1, p2, p11, p17)

4. **OTA Testing**:
   - Mender agent running
   - A/B partition switching works
   - Rollback functional

---

## Lessons Learned

### Yocto Layer Compatibility
- meta-tegra R38 branch designed for newer Yocto (whinlatter)
- Scarthgap compatibility requires careful patching and workarounds
- Some recipes (tar-l4t-workaround) don't work cleanly with scarthgap

### Provider Resolution
- libglvnd doesn't automatically provide virtual/* interfaces in all configurations
- Explicit PROVIDES statements needed for BitBake dependency resolution
- PREFERRED_PROVIDER alone insufficient if provider doesn't declare what it provides

### Machine-Specific Overrides
- Always check for machine-specific RDEPENDS overrides when adding new SoC support
- Thor (tegra264) needs same treatment as Orin (tegra234) and Xavier (tegra194)
- UEFI-based platforms share common patterns (no boot-tools, extlinux bootloader)

### Prebuilt vs Source Build
- Prebuilt bootloader components essential for rapid development
- Source builds require complex toolchain dependencies (LLVM/Clang ecosystem)
- Masking recipes more reliable than PREFERRED_PROVIDER for forcing prebuilt usage

---

## Recommendations

### For Production
1. Consider upgrading to Yocto whinlatter (5.3) for better R38 compatibility
2. Monitor meta-tegra for official scarthgap R38 support
3. Contribute libglvnd and libubootenv-fake fixes back to meta-mender-community

### For Development
1. Current configuration (scarthgap + patches) works for Thor development
2. Keep build/conf/local.conf BBMASK settings for reproducible builds
3. Document any additional recipe fixes in this file

---

## Related Documentation

- **THOR-IMPLEMENTATION-STATUS.md** - Complete Thor implementation status
- **conf/machine/jetson-agx-thor-devkit-nvme-wendyos.conf** - Thor machine config
- **conf/distro/include/l4t-r38-2-1.conf** - L4T R38.2.1 version pins
- **repos/meta-mender-community/meta-mender-tegra/meta-mender-tegra-jetpack7/** - Mender JetPack 7 layer

---

## Contact & Support

For questions about these fixes or Thor support:
- Review full implementation plan in project documentation
- Check THOR-IMPLEMENTATION-STATUS.md for hardware specs and partition layouts
- Refer to meta-tegra and meta-mender-community documentation for upstream changes
