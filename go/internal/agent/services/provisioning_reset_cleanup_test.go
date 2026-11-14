package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/resetcleanup"
	agentpb "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func assertResetCleanupBlocksEnrollment(t *testing.T, svc *ProvisioningService) {
	t.Helper()
	ctx := context.Background()
	v2 := NewProvisioningServiceV2(svc)
	if _, err := svc.IsProvisioned(ctx, &agentpb.IsProvisionedRequest{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("v1 status exposed incomplete cleanup: %v", err)
	}
	if _, err := v2.IsProvisioned(ctx, &agentpbv2.IsProvisionedRequest{}); status.Code(err) != codes.Unavailable {
		t.Fatalf("v2 completion exposed incomplete cleanup: %v", err)
	}
	if _, err := svc.StartProvisioning(ctx, &agentpb.StartProvisioningRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("legacy enrollment started during cleanup: %v", err)
	}
	if _, err := v2.StartACMEProvisioning(ctx, acmeProvisionRequest()); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("ACME enrollment started during cleanup: %v", err)
	}
}

func TestCloudResetWaitsForResourceCleanupBeforeExposingCompletion(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "cleanup-failure"
		}
		t.Run(name, func(t *testing.T) {
			svc, req, ctx := cloudResetFixture(t)
			entered, release := make(chan struct{}), make(chan struct{})
			svc.BeforeUnprovisioned = func() error {
				close(entered)
				<-release
				if fail {
					return errors.New("owned mesh route removal failed")
				}
				return nil
			}
			restarted := make(chan struct{}, 1)
			svc.OnUnprovisioned = func() { restarted <- struct{}{} }
			type result struct {
				response *agentpbv2.UnprovisionResponse
				err      error
			}
			done := make(chan result, 1)
			go func() {
				response, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req)
				done <- result{response, err}
			}()
			select {
			case <-entered:
			case early := <-done:
				t.Fatalf("reset did not reach cleanup: %v", early.err)
			case <-time.After(5 * time.Second):
				t.Fatal("reset cleanup did not start")
			}
			// Always release the worker, including if a gate assertion fails.
			func() {
				defer close(release)
				assertResetCleanupBlocksEnrollment(t, svc)
				if _, err := os.Stat(filepath.Join(svc.configPath, "device-key.pem")); !os.IsNotExist(err) {
					t.Fatalf("authorized reset did not erase its key before cleanup: %v", err)
				}
			}()
			finished := <-done
			if fail {
				if status.Code(finished.err) != codes.Unavailable || finished.response != nil {
					t.Fatalf("cleanup failure reported reset success or retained credentials: %+v", finished)
				}
				assertResetCleanupBlocksEnrollment(t, svc)
				// Startup's successful owned-resource retry can expose the original
				// receipt without recreating credentials or changing authorization.
				svc.SetResetCleanupError(nil)
			} else if finished.err != nil || len(finished.response.GetUnenrollmentCompletion()) == 0 {
				t.Fatalf("clean reset did not complete: %+v", finished)
			}
			state, err := NewProvisioningServiceV2(svc).IsProvisioned(ctx, &agentpbv2.IsProvisionedRequest{})
			if err != nil || len(state.GetNotProvisioned().GetUnenrollmentCompletion()) == 0 {
				t.Fatalf("completed cleanup did not expose the durable receipt: %v", err)
			}
			if _, err := svc.StartProvisioning(ctx, &agentpb.StartProvisioningRequest{}); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("legacy enrollment started before reset restart: %v", err)
			}
			if _, err := NewProvisioningServiceV2(svc).StartACMEProvisioning(ctx, acmeProvisionRequest()); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("ACME enrollment started before reset restart: %v", err)
			}
			recovered := NewProvisioningService(zap.NewNop(), svc.configPath)
			state, err = NewProvisioningServiceV2(recovered).IsProvisioned(ctx, &agentpbv2.IsProvisionedRequest{})
			if err != nil || len(state.GetNotProvisioned().GetUnenrollmentCompletion()) == 0 || recovered.resetRestartPending {
				t.Fatalf("fresh process did not recover reset completion: state=%v err=%v", state, err)
			}
			select {
			case <-restarted:
			case <-time.After(2 * time.Second):
				t.Fatal("reset did not schedule restart for cleanup recovery")
			}
		})
	}
}

func TestResetResourceCleanupCannotClearInvalidAuthorization(t *testing.T) {
	svc := NewProvisioningService(zap.NewNop(), t.TempDir())
	svc.resetRecoveryErr = errors.New("invalid durable reset authorization")
	svc.SetResetCleanupError(errors.New("temporary cleanup failure"))
	svc.SetResetCleanupError(nil)
	assertResetCleanupBlocksEnrollment(t, svc)
}

// "Cleanup incomplete" alone leaves a person with nothing to act on: the device
// refuses to enroll and does not say why. Every refusal names what is failing,
// in its message for any client and as a detail a client can recognise.
func TestResetCleanupRefusalsSayWhatIsFailing(t *testing.T) {
	const cause = "wendy-net policy rule ownership conflict during reset cleanup: family=2 priority=51820 table=51820"
	svc := NewProvisioningService(zap.NewNop(), t.TempDir())
	svc.SetResetCleanupError(errors.New(cause))
	ctx := context.Background()
	v2 := NewProvisioningServiceV2(svc)

	refusals := []struct {
		name string
		code codes.Code
		call func() error
	}{
		{"v1 status", codes.Unavailable, func() error {
			_, err := svc.IsProvisioned(ctx, &agentpb.IsProvisionedRequest{})
			return err
		}},
		{"v2 status", codes.Unavailable, func() error {
			_, err := v2.IsProvisioned(ctx, &agentpbv2.IsProvisionedRequest{})
			return err
		}},
		{"legacy enrollment", codes.FailedPrecondition, func() error {
			_, err := svc.StartProvisioning(ctx, &agentpb.StartProvisioningRequest{})
			return err
		}},
		{"ACME enrollment", codes.FailedPrecondition, func() error {
			_, err := v2.StartACMEProvisioning(ctx, acmeProvisionRequest())
			return err
		}},
	}
	for _, refusal := range refusals {
		t.Run(refusal.name, func(t *testing.T) {
			err := refusal.call()
			if status.Code(err) != refusal.code {
				t.Fatalf("code = %v, want %v (err=%v)", status.Code(err), refusal.code, err)
			}
			if got, ok := resetcleanup.Cause(err); !ok || got != cause {
				t.Errorf("resetcleanup.Cause() = %q, %v; want the cleanup failure", got, ok)
			}
			if message := status.Convert(err).Message(); !strings.Contains(message, cause) {
				t.Errorf("message %q does not say what is failing", message)
			}
		})
	}

	// Once cleanup succeeds nothing of it is left on the answers.
	svc.SetResetCleanupError(nil)
	if _, err := v2.IsProvisioned(ctx, &agentpbv2.IsProvisionedRequest{}); err != nil {
		t.Fatalf("status after cleanup = %v, want success", err)
	}
}
