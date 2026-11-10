package commands

import (
	"errors"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/devicepin"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/failure"
)

// DesktopFailure retains the evidence collected during dialing. In particular,
// a peer's generic TLS alert does not establish that our login has expired.
func DesktopFailure(err error) failure.Info {
	var explicit *failure.Error
	if errors.As(err, &explicit) {
		return explicit.Info
	}
	// A denied browse is the platform's own answer, not a guess from its text.
	if errors.Is(err, discovery.ErrLocalNetworkDenied) {
		return failure.Info{Code: failure.LocalNetworkDenied, Remedy: failure.OpenNetworkSettings}
	}
	if expiredLoginFrom(err) != nil {
		return failure.Info{Code: failure.CredentialsExpired, Remedy: failure.SignIn}
	}
	if errors.Is(err, config.ErrNotLoggedIn) {
		return failure.Info{Code: failure.AuthenticationRequired, Remedy: failure.SignIn}
	}
	if errors.Is(err, config.ErrMultipleSessions) {
		return failure.Info{Code: failure.AccountMismatch, Remedy: failure.ChooseAccount}
	}
	if isUnauthenticatedCloudError(err) {
		return failure.Info{Code: failure.AuthenticationRequired, Remedy: failure.SignIn}
	}
	var org orgMismatchDeviceError
	if errors.As(err, &org) {
		return failure.Info{Code: failure.AccountMismatch, Remedy: failure.ChooseAccount}
	}
	var refusal *deviceIdentityRefusalError
	var identity *certs.IdentityMismatchError
	var pin *devicepin.PinMismatchError
	var blocking certs.BlockingPinError
	if errors.As(err, &refusal) || errors.As(err, &identity) || errors.As(err, &pin) || errors.As(err, &blocking) {
		return failure.Info{Code: failure.IdentityMismatch, Remedy: failure.ReviewIdentity}
	}
	var rejected tlsHandshakeRejectedError
	if errors.As(err, &rejected) {
		if rejected.switchStep != "" {
			return failure.Info{Code: failure.AccountMismatch, Remedy: failure.ChooseAccount}
		}
		if rejected.refreshReason != "" {
			return failure.Info{Code: failure.CredentialsExpired, Remedy: failure.SignIn}
		}
		return failure.Info{Code: failure.SecureConnectionRejected, Remedy: failure.CheckClock}
	}
	return failure.Describe(err)
}
