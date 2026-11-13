package services

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/timesync"
	"github.com/wendylabsinc/wendy/go/internal/shared/unenrollproof"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Every refusal must leave both the enrollment and credential bytes intact.
func checkResetStateUnchanged(t *testing.T, svc *ProvisioningService) func() {
	t.Helper()
	original := make(map[string][]byte)
	for _, name := range []string{"provisioning.json", "device-key.pem", "acme-account-key.pem"} {
		data, err := os.ReadFile(filepath.Join(svc.configPath, name))
		if err != nil {
			t.Fatal(err)
		}
		original[name] = data
	}
	return func() {
		t.Helper()
		for name, want := range original {
			got, err := os.ReadFile(filepath.Join(svc.configPath, name))
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("reset refusal changed %s: %v", name, err)
			}
		}
	}
}

// anchored stamps an injected window with the current boot time, as FreshTime does.
func anchored(w timesync.TimeWindow) timesync.TimeWindow {
	w.Boot, _ = timesync.BootTime()
	return w
}

func TestCloudResetCountsSuspendSinceFreshTime(t *testing.T) {
	boot, err := timesync.BootTime()
	if err != nil || boot <= int64(2*time.Minute) {
		t.Skip("requires a boot-clock anchor at least two minutes old to model suspension")
	}
	svc, req, ctx := cloudResetFixture(t)
	unchanged := checkResetStateUnchanged(t, svc)
	svc.trustedTime = func(context.Context) (timesync.TimeWindow, error) {
		now := time.Now()
		w := anchored(timesync.TimeWindow{Earliest: now, Latest: now})
		w.Boot -= int64(2 * time.Minute) // suspend after the sample
		return w, nil
	}
	_, err = NewProvisioningServiceV2(svc).Unprovision(ctx, req)
	if !strings.Contains(status.Convert(err).Message(), "Cloud reset authorization") {
		t.Fatalf("30-second authorization after 2m of suspend: %v", err)
	}
	unchanged()
}

func TestCloudResetChecksOperatorValidityAtAuthenticatedTime(t *testing.T) {
	for _, name := range []string{"expired", "not-yet-valid"} {
		t.Run(name, func(t *testing.T) {
			svc, req, ctx := cloudResetFixture(t)
			unchanged := checkResetStateUnchanged(t, svc)
			p, _ := peer.FromContext(ctx)
			actor := p.AuthInfo.(credentials.TLSInfo).State.PeerCertificates[0]
			if name == "expired" {
				actor.NotAfter = time.Now().Add(-time.Hour)
			} else {
				actor.NotBefore = time.Now().Add(time.Hour)
			}
			if _, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req); status.Code(err) != codes.PermissionDenied {
				t.Fatalf("reset = %v, want permission denied", err)
			}
			unchanged()
		})
	}
}

func TestCloudResetChecksRevocationFreshnessAtAuthenticatedTime(t *testing.T) {
	for _, offset := range []time.Duration{-2 * time.Hour, 2 * time.Hour} {
		t.Run(offset.String(), func(t *testing.T) {
			// The signed OCSP evidence is expired or not yet valid. Keep the
			// operator valid so the refusal specifically exercises that proof.
			svc, req, ctx := cloudResetFixtureAt(t, time.Now().Add(offset))
			unchanged := checkResetStateUnchanged(t, svc)
			p, _ := peer.FromContext(ctx)
			actor := p.AuthInfo.(credentials.TLSInfo).State.PeerCertificates[0]
			actor.NotBefore = time.Now().Add(-4 * time.Hour)
			actor.NotAfter = time.Now().Add(4 * time.Hour)
			if _, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("reset = %v, want failed precondition", err)
			}
			unchanged()
		})
	}
}

func TestCloudResetRejectsFutureDeletion(t *testing.T) {
	svc, req, ctx := cloudResetFixture(t)
	unchanged := checkResetStateUnchanged(t, svc)
	var deleted cloudpbv2.DeletedAsset
	if err := proto.Unmarshal(req.CloudDeletion, &deleted); err != nil {
		t.Fatal(err)
	}
	deleted.DeletedAt = timestamppb.New(time.Now().Add(time.Hour))
	var err error
	req.CloudDeletion, err = proto.Marshal(&deleted)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("reset = %v, want failed precondition", err)
	}
	unchanged()
}

func TestCloudResetCanceledRequestRetainsKeys(t *testing.T) {
	svc, req, ctx := cloudResetFixture(t)
	unchanged := checkResetStateUnchanged(t, svc)
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req); status.Code(err) != codes.Canceled {
		t.Fatalf("reset = %v, want canceled", err)
	}
	unchanged()
}

func TestCloudResetRecordsAuthenticatedAuthorizationTime(t *testing.T) {
	svc, req, ctx := cloudResetFixture(t)
	// Place the authenticated bound at a second boundary and age the sample
	// without sleeping. Verification must advance it, including on non-Linux
	// hosts whose fallback boot clock starts when the test process starts.
	latest := time.Now().Add(10 * time.Second).Truncate(time.Second).Add(time.Second - time.Nanosecond)
	var sample timesync.TimeWindow
	var minimumElapsed time.Duration
	svc.trustedTime = func(context.Context) (timesync.TimeWindow, error) {
		sample = anchored(timesync.TimeWindow{Earliest: latest.Add(-10 * time.Second), Latest: latest})
		minimumElapsed = time.Duration(min(sample.Boot/2, int64(time.Second)))
		sample.Boot -= int64(minimumElapsed)
		return sample, nil
	}
	response, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	// ReadCompletion also verifies the signed OCSP evidence at the recorded
	// authorization time, as restart recovery does after erasing the keys.
	completion, _, err := unenrollproof.ReadCompletion(response.GetUnenrollmentCompletion())
	if err != nil {
		t.Fatal(err)
	}
	boot, err := timesync.BootTime()
	if err != nil {
		t.Fatal(err)
	}
	authorized := time.Unix(0, completion.AuthorizedAtNanos)
	lower, upper := latest.Add(minimumElapsed), latest.Add(time.Duration(boot-sample.Boot))
	if authorized.Before(lower) || authorized.After(upper) || completion.AuthorizedAt != authorized.Unix() {
		t.Fatalf("authorization time = %s (%d seconds), want advanced authenticated upper bound in [%s, %s]", authorized, completion.AuthorizedAt, lower, upper)
	}
}

func TestCloudResetFreshTimeFailureRetainsKeys(t *testing.T) {
	for _, name := range []string{"missing", "unavailable", "empty", "reversed"} {
		t.Run(name, func(t *testing.T) {
			svc, req, ctx := cloudResetFixture(t)
			unchanged := checkResetStateUnchanged(t, svc)
			svc.trustedTime = nil
			if name != "missing" {
				svc.trustedTime = func(context.Context) (timesync.TimeWindow, error) {
					switch name {
					case "unavailable":
						return timesync.TimeWindow{}, errors.New("offline")
					case "reversed":
						return timesync.TimeWindow{Earliest: time.Now(), Latest: time.Now().Add(-time.Hour)}, nil
					default:
						return timesync.TimeWindow{}, nil
					}
				}
			}
			if _, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("reset = %v, want failed precondition", err)
			}
			unchanged()
		})
	}
}

func TestCloudResetAuthenticatedIntervalRejectsPartialValidity(t *testing.T) {
	for _, name := range []string{"operator-start", "operator-end", "ocsp-start", "ocsp-end"} {
		t.Run(name, func(t *testing.T) {
			svc, req, ctx := cloudResetFixture(t)
			unchanged := checkResetStateUnchanged(t, svc)
			now := time.Now()
			p, _ := peer.FromContext(ctx)
			actor := p.AuthInfo.(credentials.TLSInfo).State.PeerCertificates[0]
			window := timesync.TimeWindow{Earliest: now.Add(-10 * time.Second), Latest: now.Add(10 * time.Second)}
			want := codes.PermissionDenied
			switch name {
			case "operator-start":
				actor.NotBefore = now
			case "operator-end":
				actor.NotAfter = now
			case "ocsp-start", "ocsp-end":
				actor.NotBefore, actor.NotAfter = now.Add(-4*time.Hour), now.Add(4*time.Hour)
				want = codes.FailedPrecondition
				if name == "ocsp-start" {
					window.Earliest = now.Add(-2 * time.Minute)
				} else {
					window.Latest = now.Add(2 * time.Hour)
				}
			}
			svc.trustedTime = func(context.Context) (timesync.TimeWindow, error) { return anchored(window), nil }
			if _, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req); status.Code(err) != want {
				t.Fatalf("reset = %v, want %v", err, want)
			}
			unchanged()
		})
	}
}

func TestCloudResetFreshTimeDoesNotUseWallClock(t *testing.T) {
	for _, offset := range []time.Duration{-2 * time.Hour, 2 * time.Hour} {
		t.Run(offset.String(), func(t *testing.T) {
			now := time.Now().Add(offset)
			svc, req, ctx := cloudResetFixtureAt(t, now)
			renewFixtureAuthorization(t, svc, req, ctx, now)
			svc.trustedTime = func(context.Context) (timesync.TimeWindow, error) {
				return anchored(timesync.TimeWindow{Earliest: now, Latest: now}), nil
			}
			unchanged := checkResetStateUnchanged(t, svc)
			_, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req)
			if offset < 0 {
				// ocsp.CreateResponse stamps ProducedAt with the actual wall time.
				// It is future evidence relative to these signed bounds, despite
				// the operator being valid there: do not relax that check.
				if status.Code(err) != codes.FailedPrecondition {
					t.Fatalf("future ProducedAt accepted against signed past time: %v", err)
				}
				unchanged()
			} else if err != nil {
				t.Fatalf("signed time must win over wall-clock skew: %v", err)
			}
		})
	}
}

func TestCloudResetRechecksIdentityAfterFreshTime(t *testing.T) {
	svc, req, ctx := cloudResetFixture(t)
	unchanged := checkResetStateUnchanged(t, svc)
	svc.trustedTime = func(context.Context) (timesync.TimeWindow, error) {
		svc.mu.Lock() // also proves the network callback does not hold this lock
		svc.certPEM = "rotated identity"
		svc.mu.Unlock()
		now := time.Now()
		return anchored(timesync.TimeWindow{Earliest: now, Latest: now}), nil
	}
	if _, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("reset = %v, want failed precondition", err)
	}
	unchanged()
}

func TestCloudResetDoesNotUseOrWriteClockFloor(t *testing.T) {
	for _, name := range []string{"absent", "malformed", "future", "directory"} {
		t.Run(name, func(t *testing.T) {
			svc, req, ctx := cloudResetFixture(t)
			path := filepath.Join(svc.configPath, "clock_floor")
			var original []byte
			switch name {
			case "malformed":
				original = []byte("invalid clock floor")
			case "future":
				original = make([]byte, 8)
				binary.BigEndian.PutUint64(original, uint64(time.Now().Add(24*time.Hour).Unix()))
			case "directory":
				if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if original != nil {
				if err := os.WriteFile(path, original, 0400); err != nil {
					t.Fatal(err)
				}
			}
			response, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := unenrollproof.ReadCompletion(response.GetUnenrollmentCompletion()); err != nil {
				t.Fatal(err)
			}
			if original != nil {
				got, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(got, original) {
					t.Fatalf("clock floor changed: %v", err)
				}
			} else if name == "directory" {
				info, err := os.Stat(path)
				if err != nil || !info.IsDir() {
					t.Fatalf("clock directory replaced: %v", err)
				}
				entries, err := os.ReadDir(path)
				if err != nil || len(entries) != 0 {
					t.Fatalf("clock directory changed: %v", err)
				}
			} else if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("reset created clock floor: %v", err)
			}
			entries, err := os.ReadDir(svc.configPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if entry.Name() != "provisioning.json" && entry.Name() != "clock_floor" {
					t.Fatalf("unexpected persistent file after reset: %s", entry.Name())
				}
			}
		})
	}
}
