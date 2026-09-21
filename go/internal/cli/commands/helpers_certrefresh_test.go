package commands

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
)

func TestIsCertRefreshableError(t *testing.T) {
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
			if got := isCertRefreshableError(tc.err); got != tc.want {
				t.Errorf("isCertRefreshableError(%v) = %v, want %v", tc.err, got, tc.want)
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

func TestOfferCertRefreshAndRetry(t *testing.T) {
	certErr := errors.New("certificate is not valid for client authentication")

	setup := func(interactive, accept bool, refreshErr error) (restore func(), refreshCalls, retryCalls *int) {
		origInteractive := isInteractiveTerminalFn
		origJSON := jsonOutput
		origPrompt := confirmFn
		origPromptNo := confirmDefaultNoFn
		origRefresh := refreshAllCertsFn
		refreshCalls = new(int)
		retryCalls = new(int)
		isInteractiveTerminalFn = func() bool { return interactive }
		jsonOutput = false
		confirmFn = func(string) bool { return accept }
		confirmDefaultNoFn = func(string) bool { return accept }
		refreshAllCertsFn = func(context.Context) error {
			*refreshCalls++
			return refreshErr
		}
		return func() {
			isInteractiveTerminalFn = origInteractive
			jsonOutput = origJSON
			confirmFn = origPrompt
			confirmDefaultNoFn = origPromptNo
			refreshAllCertsFn = origRefresh
		}, refreshCalls, retryCalls
	}

	t.Run("non-interactive option overrides terminal", func(t *testing.T) {
		restore, refreshCalls, retryCalls := setup(true, true, nil)
		defer restore()
		confirmFn = func(string) bool {
			t.Fatal("non-interactive connection prompted for certificate refresh")
			return false
		}
		confirmDefaultNoFn = confirmFn
		_, ok := offerCertRefreshAndRetry(context.Background(), true, certErr, func() (*grpcclient.AgentConnection, error) {
			*retryCalls++
			return nil, nil
		})
		if ok || *refreshCalls != 0 || *retryCalls != 0 {
			t.Fatalf("non-interactive refresh = %v, refresh calls = %d, retry calls = %d", ok, *refreshCalls, *retryCalls)
		}
	})

	t.Run("accepted refresh retries and returns connection", func(t *testing.T) {
		restore, refreshCalls, retryCalls := setup(true, true, nil)
		defer restore()

		wantConn := &grpcclient.AgentConnection{Host: "device.local"}
		conn, ok := offerCertRefreshAndRetry(context.Background(), false, certErr, func() (*grpcclient.AgentConnection, error) {
			*retryCalls++
			return wantConn, nil
		})
		if !ok || conn != wantConn {
			t.Fatalf("offerCertRefreshAndRetry() = (%v, %v), want (%v, true)", conn, ok, wantConn)
		}
		if *refreshCalls != 1 || *retryCalls != 1 {
			t.Fatalf("refreshCalls=%d retryCalls=%d, want 1 and 1", *refreshCalls, *retryCalls)
		}
	})

	t.Run("declined prompt does not refresh or retry", func(t *testing.T) {
		restore, refreshCalls, retryCalls := setup(true, false, nil)
		defer restore()

		_, ok := offerCertRefreshAndRetry(context.Background(), false, certErr, func() (*grpcclient.AgentConnection, error) {
			*retryCalls++
			return nil, nil
		})
		if ok || *refreshCalls != 0 || *retryCalls != 0 {
			t.Fatalf("ok=%v refreshCalls=%d retryCalls=%d, want false, 0, 0", ok, *refreshCalls, *retryCalls)
		}
	})

	t.Run("enrolled timeout offers refresh and retries when accepted", func(t *testing.T) {
		restore, refreshCalls, retryCalls := setup(true, true, nil)
		defer restore()

		timeoutErr := newProvisionedAgentUnauthorizedError(
			errors.New("dial tcp 192.168.1.50:50052: i/o timeout"))
		wantConn := &grpcclient.AgentConnection{Host: "device.local"}
		conn, ok := offerCertRefreshAndRetry(context.Background(), false, timeoutErr, func() (*grpcclient.AgentConnection, error) {
			*retryCalls++
			return wantConn, nil
		})
		if !ok || conn != wantConn {
			t.Fatalf("offerCertRefreshAndRetry() = (%v, %v), want (%v, true)", conn, ok, wantConn)
		}
		if *refreshCalls != 1 || *retryCalls != 1 {
			t.Fatalf("refreshCalls=%d retryCalls=%d, want 1 and 1", *refreshCalls, *retryCalls)
		}
	})

	t.Run("enrolled timeout declined does not refresh or retry", func(t *testing.T) {
		restore, refreshCalls, retryCalls := setup(true, false, nil)
		defer restore()

		timeoutErr := newProvisionedAgentUnauthorizedError(
			errors.New("dial tcp 192.168.1.50:50052: i/o timeout"))
		_, ok := offerCertRefreshAndRetry(context.Background(), false, timeoutErr, func() (*grpcclient.AgentConnection, error) {
			*retryCalls++
			return nil, nil
		})
		if ok || *refreshCalls != 0 || *retryCalls != 0 {
			t.Fatalf("ok=%v refreshCalls=%d retryCalls=%d, want false, 0, 0", ok, *refreshCalls, *retryCalls)
		}
	})

	t.Run("non-interactive terminal never prompts", func(t *testing.T) {
		restore, refreshCalls, _ := setup(false, true, nil)
		defer restore()

		_, ok := offerCertRefreshAndRetry(context.Background(), false, certErr, func() (*grpcclient.AgentConnection, error) {
			return nil, nil
		})
		if ok || *refreshCalls != 0 {
			t.Fatalf("ok=%v refreshCalls=%d, want false, 0", ok, *refreshCalls)
		}
	})

	t.Run("non-refreshable error never prompts", func(t *testing.T) {
		restore, refreshCalls, _ := setup(true, true, nil)
		defer restore()

		_, ok := offerCertRefreshAndRetry(context.Background(), false, errors.New("connection refused"), func() (*grpcclient.AgentConnection, error) {
			return nil, nil
		})
		if ok || *refreshCalls != 0 {
			t.Fatalf("ok=%v refreshCalls=%d, want false, 0", ok, *refreshCalls)
		}
	})

	t.Run("failed refresh does not retry", func(t *testing.T) {
		restore, refreshCalls, retryCalls := setup(true, true, fmt.Errorf("cloud unreachable"))
		defer restore()

		_, ok := offerCertRefreshAndRetry(context.Background(), false, certErr, func() (*grpcclient.AgentConnection, error) {
			*retryCalls++
			return nil, nil
		})
		if ok || *refreshCalls != 1 || *retryCalls != 0 {
			t.Fatalf("ok=%v refreshCalls=%d retryCalls=%d, want false, 1, 0", ok, *refreshCalls, *retryCalls)
		}
	})

	t.Run("failed retry returns not ok", func(t *testing.T) {
		restore, _, _ := setup(true, true, nil)
		defer restore()

		_, ok := offerCertRefreshAndRetry(context.Background(), false, certErr, func() (*grpcclient.AgentConnection, error) {
			return nil, errors.New("still unauthorized")
		})
		if ok {
			t.Fatal("expected ok=false when retry fails")
		}
	})
}
