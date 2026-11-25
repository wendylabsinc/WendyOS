#!/bin/bash
set -euo pipefail
mkdir -p "$TARGET_BUILD_DIR/$EXECUTABLE_FOLDER_PATH"
FRAMEWORKS="$TARGET_BUILD_DIR/$FRAMEWORKS_FOLDER_PATH"
mkdir -p "$FRAMEWORKS"
# Sign new inodes before replacing the installed files. Overwriting a running
# executable/library in place can invalidate macOS's cached code signature.
CLI="$(mktemp "$TARGET_BUILD_DIR/$EXECUTABLE_FOLDER_PATH/.wendy.XXXXXX")"
USB="$(mktemp "$FRAMEWORKS/.libusb.XXXXXX")"
trap 'rm -f "$CLI" "$USB"' EXIT
install -m 755 "$SRCROOT/Build/wendy" "$CLI"
install -m 755 "$SRCROOT/Build/libusb-1.0.dylib" "$USB"
USB_DEPENDENCY="$(otool -L "$CLI" | awk '/libusb.*dylib/{print $1; exit}')"
if [[ -n "$USB_DEPENDENCY" ]]; then
  install_name_tool -change "$USB_DEPENDENCY" '@executable_path/../Frameworks/libusb-1.0.dylib' "$CLI"
fi
if [[ "${CODE_SIGNING_ALLOWED:-NO}" == YES && -n "${EXPANDED_CODE_SIGN_IDENTITY:-}" ]]; then
  codesign --force --options runtime --timestamp=none --sign "$EXPANDED_CODE_SIGN_IDENTITY" "$USB"
  codesign --force --options runtime --timestamp=none --sign "$EXPANDED_CODE_SIGN_IDENTITY" "$CLI"
else
  codesign --force --sign - "$USB"
  codesign --force --sign - "$CLI"
fi
mv -f "$USB" "$FRAMEWORKS/libusb-1.0.dylib"
mv -f "$CLI" "$TARGET_BUILD_DIR/$EXECUTABLE_FOLDER_PATH/wendy"
