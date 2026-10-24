#!/bin/bash
set -euo pipefail
mkdir -p "$TARGET_BUILD_DIR/$EXECUTABLE_FOLDER_PATH"
cp "$SRCROOT/Build/wendy" "$TARGET_BUILD_DIR/$EXECUTABLE_FOLDER_PATH/wendy"
FRAMEWORKS="$TARGET_BUILD_DIR/$FRAMEWORKS_FOLDER_PATH"
mkdir -p "$FRAMEWORKS"
install -m 755 "$SRCROOT/Build/libusb-1.0.dylib" "$FRAMEWORKS/libusb-1.0.dylib"
# Rewrite the CLI's build-machine dependency to its sibling Frameworks folder.
CLI="$TARGET_BUILD_DIR/$EXECUTABLE_FOLDER_PATH/wendy"
USB_DEPENDENCY="$(otool -L "$CLI" | awk '/libusb.*dylib/{print $1; exit}')"
if [[ -n "$USB_DEPENDENCY" ]]; then
  install_name_tool -change "$USB_DEPENDENCY" '@executable_path/../Frameworks/libusb-1.0.dylib' "$CLI"
fi
if [[ "${CODE_SIGNING_ALLOWED:-NO}" == YES && -n "${EXPANDED_CODE_SIGN_IDENTITY:-}" ]]; then
  codesign --force --options runtime --timestamp=none --sign "$EXPANDED_CODE_SIGN_IDENTITY" "$FRAMEWORKS/libusb-1.0.dylib"
  codesign --force --options runtime --timestamp=none --sign "$EXPANDED_CODE_SIGN_IDENTITY" "$CLI"
else
  codesign --force --sign - "$FRAMEWORKS/libusb-1.0.dylib"
  codesign --force --sign - "$CLI"
fi
