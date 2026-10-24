package devices

import (
	"errors"
	"testing"
)

func TestIsCertRejectionErrorIgnoresPlaintextTLSProbe(t *testing.T) {
	err := errors.New(`rpc error: code = Unavailable desc = connection error: desc = "transport: authentication handshake failed: tls: first record does not look like a TLS handshake"`)

	if isCertRejectionError("192.168.2.253:50052", err) {
		t.Fatal("isCertRejectionError() = true, want false for plaintext TLS probe")
	}
}

func TestIsCertRejectionErrorDetectsTLSAlert(t *testing.T) {
	err := errors.New("rpc error: code = Unavailable desc = remote error: tls: bad certificate")

	if !isCertRejectionError("192.168.2.253:50052", err) {
		t.Fatal("isCertRejectionError() = false, want true for TLS alert")
	}
}

func TestIsCertRejectionError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{
			// A plaintext (unprovisioned) agent probed with TLS: gRPC wraps the
			// "first record does not look like a TLS handshake" detail inside an
			// "authentication handshake failed" envelope. This is NOT a cert
			// rejection — the server simply isn't a TLS endpoint, so the CLI must
			// fall back to plaintext rather than report a bogus clock/cert error.
			"plaintext server probed with TLS",
			errors.New(`rpc error: code = Unavailable desc = connection error: desc = "transport: authentication handshake failed: tls: first record does not look like a TLS handshake"`),
			false,
		},
		{
			"server sent TLS alert (bad cert)",
			errors.New("rpc error: code = Unavailable desc = connection error: desc = \"remote error: tls: bad certificate\""),
			true,
		},
		{
			"client certificate required",
			errors.New("rpc error: code = Unavailable desc = connection error: desc = \"remote error: tls: certificate required\""),
			true,
		},
		{
			"plain transport error (connection refused)",
			errors.New(`rpc error: code = Unavailable desc = connection error: desc = "transport: Error while dialing: dial tcp 127.0.0.1:50052: connect: connection refused"`),
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCertRejectionError("192.168.2.253:50052", tc.err); got != tc.want {
				t.Errorf("isCertRejectionError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestIsCertRejectionErrorClassifiesHandshakeFailures(t *testing.T) {
	cases := []struct {
		name string
		msg  string
		want bool
	}{{
		name: "server sent a TLS alert",
		msg:  `rpc error: desc = "transport: authentication handshake failed: remote error: tls: bad certificate"`,
		want: true,
	}, {
		name: "server demanded a certificate",
		msg:  `rpc error: desc = "transport: authentication handshake failed: certificate required"`,
		want: true,
	}, {
		name: "plaintext agent probed with TLS",
		msg:  `rpc error: desc = "transport: authentication handshake failed: tls: first record does not look like a TLS handshake"`,
		want: false,
	}, {
		// Off loopback an EOF may be an on-path reset, so it stays strict.
		name: "handshake ended in EOF",
		msg:  `rpc error: desc = "transport: authentication handshake failed: EOF"`,
		want: true,
	}}
	for _, tc := range cases {
		if got := isCertRejectionError("192.168.2.253:50052", errors.New(tc.msg)); got != tc.want {
			t.Errorf("%s: isCertRejectionError() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestLoopbackClosedPortIsNotTreatedAsACertRejection(t *testing.T) {
	// QEMU's user-mode networking accepts on the host and only then finds the
	// guest port closed, so an unprovisioned VM's mTLS probe ends in EOF or a
	// TCP reset. Neither should suppress plaintext for an unpinned loopback VM.
	cases := []struct {
		name string
		msg  string
	}{
		{"EOF", `rpc error: desc = "transport: authentication handshake failed: EOF"`},
		{"TCP reset", `rpc error: code = Unavailable desc = connection error: desc = "transport: authentication handshake failed: read tcp 127.0.0.1:62970->127.0.0.1:50054: read: connection reset by peer"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := errors.New(tc.msg)
			for _, addr := range []string{"127.0.0.1:50054", "localhost:50054", "[::1]:50054"} {
				if isCertRejectionError(addr, err) {
					t.Errorf("%s: a loopback closed port still counts as a cert rejection", addr)
				}
			}
			for _, addr := range []string{"192.168.2.253:50052", "robot.local:50052", "[2001:db8::1]:50052"} {
				if !isCertRejectionError(addr, err) {
					t.Errorf("%s: a handshake failure off loopback stopped counting as a cert rejection", addr)
				}
			}
		})
	}
}

func TestLoopbackStillRejectsTLSAlertsAndOtherHandshakeFailures(t *testing.T) {
	for _, msg := range []string{
		`transport: authentication handshake failed: remote error: tls: bad certificate`,
		`transport: authentication handshake failed: certificate required`,
		`transport: authentication handshake failed: context deadline exceeded`,
		// A transport detail must not hide an explicit TLS rejection signal.
		`transport: authentication handshake failed: read tcp 127.0.0.1:62970->127.0.0.1:50054: read: connection reset by peer; remote error: tls: bad certificate`,
	} {
		if !isCertRejectionError("127.0.0.1:50054", errors.New(msg)) {
			t.Errorf("loopback handshake failure stopped counting as a cert rejection: %s", msg)
		}
	}
}
