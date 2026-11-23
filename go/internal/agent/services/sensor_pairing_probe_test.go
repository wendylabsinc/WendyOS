package services_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/mcusource"
	"github.com/wendylabsinc/wendy/go/internal/agent/services"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const probeTenant = "11111111-2222-4333-8444-555555555555"

func probedPairingService(t *testing.T, probe services.PairingProbeFunc) (*services.SensorPairingService, *mcusource.PairingStore, *int) {
	t.Helper()
	t.Cleanup(services.SetPairingProbeRetryPause(time.Millisecond))
	store := mcusource.NewPairingStore(filepath.Join(t.TempDir(), "p.json"))
	starts := new(int)
	svc := services.NewSensorPairingService(zap.NewNop(), store, func() certs.Scope { return certs.Scope{TenantUUID: probeTenant} },
		func(mcusource.SensorPairing, string) { *starts++ }, func(mcusource.SensorPairing) {}, func(mcusource.SensorPairing) bool { return false })
	svc.SetPairingProbe(probe)
	return svc, store, starts
}

func addProbedPairing(svc *services.SensorPairingService, ctx context.Context, address string) error {
	_, err := svc.AddSensorPairing(ctx, &agentpbv2.AddSensorPairingRequest{
		SourcePrincipalUri: certs.DeviceSPIFFEURI(probeTenant, "lite-board"), Transport: "wendycom", Name: "micro-fire", SourceAddress: address})
	return err
}

func TestAddSensorPairingRefusesASourceThatRejectsThisDevice(t *testing.T) {
	probes := 0
	svc, store, starts := probedPairingService(t, func(context.Context, mcusource.SensorPairing, string) error {
		probes++
		return fmt.Errorf("SensorLink handshake: %w (issued by %q)", mcusource.ErrSourceRejectedCertificate, "Identity CA")
	})
	err := addProbedPairing(svc, context.Background(), "192.168.100.49:5054")
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "Identity CA") || !strings.Contains(err.Error(), "not saved") {
		t.Fatalf("err = %v, want FailedPrecondition naming the cause", err)
	}
	if len(store.List()) != 0 || *starts != 0 {
		t.Fatal("a pairing the source refuses was saved or started")
	}
	if probes != 2 {
		t.Fatalf("probes = %d, want a second probe to confirm the refusal", probes)
	}
}

// A board short of memory can end one handshake the way a refusal does; only
// a refusal that repeats keeps the pairing from being saved.
func TestAddSensorPairingSavesWhenARefusalDoesNotRepeat(t *testing.T) {
	probes := 0
	svc, store, starts := probedPairingService(t, func(context.Context, mcusource.SensorPairing, string) error {
		probes++
		if probes == 1 {
			return mcusource.ErrSourceRejectedCertificate
		}
		return nil
	})
	if err := addProbedPairing(svc, context.Background(), "192.168.100.49:5054"); err != nil {
		t.Fatal(err)
	}
	if probes != 2 || len(store.List()) != 1 || *starts != 1 {
		t.Fatalf("probes = %d, saved = %d, started = %d; want the pairing saved", probes, len(store.List()), *starts)
	}
}

func TestAddSensorPairingSavesWhenARefusalIsAmbiguous(t *testing.T) {
	ambiguous := mcusource.ErrSourceMayHaveRejectedCertificate
	confirmed := mcusource.ErrSourceRejectedCertificate
	for _, tc := range []struct {
		name          string
		first, second error
	}{
		{"two ambiguous errors", ambiguous, ambiguous},
		{"confirmed then ambiguous", confirmed, ambiguous},
		{"ambiguous then confirmed", ambiguous, confirmed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probes := 0
			svc, store, starts := probedPairingService(t, func(context.Context, mcusource.SensorPairing, string) error {
				probes++
				if probes == 1 {
					return tc.first
				}
				return tc.second
			})
			if err := addProbedPairing(svc, context.Background(), "192.168.100.49:5054"); err != nil {
				t.Fatal(err)
			}
			if probes != 2 || len(store.List()) != 1 || *starts != 1 {
				t.Fatalf("probes = %d, saved = %d, started = %d; want the pairing saved", probes, len(store.List()), *starts)
			}
		})
	}
}

func TestAddSensorPairingSavesWhenTheSourceIsNotReachable(t *testing.T) {
	for _, probeErr := range []error{nil, errors.New("dial tcp: i/o timeout"), context.DeadlineExceeded} {
		svc, store, starts := probedPairingService(t, func(context.Context, mcusource.SensorPairing, string) error { return probeErr })
		if err := addProbedPairing(svc, context.Background(), "192.168.100.49:5054"); err != nil {
			t.Fatalf("probe %v: %v", probeErr, err)
		}
		if len(store.List()) != 1 || *starts != 1 {
			t.Fatalf("probe %v: pairing not saved and started", probeErr)
		}
	}
}

func TestAddSensorPairingProbeLeavesTimeToSave(t *testing.T) {
	var probeDeadline time.Time
	svc, store, _ := probedPairingService(t, func(ctx context.Context, _ mcusource.SensorPairing, _ string) error {
		probeDeadline, _ = ctx.Deadline()
		<-ctx.Done()
		return ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	callerDeadline, _ := ctx.Deadline()
	if err := addProbedPairing(svc, ctx, "192.168.100.49:5054"); err != nil {
		t.Fatal(err)
	}
	if probeDeadline.IsZero() || !probeDeadline.Before(callerDeadline) {
		t.Fatalf("probe deadline %v is not before the caller's %v", probeDeadline, callerDeadline)
	}
	if len(store.List()) != 1 {
		t.Fatal("a source that did not answer in time was not saved")
	}
}

// The CLI sends no address; the agent finds the source by identity, so the
// probe must still run and resolve the source itself.
func TestAddSensorPairingProbesASourceWithoutAddress(t *testing.T) {
	var probedAddr *string
	svc, store, _ := probedPairingService(t, func(_ context.Context, _ mcusource.SensorPairing, addr string) error {
		probedAddr = &addr
		return mcusource.ErrSourceRejectedCertificate
	})
	err := addProbedPairing(svc, context.Background(), "")
	if probedAddr == nil || *probedAddr != "" {
		t.Fatalf("probe not run for a pairing without address")
	}
	if status.Code(err) != codes.FailedPrecondition || len(store.List()) != 0 {
		t.Fatalf("err = %v, saved = %d; want the refusal", err, len(store.List()))
	}
}

// A connected source already proved it accepts this device; a second session
// would cost a constrained board memory it may not have.
func TestAddSensorPairingSkipsProbeForAConnectedSource(t *testing.T) {
	store := mcusource.NewPairingStore(filepath.Join(t.TempDir(), "p.json"))
	svc := services.NewSensorPairingService(zap.NewNop(), store, func() certs.Scope { return certs.Scope{TenantUUID: probeTenant} },
		func(mcusource.SensorPairing, string) {}, func(mcusource.SensorPairing) {}, func(mcusource.SensorPairing) bool { return true })
	called := false
	svc.SetPairingProbe(func(context.Context, mcusource.SensorPairing, string) error {
		called = true
		return nil
	})
	if err := addProbedPairing(svc, context.Background(), "192.168.100.49:5054"); err != nil || called {
		t.Fatalf("err = %v, probed a connected source = %v", err, called)
	}
}
