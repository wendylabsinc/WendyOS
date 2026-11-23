package interceptor

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	_ "github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	agentpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/agentpb/v2"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

// Every agent call has exactly one role, and the table names no call that
// does not exist. A new agent RPC fails this test until it gets a role.
func TestEveryAgentMethodHasARole(t *testing.T) {
	agentMethods := map[string]bool{}
	protoregistry.GlobalFiles.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		if !strings.HasPrefix(string(fd.Package()), "wendy.agent.services.") {
			return true
		}
		for i := 0; i < fd.Services().Len(); i++ {
			s := fd.Services().Get(i)
			for j := 0; j < s.Methods().Len(); j++ {
				agentMethods[fmt.Sprintf("/%s/%s", s.FullName(), s.Methods().Get(j).Name())] = true
			}
		}
		return true
	})
	if len(agentMethods) == 0 {
		t.Fatal("no agent methods registered; the test would pass vacuously")
	}
	for m := range agentMethods {
		if !mapped(m) {
			t.Errorf("%s has no role in methodRoles (access_table.go): give it owner, operator, viewer or anyone", m)
		}
	}
	for m := range methodRoles {
		if !agentMethods[m] {
			t.Errorf("methodRoles names %s, which no agent proto defines", m)
		}
	}
}

// The keyless set is the fixed 20-call deploy-and-manage-apps set, never the
// host shell nor anything above operator.
func TestKeylessSetIsTheAppsSet(t *testing.T) {
	if len(keylessMethods) != 20 {
		t.Fatalf("keyless set has %d calls, want the 20 of the apps set", len(keylessMethods))
	}
	for m := range keylessMethods {
		role, ok := methodRoles[m]
		if !ok || role > operator {
			t.Errorf("keyless %s: role %v mapped %v", m, role, ok)
		}
	}
	if methodRoles["/wendy.agent.services.v1.WendyShellService/HostShell"] != owner {
		t.Error("host shell is not owner only")
	}
}

func spkiOf(t *testing.T) []byte {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(k.Public())
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func keyCtx(spki []byte) context.Context {
	return peer.NewContext(context.Background(), &peer.Peer{AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{{RawSubjectPublicKeyInfo: spki}},
	}}})
}

func TestClosedAuthorizer(t *testing.T) {
	keys, err := trustedkeys.Open(filepath.Join(t.TempDir(), trustedkeys.FileName))
	if err != nil {
		t.Fatal(err)
	}
	ownerKey, opKey, viewKey, strangerKey := spkiOf(t), spkiOf(t), spkiOf(t), spkiOf(t)
	o, err := keys.Close(ownerKey, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Add(opKey, trustedkeys.RoleOperator, "", o.Fingerprint(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := keys.Add(viewKey, trustedkeys.RoleViewer, "", o.Fingerprint(), time.Now()); err != nil {
		t.Fatal(err)
	}

	const (
		listApps  = "/wendy.agent.services.v1.WendyContainerService/ListContainers"
		startApp  = "/wendy.agent.services.v1.WendyContainerService/StartContainer"
		wifi      = "/wendy.agent.services.v2.WendyWiFiService/ConnectToWiFi"
		shell     = "/wendy.agent.services.v1.WendyShellService/HostShell"
		addKey    = agentpbv2.WendyAccessService_AddTrustedKey_FullMethodName
		getAccess = agentpbv2.WendyAccessService_GetAccess_FullMethodName
		enroll    = "/wendy.agent.services.v2.WendyProvisioningService/StartACMEProvisioning"
		unknown   = "/wendy.agent.services.v2.Nope/Nope"
	)
	hook := func(ctx context.Context) context.Context {
		return metadata.NewIncomingContext(ctx, metadata.Pairs(appconfig.PostStartAgentHookMetadataKey, "id"))
	}
	keyAuthz := &ClosedAuthorizer{Logger: zap.NewNop(), Listener: ClosedKeyListener, Keys: keys}
	keyless := &ClosedAuthorizer{Logger: zap.NewNop(), Listener: ClosedKeylessListener, Keys: keys}
	socket := &ClosedAuthorizer{Logger: zap.NewNop(), Listener: ClosedAdminSocket, Keys: keys}

	type tc struct {
		name   string
		a      *ClosedAuthorizer
		ctx    context.Context
		method string
		ok     bool
	}
	run := func(cases []tc) {
		t.Helper()
		for _, c := range cases {
			_, err := c.a.authorize(c.ctx, c.method)
			if (err == nil) != c.ok {
				t.Errorf("%s: err=%v, want allowed=%v", c.name, err, c.ok)
			}
			if err != nil && status.Code(err) != codes.PermissionDenied {
				t.Errorf("%s: code %v, want PermissionDenied", c.name, status.Code(err))
			}
		}
	}
	bg := context.Background()
	run([]tc{
		{"viewer reads", keyAuthz, keyCtx(viewKey), listApps, true},
		{"viewer cannot start apps", keyAuthz, keyCtx(viewKey), startApp, false},
		{"operator starts apps", keyAuthz, keyCtx(opKey), startApp, true},
		{"operator changes wifi", keyAuthz, keyCtx(opKey), wifi, true},
		{"operator has no shell", keyAuthz, keyCtx(opKey), shell, false},
		{"operator cannot manage keys", keyAuthz, keyCtx(opKey), addKey, false},
		{"operator cannot enroll", keyAuthz, keyCtx(opKey), enroll, false},
		{"operator cannot send the root hook", keyAuthz, hook(keyCtx(opKey)), startApp, false},
		{"owner shell", keyAuthz, keyCtx(ownerKey), shell, true},
		{"owner manages keys", keyAuthz, keyCtx(ownerKey), addKey, true},
		{"owner enrolls", keyAuthz, keyCtx(ownerKey), enroll, true},
		{"owner sends the root hook", keyAuthz, hook(keyCtx(ownerKey)), startApp, true},
		{"untrusted key", keyAuthz, keyCtx(strangerKey), listApps, false},
		{"no peer", keyAuthz, bg, listApps, false},
		{"unknown method, owner", keyAuthz, keyCtx(ownerKey), unknown, false},
		{"keyless off: apps refused", keyless, bg, listApps, false},
		{"keyless: anyone reads the shape", keyless, bg, getAccess, true},
		{"admin socket is operator", socket, bg, startApp, true},
		{"admin socket cannot manage keys", socket, bg, addKey, false},
		{"admin socket cannot enroll", socket, bg, enroll, false},
	})

	if err := keys.SetKeylessApps(true, o.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	run([]tc{
		{"keyless on: apps set", keyless, bg, startApp, true},
		{"keyless on: outside the set", keyless, bg, wifi, false},
		{"keyless on: no shell", keyless, bg, shell, false},
		{"keyless on: no root hook", keyless, hook(bg), startApp, false},
	})
	ctx, err := keyless.authorize(bg, startApp)
	if err != nil || !trustedkeys.Keyless(ctx) {
		t.Fatalf("keyless call not marked keyless: %v", err)
	}
	ctx, err = keyAuthz.authorize(keyCtx(opKey), startApp)
	if err != nil || trustedkeys.Keyless(ctx) {
		t.Fatalf("keyed call marked keyless: %v", err)
	}
}
