//go:build !windows

package sessionbroker

import (
	"errors"
	"strings"
	"testing"
	"time"

	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestOperatorRecoveryMissingRouteExplainsPeerState(t *testing.T) {
	for _, tc := range []struct {
		name      string
		truncated bool
		peers     bool
		wantPeer  bool
	}{
		{name: "no peers", wantPeer: true},
		{name: "target announcement pending", peers: true},
		{name: "partial snapshot cannot prove no peers", truncated: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view, adjacent, target := remoteTopologyFixture(t)
			view.Routes = nil
			view.Truncated = tc.truncated
			if tc.peers {
				view.Peers = []*pb.LocalMeshPeerLink{{LinkId: 1, Principal: adjacent.Principal()}}
			}
			_, err := operatorManagementTarget(view, adjacent, target, time.Now())
			// Exercise the same serialized details the private IPC client receives.
			err = status.ErrorProto(status.Convert(err).Proto())
			retryable, steps := OperatorRecovery(err)
			if tc.truncated {
				if retryable || len(steps) != 1 || !strings.Contains(err.Error(), "availability is unknown") {
					t.Fatalf("partial snapshot became a known waiting/absence state: %v retry=%v steps=%v", err, retryable, steps)
				}
			} else if status.Code(err) != codes.Unavailable || !retryable || len(steps) != 2 {
				t.Fatalf("readiness error lost recovery detail: %v, retry=%v steps=%v", err, retryable, steps)
			}
			if got := strings.Contains(err.Error(), "no authenticated connections"); got != tc.wantPeer {
				t.Fatalf("peer absence claim = %v, want %v: %v", got, tc.wantPeer, err)
			}
		})
	}
}

func TestOperatorRecoveryDoesNotReclassifyOtherFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		code   codes.Code
		domain string
		reason string
	}{
		{"authentication", codes.Unauthenticated, operatorErrorDomain, operatorNoPeers},
		{"policy", codes.PermissionDenied, operatorErrorDomain, operatorNoPeers},
		{"other domain", codes.Unavailable, "other.service", operatorNoPeers},
		{"future reason", codes.Unavailable, operatorErrorDomain, "FUTURE_REASON"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, err := status.New(tc.code, "failure").WithDetails(&errdetails.ErrorInfo{Domain: tc.domain, Reason: tc.reason})
			if err != nil {
				t.Fatal(err)
			}
			if retry, steps := OperatorRecovery(st.Err()); retry || len(steps) != 0 {
				t.Fatalf("unrelated failure reclassified: retry=%v steps=%v", retry, steps)
			}
		})
	}
	if retry, _ := OperatorRecovery(status.Error(codes.Unavailable, "generic outage")); retry {
		t.Fatal("generic Unavailable became retryable")
	}
	if retry, _ := OperatorRecovery(errors.Join(operatorUnavailable(operatorNoPeers, "waiting"), status.Error(codes.PermissionDenied, "denied"))); retry {
		t.Fatal("a joined policy rejection became retryable")
	}
}

func TestOperatorRecoveryOnlySuggestsAutomaticJoining(t *testing.T) {
	errorsToCheck := []error{operatorUnavailable(operatorNoPeers, "waiting"), operatorUnavailable(operatorReconnecting, "reconnecting")}
	for _, kind := range []string{"timeout", "connection_closed", "device_pin", "other"} {
		errorsToCheck = append(errorsToCheck, &operatorStartupFailure{cause: errors.New("fixture"), failure: operatorFailure{Kind: kind}})
	}
	for _, err := range errorsToCheck {
		_, steps := OperatorRecovery(err)
		text := strings.Join(steps, " ")
		for _, removed := range []string{"--address", "--router", "--transport", "--psm", "selected first-hop", "choose a new first hop"} {
			if strings.Contains(text, removed) {
				t.Fatalf("recovery recommends unsupported targeting: %s", text)
			}
		}
	}
}
