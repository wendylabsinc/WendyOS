# HelloXcode

A minimal command-line tool example that exercises the Xcode-only build path
(`*.xcodeproj`, no `Package.swift`).

## Creating the Xcode project

The `.xcodeproj` must be created manually with Xcode because SwiftPM cannot
copy Metal shader resource bundles into build products (the primary reason
Xcode projects are needed).

1. Open Xcode → **File › New › Project…**
2. Choose **macOS › Command Line Tool**, click **Next**.
3. Set **Product Name** to `HelloXcode`, language **Swift**.
4. Save into this directory (`Examples/HelloXcode/`).
5. Xcode creates `HelloXcode.xcodeproj/`. Confirm no `Package.swift` is present.

Optionally, add a resource bundle target to exercise the `.bundle` sync path:
1. **File › New › Target… › macOS › Bundle**, name it `Resources`.
2. Add it as a dependency of the `HelloXcode` target.
3. Xcode will place `Resources.bundle` next to the `HelloXcode` binary in the
   Release build products directory — `wendy run` syncs it automatically.

## Running

```
wendy run
```

`wendy` detects `HelloXcode.xcodeproj`, discovers the `HelloXcode` scheme,
builds with `xcodebuild -configuration Release`, syncs the binary (and any
sibling `.bundle` directories) to the target device, and starts the container.

## wendy.json options

| Key | Purpose |
|-----|---------|
| `xcode.scheme` | Override auto-detected scheme when the project has multiple schemes. |

Example:
```json
{
  "appId": "helloxcode",
  "version": "0.1.0",
  "language": "swift",
  "xcode": {
    "scheme": "HelloXcode"
  }
}
```
