# Wendy runtime artifacts

`WendyAgentMac/RuntimeGuest/build.sh` places the generated, architecture-specific
initramfs and matching kernel here. CI builds and attests them before the macOS
packaging job. Xcode copies this directory into
`WendyAgentMac.app/Contents/Resources/runtime`.
