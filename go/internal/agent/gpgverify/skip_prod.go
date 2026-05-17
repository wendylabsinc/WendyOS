//go:build !wendy_dev_skip_gpg

package gpgverify

// SkipVerificationAllowed reports whether this agent build is permitted to
// install updates without verifying their GPG signature.
//
// In production builds it is always false: signature verification can never
// be bypassed, and no client request field can change that. A developer who
// genuinely needs to install an unsigned local build must compile the agent
// with the `wendy_dev_skip_gpg` build tag, which is an explicit, local,
// build-time decision rather than a remotely controllable one.
const SkipVerificationAllowed = false
