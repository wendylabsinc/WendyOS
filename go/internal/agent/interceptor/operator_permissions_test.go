package interceptor

import (
	"context"
	"crypto/x509/pkix"
	"encoding/asn1"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	"google.golang.org/grpc/metadata"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/operatorpermission"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestOperatorRPCEnforcementAndAudit(t *testing.T) {
	leaf := rpcIdentityLeaf(t, "operator", "spiffe://wendy.sh/tenant/"+rpcTestTenant+"/operator/user")
	leaf.NotBefore = time.Now().Add(-time.Minute)
	leaf.NotAfter = time.Now().Add(time.Minute)
	ctx := rpcIdentityContext(leaf)
	for _, enforce := range []bool{true, false} {
		core, logs := observer.New(zap.WarnLevel)
		p := &operatorpermission.Policy{Enforce: enforce, Scope: certs.Scope{TenantUUID: rpcTestTenant}, Logger: zap.New(core)}
		for _, method := range []string{"/wendy.agent.services.v1.WendyShellService/HostShell", "/unknown.Service/Write"} {
			called := false
			_, err := UnaryOperatorPermissionInterceptor(p)(ctx, nil, &grpc.UnaryServerInfo{FullMethod: method}, func(context.Context, any) (any, error) { called = true; return nil, nil })
			if called == enforce || (err != nil) != enforce {
				t.Fatalf("enforce=%v called=%v err=%v", enforce, called, err)
			}
			called = false
			err = StreamOperatorPermissionInterceptor(p)(nil, rpcContextStream{ctx: ctx}, &grpc.StreamServerInfo{FullMethod: method}, func(any, grpc.ServerStream) error { called = true; return nil })
			if called == enforce || (err != nil) != enforce {
				t.Fatalf("stream enforce=%v called=%v err=%v", enforce, called, err)
			}
		}
		if logs.Len() != 4 {
			t.Fatalf("missing decision logs: %d", logs.Len())
		}
	}
}
func TestEntitledStreamEndsAtCertificateExpiry(t *testing.T) {
	leaf := rpcIdentityLeaf(t, "operator", "spiffe://wendy.sh/tenant/"+rpcTestTenant+"/operator/user")
	leaf.NotBefore = time.Now().Add(-time.Minute)
	leaf.NotAfter = time.Now().Add(50 * time.Millisecond)
	der, _ := asn1.Marshal([]asn1.RawValue{{Tag: 12, Bytes: []byte("entitlement:device:shell:allow")}})
	leaf.Extensions = []pkix.Extension{{Id: operatorpermission.OID, Value: der}}
	p := &operatorpermission.Policy{Enforce: true, Scope: certs.Scope{TenantUUID: rpcTestTenant}}
	canceled := make(chan struct{})
	err := StreamOperatorPermissionInterceptor(p)(nil, rpcContextStream{ctx: rpcIdentityContext(leaf)}, &grpc.StreamServerInfo{FullMethod: "/wendy.agent.services.v1.WendyShellService/HostShell"}, func(_ any, ss grpc.ServerStream) error {
		<-ss.Context().Done()
		close(canceled)
		return ss.Context().Err()
	})
	if status.Code(err) != codes.DeadlineExceeded && err != context.DeadlineExceeded {
		t.Fatalf("expiry=%v", err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("handler not canceled")
	}
}

func TestContainerControlCannotInjectHostHook(t *testing.T) {
	leaf := rpcIdentityLeaf(t, "operator", "spiffe://wendy.sh/tenant/"+rpcTestTenant+"/operator/user")
	leaf.NotBefore = time.Now().Add(-time.Minute)
	leaf.NotAfter = time.Now().Add(time.Minute)
	der, _ := asn1.Marshal([]asn1.RawValue{{Tag: 12, Bytes: []byte("entitlement:deployment:control-container:allow")}})
	leaf.Extensions = []pkix.Extension{{Id: operatorpermission.OID, Value: der}}
	p := &operatorpermission.Policy{Enforce: true, Scope: certs.Scope{TenantUUID: rpcTestTenant}}
	ctx := metadata.NewIncomingContext(rpcIdentityContext(leaf), metadata.Pairs(appconfig.PostStartAgentHookMetadataKey, "a-host-command"))
	if _, err := checkOperatorRPC(ctx, p, "/wendy.agent.services.v1.WendyContainerService/StartContainer"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("host hook bypass: %v", err)
	}
}
