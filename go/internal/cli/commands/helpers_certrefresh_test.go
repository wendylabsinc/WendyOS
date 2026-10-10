package commands

import (
	"errors"
	"strings"
	"testing"
)

func TestIsTLSCertificateError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "agent clientAuth EKU rejection",
			err:  errors.New("rpc error: code = Unauthenticated desc = certificate is not valid for client authentication"),
			want: true,
		},
		{
			name: "expired certificate",
			err:  errors.New("certificate not valid at current time (NotBefore=2025 NotAfter=2026)"),
			want: true,
		},
		{
			name: "tls expired alert",
			err:  errors.New("remote error: tls: expired certificate"),
			want: true,
		},
		{
			name: "tls bad certificate alert",
			err:  errors.New("remote error: tls: bad certificate"),
			want: true,
		},
		{
			name: "connection refused",
			err:  errors.New("connection refused"),
			want: false,
		},
		{
			name: "plaintext port probed with TLS",
			err:  errors.New("tls: first record does not look like a TLS handshake"),
			want: false,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTLSCertificateError(tc.err); got != tc.want {
				t.Errorf("isTLSCertificateError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestProvisionedAgentUnauthorizedErrorIncludesRefreshHint(t *testing.T) {
	refreshable := newProvisionedAgentUnauthorizedError(
		errors.New("host:50052: certificate is not valid for client authentication"))
	if !strings.Contains(refreshable.Error(), "wendy auth refresh-certs") {
		t.Errorf("expected refresh hint for cert rejection, got %q", refreshable.Error())
	}

	unreachable := newProvisionedAgentUnauthorizedError(errors.New("host:50052: connection refused"))
	if strings.Contains(unreachable.Error(), "wendy auth refresh-certs") {
		t.Errorf("did not expect refresh hint for reachability error, got %q", unreachable.Error())
	}
}
