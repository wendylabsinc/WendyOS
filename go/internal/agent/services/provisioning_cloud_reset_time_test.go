package services

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/timesync"
	"github.com/wendylabsinc/wendy/go/internal/shared/unenrollproof"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func TestCloudResetRetainsKeysWithoutFreshAuthenticatedTime(t *testing.T) {
	for _, name := range []string{"missing", "unavailable", "zero", "inverted", "canceled"} {
		t.Run(name, func(t *testing.T) {
			svc, req, ctx := cloudResetFixture(t)
			original, err := os.ReadFile(filepath.Join(svc.configPath, "provisioning.json"))
			if err != nil {
				t.Fatal(err)
			}
			window, _ := svc.trustedTime(ctx)
			switch name {
			case "missing":
				svc.trustedTime = nil
			case "unavailable":
				svc.trustedTime = func(context.Context) (timesync.TimeWindow, error) {
					return timesync.TimeWindow{}, errors.New("no authenticated consensus")
				}
			case "zero":
				svc.trustedTime = func(context.Context) (timesync.TimeWindow, error) { return timesync.TimeWindow{}, nil }
			case "inverted":
				svc.trustedTime = func(context.Context) (timesync.TimeWindow, error) {
					return timesync.TimeWindow{Earliest: window.Latest.Add(time.Second), Latest: window.Earliest}, nil
				}
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				svc.trustedTime = func(context.Context) (timesync.TimeWindow, error) {
					cancel()
					return window, nil
				}
			}
			if _, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("reset = %v, want failed precondition", err)
			}
			got, err := os.ReadFile(filepath.Join(svc.configPath, "provisioning.json"))
			if err != nil || string(got) != string(original) {
				t.Fatalf("authorization failure changed provisioning state: %v", err)
			}
			for _, name := range []string{"device-key.pem", "acme-account-key.pem"} {
				if _, err := os.Stat(filepath.Join(svc.configPath, name)); err != nil {
					t.Fatalf("time failure removed %s: %v", name, err)
				}
			}
		})
	}
}

func TestCloudResetChecksBothAuthenticatedTimeBounds(t *testing.T) {
	for _, bound := range []string{"earliest", "latest"} {
		t.Run(bound, func(t *testing.T) {
			svc, req, ctx := cloudResetFixture(t)
			window, _ := svc.trustedTime(ctx)
			// Keep operator validity broad so failure specifically exercises the
			// revocation proof's window rather than the actor's certificate.
			p, _ := peer.FromContext(ctx)
			actor := p.AuthInfo.(credentials.TLSInfo).State.PeerCertificates[0]
			actor.NotBefore = window.Earliest.Add(-4 * time.Hour)
			actor.NotAfter = window.Latest.Add(4 * time.Hour)
			if bound == "earliest" {
				window.Earliest = window.Earliest.Add(-2 * time.Hour)
			} else {
				window.Latest = window.Latest.Add(2 * time.Hour)
			}
			svc.trustedTime = func(context.Context) (timesync.TimeWindow, error) { return window, nil }
			if _, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req); status.Code(err) != codes.FailedPrecondition {
				t.Fatalf("accepted revocation evidence invalid at %s bound: %v", bound, err)
			}
			if _, err := os.Stat(filepath.Join(svc.configPath, "device-key.pem")); err != nil {
				t.Fatalf("uncertain time removed key: %v", err)
			}
		})
	}
}

func TestCloudResetRechecksIdentityAfterTimeQuery(t *testing.T) {
	svc, req, ctx := cloudResetFixture(t)
	window, _ := svc.trustedTime(ctx)
	svc.trustedTime = func(context.Context) (timesync.TimeWindow, error) {
		// This also proves network acquisition runs without the state lock.
		svc.mu.Lock()
		svc.principalURI = "spiffe://wendy.sh/tenant/" + resetTenant + "/device/replacement"
		svc.mu.Unlock()
		return window, nil
	}
	if _, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("accepted changed identity: %v", err)
	}
	if _, err := os.Stat(filepath.Join(svc.configPath, "device-key.pem")); err != nil {
		t.Fatalf("replacement identity key removed: %v", err)
	}
}

func TestCloudResetChecksOperatorValidityAtAuthenticatedTime(t *testing.T) {
	svc, req, ctx := cloudResetFixture(t)
	window, _ := svc.trustedTime(ctx)
	p, _ := peer.FromContext(ctx)
	actor := p.AuthInfo.(credentials.TLSInfo).State.PeerCertificates[0]
	actor.NotAfter = window.Earliest.Add(-time.Second)
	if _, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("accepted expired operator: %v", err)
	}
	if _, err := os.Stat(filepath.Join(svc.configPath, "device-key.pem")); err != nil {
		t.Fatalf("expired operator removed key: %v", err)
	}
}

func TestCloudResetRecordsAuthenticatedAuthorizationTime(t *testing.T) {
	svc, req, ctx := cloudResetFixture(t)
	window, _ := svc.trustedTime(ctx)
	window.Earliest = window.Earliest.Add(30 * time.Second)
	window.Latest = window.Earliest.Add(time.Second)
	svc.trustedTime = func(context.Context) (timesync.TimeWindow, error) { return window, nil }
	response, err := NewProvisioningServiceV2(svc).Unprovision(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	completion, _, err := unenrollproof.ReadCompletion(response.GetUnenrollmentCompletion())
	if err != nil {
		t.Fatal(err)
	}
	if completion.AuthorizedAt != window.Latest.Unix() {
		t.Fatalf("authorization time = %d, want authenticated bound %d", completion.AuthorizedAt, window.Latest.Unix())
	}
}
