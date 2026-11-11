package commands

import (
	"errors"
	"fmt"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/failure"
	"github.com/wendylabsinc/wendy/go/proto/gen/cloudpb"
)

func TestDesktopFailureEvidence(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want failure.Info
	}{
		{"signed out", config.ErrNotLoggedIn, failure.Info{Code: failure.AuthenticationRequired, Remedy: failure.SignIn}},
		{"choose session", config.ErrMultipleSessions, failure.Info{Code: failure.AccountMismatch, Remedy: failure.ChooseAccount}},
		{"expired login", tlsHandshakeRejectedError{expiredLogin: &expiredLogin{}}, failure.Info{Code: failure.CredentialsExpired, Remedy: failure.SignIn}},
		{"local expiry", tlsHandshakeRejectedError{refreshReason: "local credential evidence"}, failure.Info{Code: failure.CredentialsExpired, Remedy: failure.SignIn}},
		{"other account", tlsHandshakeRejectedError{refreshReason: "expired", switchStep: "select correct account"}, failure.Info{Code: failure.AccountMismatch, Remedy: failure.ChooseAccount}},
		{"peer expiry is not local expiry", newTLSHandshakeRejectedError(errors.New("remote error: tls: expired certificate")), failure.Info{Code: failure.SecureConnectionRejected, Remedy: failure.CheckClock}},
		{"org", orgMismatchDeviceError{}, failure.Info{Code: failure.AccountMismatch, Remedy: failure.ChooseAccount}},
		{"cloud certificate", cloudCertError{code: cloudpb.CertificateErrorCode_CERTIFICATE_ERROR_UNAUTHORIZED}, failure.Info{Code: failure.AuthenticationRequired, Remedy: failure.SignIn}},
		{"untyped diagnostic", errors.New("The available client certificates are expired"), failure.Info{Code: failure.Unknown, Remedy: failure.None}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := DesktopFailure(fmt.Errorf("wrapped: %w", tc.err)); got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
