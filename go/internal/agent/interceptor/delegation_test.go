package interceptor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"net/url"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/delegation"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const delegationDevice = "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/robot"
const delegationMethod = "/wendy.agent.services.v2.WendyContainerService/StartContainer"

func delegationLeaf(t *testing.T) *x509.Certificate {
	t.Helper()
	owner := "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/user"
	raw := func(s string) asn1.RawValue { return asn1.RawValue{Tag: asn1.TagUTF8String, Bytes: []byte(s)} }
	wire := struct {
		Version       int
		ID            string `asn1:"utf8"`
		Owner         string `asn1:"utf8"`
		Devices, Apps []asn1.RawValue
	}{1, "22222222-2222-4222-8222-222222222222", owner, []asn1.RawValue{raw(delegationDevice)}, []asn1.RawValue{raw("demo")}}
	scope, err := asn1.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	ent, _ := asn1.Marshal([]asn1.RawValue{raw("entitlement:wendy.agent.services.v2.WendyContainerService:StartContainer:allow")})
	u, _ := url.Parse(owner)
	return &x509.Certificate{URIs: []*url.URL{u}, Extensions: []pkix.Extension{{Id: delegation.ScopeOID, Critical: true, Value: scope}, {Id: delegation.EntitlementsOID, Value: ent}}, UnhandledCriticalExtensions: []asn1.ObjectIdentifier{delegation.ScopeOID}}
}
func delegationContext(leaf *x509.Certificate) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}}})
}

type appRequest struct{ name string }

func (a *appRequest) GetAppName() string { return a.name }
func TestUnaryDelegationProtectsHandler(t *testing.T) {
	ctx := delegationContext(delegationLeaf(t))
	for _, app := range []string{"demo", "other", ""} {
		t.Run(app, func(t *testing.T) {
			called := false
			_, err := UnaryDelegationInterceptor(delegationDevice)(ctx, &appRequest{app}, &grpc.UnaryServerInfo{FullMethod: delegationMethod}, func(context.Context, any) (any, error) { called = true; return nil, nil })
			if called != (app == "demo") {
				t.Fatalf("handler called=%v", called)
			}
			if app != "demo" && status.Code(err) != codes.PermissionDenied {
				t.Fatalf("got %v", err)
			}
		})
	}
}

type fakeDelegationStream struct {
	grpc.ServerStream
	ctx  context.Context
	app  string
	sent bool
}

func (f *fakeDelegationStream) Context() context.Context { return f.ctx }
func (f *fakeDelegationStream) RecvMsg(m any) error      { m.(*appRequest).name = f.app; return nil }
func (f *fakeDelegationStream) SendMsg(any) error        { f.sent = true; return nil }
func TestStreamDelegationChecksBeforeDelivery(t *testing.T) {
	for _, app := range []string{"demo", "other"} {
		t.Run(app, func(t *testing.T) {
			f := &fakeDelegationStream{ctx: delegationContext(delegationLeaf(t)), app: app}
			delivered := false
			err := StreamDelegationInterceptor(delegationDevice)(nil, f, &grpc.StreamServerInfo{FullMethod: delegationMethod}, func(_ any, s grpc.ServerStream) error {
				if status.Code(s.SendMsg(nil)) != codes.PermissionDenied {
					t.Fatal("sent before request validation")
				}
				req := &appRequest{}
				if err := s.RecvMsg(req); err != nil {
					return err
				}
				delivered = true
				return s.SendMsg(nil)
			})
			if delivered != (app == "demo") || f.sent != (app == "demo") {
				t.Fatal("scope did not guard delivery")
			}
			if app == "other" && status.Code(err) != codes.PermissionDenied {
				t.Fatalf("got %v", err)
			}
		})
	}
}
func TestCriticalScopeAcknowledgedOnlyAfterValidation(t *testing.T) {
	leaf := delegationLeaf(t)
	other := asn1.ObjectIdentifier{1, 2, 3, 4}
	leaf.UnhandledCriticalExtensions = append(leaf.UnhandledCriticalExtensions, other)
	checked, err := DelegatedCertificateForVerification(leaf)
	if err != nil {
		t.Fatal(err)
	}
	if len(checked.UnhandledCriticalExtensions) != 1 || !checked.UnhandledCriticalExtensions[0].Equal(other) {
		t.Fatal("cleared unrelated critical constraint")
	}
	if len(leaf.UnhandledCriticalExtensions) != 2 {
		t.Fatal("mutated caller certificate")
	}
	leaf.Extensions[0].Value = []byte{0}
	if _, err := DelegatedCertificateForVerification(leaf); err == nil {
		t.Fatal("acknowledged malformed scope")
	}
}
