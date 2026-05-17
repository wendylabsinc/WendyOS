//go:build wendy_dev_skip_gpg

package gpgverify

// SkipVerificationAllowed is true only in agent builds compiled with the
// `wendy_dev_skip_gpg` build tag. Such builds install updates WITHOUT GPG
// signature verification and must never be shipped to production devices.
const SkipVerificationAllowed = true
