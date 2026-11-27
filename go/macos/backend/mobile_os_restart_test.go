package backend

import (
	"context"
	"io"
	"testing"

	v1 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	pb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
)

func TestOSRestartRequiresCompletedInstallation(t *testing.T) {
	result := &pb.GetOSUpdateStatusResponse{CreatedAtUnix: 12}
	for _, state := range []OSInstallState{{}, {Active: true, RebootRequired: true}, {RebootRequired: true, Baseline: 11}} {
		if validateOSRestart(state, result) == nil {
			t.Fatalf("accepted unsafe restart %+v", state)
		}
	}
	if err := validateOSRestart(OSInstallState{RebootRequired: true, Baseline: 12}, result); err != nil {
		t.Fatal(err)
	}
	result.EngineStatus = &pb.OSUpdateEngineStatus{Pending: &pb.OSUpdateEngineStatus_PendingUpdate{Phase: "installed"}}
	if err := validateOSRestart(OSInstallState{}, result); err != nil {
		t.Fatal(err)
	}
	result.EngineStatus.Pending.Phase = "staging"
	if validateOSRestart(OSInstallState{}, result) == nil {
		t.Fatal("accepted incomplete engine update")
	}
	if validateOSRestart(OSInstallState{Active: true}, result) == nil {
		t.Fatal("accepted active install")
	}
}

func TestOSRestartNeedsExplicitAcknowledgement(t *testing.T) {
	for _, test := range []struct {
		code    int32
		eof     bool
		success bool
	}{{0, false, true}, {1, false, false}, {0, true, false}} {
		err := receiveOSRestart(func() (*v1.HostShellResponse, error) {
			if test.eof {
				return nil, io.EOF
			}
			return &v1.HostShellResponse{ResponseType: &v1.HostShellResponse_ExitCode{ExitCode: test.code}}, nil
		})
		if (err == nil) != test.success {
			t.Fatalf("%+v: %v", test, err)
		}
	}
}

func TestOSUpdateCancellationIsSessionScoped(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b := New()
	b.osUpdate.session = 7
	b.osUpdate.cancel = cancel
	b.CancelMobileOSUpdate(6)
	if ctx.Err() != nil {
		t.Fatal("stale session cancelled update")
	}
	b.CancelMobileOSUpdate(7)
	if ctx.Err() == nil {
		t.Fatal("matching session did not cancel update")
	}
}

func TestOSRestartRejectsMissingStaleAndUnauthenticatedSessions(t *testing.T) {
	agent := &mobileOSAgent{release: make(chan struct{}), calls: make(chan string, 1)}
	b := newMobileOSBackend(t, agent)
	for _, session := range []uint64{0, 6, 7} {
		if err := b.RestartAfterOSUpdate(session); err == nil {
			t.Fatalf("accepted session %d without authenticated connection", session)
		}
	}
}
