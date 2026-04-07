# HelloXcode

A minimal command-line tool that exercises the Xcode-only build path
(`*.xcodeproj`, no `Package.swift`).

## Running

```
wendy run
```

`wendy` detects `HelloXcode.xcodeproj`, discovers the `HelloXcode` scheme,
builds with `xcodebuild -configuration Release`, and syncs the binary to
the target device. Build output is written to `.xcode/build.log`; follow
along in a second terminal with:

```
tail -f .xcode/build.log
```

## wendy.json options

Set `xcode.scheme` to override the auto-detected scheme when the project
has multiple schemes:

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
