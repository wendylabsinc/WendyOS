# Wendy Platform — included Core adapters

Platform adapters live inside `wendy-core` and share its strict-SemVer version and release guarantees. They have no independent version/release train. Builds include only the required adapters/capabilities; sharing a repository does not collapse the lower Core boundary or imply support for every family/variant.

Native adapters implement narrow capabilities that portable Core cannot safely or portably own, such as hardware-backed keys, platform credential stores, native discovery and lifecycle/background execution. Consumer bindings do not own these adapters; implementation language is independent of the consuming SDK language.

`include/WendyCore/WendyPlatform.h` reserves the lower public C contract, but declares no capability or registration API. A real capability requires a versioned contract, ownership/lifecycle/threading/cancellation rules and explicit availability. Unsupported, temporarily unavailable and permission denied are distinct outcomes. No auth, signing, credential or tunnel behavior is implemented by this scaffold.

`apple/common` holds family-level responsibility notes; `apple/ios`, `apple/macos` and `apple/watchos` hold variant differences. Android, Linux, Windows and Web have their own directories. Directory presence is not a support claim. Browser/WASM engine support is not proof of a Web capability adapter.
