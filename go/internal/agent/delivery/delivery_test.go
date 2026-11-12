package delivery_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/delivery"
	"github.com/wendylabsinc/wendy/go/internal/agent/interceptor"
	"github.com/wendylabsinc/wendy/go/internal/shared/builddelivery"
	"github.com/wendylabsinc/wendy/go/internal/testutil/deliverypki"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

func TestResetClearsGrantsAndAllowsEnrollmentInSameProcess(t *testing.T) {
	fixture := deliverypki.New(t)
	leaf := fixture.Target.PEM
	manager := delivery.NewManager(filepath.Join(t.TempDir(), "grants"), "localhost:5000/", func() (string, string) {
		return leaf, fixture.Chain
	})
	t.Cleanup(manager.Close)

	claims := fixture.Claims("app:latest")
	id := fixture.Admit(t, manager, claims)
	previous, err := manager.Get()
	if err != nil {
		t.Fatal(err)
	}
	leaf = ""
	if err := manager.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, err := previous.Store.Policy(id, fixture.Builder.Leaf); err == nil {
		t.Fatal("previous enrollment still authorizes delivery")
	}
	if current, err := manager.Get(); err != nil || current.Provisioned || current.Store != nil {
		t.Fatalf("unenrolled snapshot: %+v, %v", current, err)
	}
	leaf = fixture.Target.PEM
	current, err := manager.Get()
	if err != nil || current.Store == nil {
		t.Fatalf("could not open store after enrollment: %v", err)
	}
	if _, err := current.Store.Policy(id, fixture.Builder.Leaf); err == nil {
		t.Fatal("fresh enrollment inherited an old admission")
	}
	fixture.Admit(t, manager, fixture.Claims("app:latest"))
}

func TestImageDeletionSerializesWithPublication(t *testing.T) {
	fixture := deliverypki.New(t)
	manager := fixture.Manager(t)
	claims := fixture.Claims("app:latest")
	claims.ExpectedDigest = "sha256:" + strings.Repeat("a", 64)
	session := fixture.Admit(t, manager, claims)
	output := "sha256:" + strings.Repeat("b", 64)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(builddelivery.MetadataKey, session))
	ctx = peer.NewContext(ctx, &peer.Peer{AuthInfo: credentials.TLSInfo{
		State: tls.ConnectionState{HandshakeComplete: true, PeerCertificates: []*x509.Certificate{fixture.Builder.Leaf}},
	}})
	ctx, _, err := delivery.RPC(ctx, manager.Get, interceptor.OrgModeStrict, "localhost:5000/app:latest")
	if err != nil {
		t.Fatal(err)
	}

	current := claims.ExpectedDigest
	comparing, publish := make(chan struct{}), make(chan struct{})
	published := make(chan error, 1)
	go func() {
		published <- delivery.Commit(ctx, manager.Get, "localhost:5000/app:latest", output, func() (string, error) {
			close(comparing)
			select {
			case <-publish:
				return current, nil
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}, func() error {
			current = output
			return nil
		})
	}()
	select {
	case <-comparing:
	case <-ctx.Done():
		t.Fatal("publication never reached comparison")
	}
	deleting, deleted := make(chan struct{}), make(chan error, 1)
	go func() {
		deleted <- delivery.Direct(func() (delivery.Snapshot, error) {
			close(deleting)
			return manager.Get()
		}, func() error {
			current = ""
			return nil
		})
	}()
	<-deleting
	select {
	case err := <-deleted:
		t.Fatalf("operator deletion raced ahead of publication: %v", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(publish)
	if err := <-published; err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
	if current != "" {
		t.Fatalf("image remained after deletion: %s", current)
	}
}

func TestDeliveryUsesLegacyOperatorGracePolicy(t *testing.T) {
	for _, tc := range []struct {
		name       string
		commonName string
		mode       interceptor.OrgMode
		want       codes.Code
	}{
		{"recognized user in grace mode", "wendy/user/alice", interceptor.OrgModeGrace, codes.OK},
		{"recognized user in strict mode", "wendy/user/alice", interceptor.OrgModeStrict, codes.PermissionDenied},
		{"unknown caller in grace mode", "unknown", interceptor.OrgModeGrace, codes.PermissionDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{
				State: tls.ConnectionState{
					HandshakeComplete: true,
					PeerCertificates: []*x509.Certificate{
						{Subject: pkix.Name{CommonName: tc.commonName}},
					},
				},
			}})
			_, grant, err := delivery.RPC(ctx, nil, tc.mode, "localhost:5000/app:latest")
			if grant != nil || status.Code(err) != tc.want {
				t.Fatalf("grant = %v, error = %v, want %v", grant, err, tc.want)
			}
		})
	}
}
