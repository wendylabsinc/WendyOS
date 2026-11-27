package services

import (
	"context"
	"crypto/mldsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/appcgroup"
	"github.com/wendylabsinc/wendy/go/internal/shared/appconfig"
	sharedenv "github.com/wendylabsinc/wendy/go/internal/shared/env"
	"github.com/wendylabsinc/wendy/go/world/production/agentmodule"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/wendycert"
	"go.uber.org/zap"
	"strings"
)

func TestWorldSubscriptionsKeepDeviceQualification(t *testing.T) {
	device := "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/device/22222222-2222-4222-8222-222222222222"
	input := []appconfig.WorldSubscription{
		{App: "com.example.gas", Service: "sensor", ComponentType: "wendy.sensor.scalar.v1", Stream: "gas"},
		{App: "com.example.gas", Service: "sensor", ComponentType: "wendy.sensor.scalar.v1", Stream: "gas", Device: device},
	}
	want := []serviceaccess.Source{
		{App: "com.example.gas", Service: "sensor", ComponentType: "wendy.sensor.scalar.v1", Stream: "gas"},
		{App: "com.example.gas", Service: "sensor", ComponentType: "wendy.sensor.scalar.v1", Stream: "gas", Device: device},
	}
	if got := worldSubscriptions(input); !reflect.DeepEqual(got, want) {
		t.Fatalf("qualified subscriptions changed: %+v", got)
	}
}

func TestWorldExactServiceCgroup(t *testing.T) {
	s := serviceorigin.Scope{App: "com.example.sensor", Service: "camera", DeploymentGeneration: 42}
	exact := "wendy-agent-com.example.sensor@camera.world-000000000000002a.scope"
	for name, tc := range map[string]struct {
		body string
		pass bool
	}{
		"exact":                      {"0::/system.slice/" + exact, true},
		"cgroupfs exact":             {"0::/system.slice:wendy-agent:com.example.sensor@camera.world-000000000000002a", true},
		"cgroupfs sibling":           {"0::/system.slice:wendy-agent:com.example.sensor@gas.world-000000000000002a", false},
		"cgroupfs old generation":    {"0::/system.slice:wendy-agent:com.example.sensor@camera.world-0000000000000029", false},
		"mixed conflicting ancestry": {"0::/system.slice:wendy-agent:other@camera.world-000000000000002a/" + exact, false},
		"v1 controllers":             {"1:cpu:/system.slice/" + exact + "\n2:memory:/system.slice/" + exact, true},
		"service sibling":            {"0::/system.slice/wendy-agent-com.example.sensor@gas.world-000000000000002a.scope", false},
		"previous deployment":        {"0::/system.slice/wendy-agent-com.example.sensor@camera.world-0000000000000029.scope", false},
		"app only":                   {"0::/system.slice/wendy-agent-com.example.sensor.scope", false},
		"prefix spoof":               {"0::/system.slice/other-" + exact, false},
		"ambiguous ancestry":         {"0::/wendy-agent-other.scope/" + exact, false},
		"relative path":              {"0::" + exact, false},
		"empty":                      {"", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := matchWorldCgroup(tc.body, "wendy-agent", s) == nil; got != tc.pass {
				t.Fatalf("match=%v want=%v", got, tc.pass)
			}
		})
	}
}

func TestWorldProvisioningSnapshotAndInvalidation(t *testing.T) {
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	tenant := "11111111-1111-4111-8111-111111111111"
	principal := "spiffe://wendy.sh/tenant/" + tenant + "/device/22222222-2222-4222-8222-222222222222"
	key, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	rootKey, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "UNTRUSTED WORLD TEST ROOT"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, rootKey.Public(), rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err = x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(principal)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "UNTRUSTED WORLD TEST DEVICE"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Minute), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, URIs: []*url.URL{u}}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, root, key.Public(), rootKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	s := NewProvisioningService(zap.NewNop(), t.TempDir())
	i := worldIdentity{s}
	if _, err = i.Snapshot(context.Background()); err == nil {
		t.Fatal("unenrolled snapshot accepted")
	}
	s.mu.Lock()
	s.enrolled = true
	s.certPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}))
	s.keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	s.mu.Unlock()
	snap, err := i.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := wendycert.NewDevice(wendycert.Config{Tenant: tenant, Roots: [][]byte{rootDER}})
	if err != nil {
		t.Fatal(err)
	}
	id, err := verifier.Verify(snap.ChainDER, now, now.Add(time.Millisecond))
	if err != nil || id.Principal != principal {
		t.Fatalf("snapshot certificate: %v", err)
	}
	message := []byte("world snapshot test")
	sig, err := snap.Key.Sign(nil, message, &mldsa.Options{})
	if err != nil || id.VerifySignature(message, sig) != nil {
		t.Fatal("snapshot key mismatch")
	}
	s.mu.Lock()
	s.invalidateWorldIdentityLocked()
	s.mu.Unlock()
	if i.Current(snap.Token) {
		t.Fatal("rotated identity remained current")
	}
	select {
	case <-snap.Invalidated:
	default:
		t.Fatal("identity signal not closed synchronously")
	}
	if err = i.WithCurrent(snap.Token, func() error { t.Fatal("stale identity committed"); return nil }); err == nil {
		t.Fatal("stale fence accepted")
	}
	s.mu.Lock()
	s.worldIdentityToken.Store(math.MaxUint64)
	s.invalidateWorldIdentityLocked()
	s.invalidateWorldIdentityLocked()
	s.mu.Unlock()
	if s.worldIdentityToken.Load() != 0 {
		t.Fatal("identity generation overflow reused a token")
	}
}

func TestWorldIdentityCommitOrdersRotation(t *testing.T) {
	s := NewProvisioningService(zap.NewNop(), t.TempDir())
	s.enrolled = true
	i := worldIdentity{s}
	token := s.worldIdentityToken.Load()
	entered, release, committed := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		committed <- i.WithCurrent(token, func() error {
			if !i.Current(token) {
				return agentmodule.ErrDenied
			}
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("reentrant Current deadlocked")
	}
	rotated := make(chan struct{})
	go func() { s.mu.Lock(); s.invalidateWorldIdentityLocked(); s.mu.Unlock(); close(rotated) }()
	select {
	case <-rotated:
		t.Fatal("rotation crossed an active commit")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-committed; err != nil {
		t.Fatal(err)
	}
	select {
	case <-rotated:
	case <-time.After(time.Second):
		t.Fatal("rotation did not finish")
	}
	if i.Current(token) {
		t.Fatal("rotation retained old token")
	}
}

// Current deployments run below the shared app memory budget. The World
// attribution gate must accept that path without accepting sibling services.
func TestWorldCgroupPreservesCurrentAppBudget(t *testing.T) {
	scope := serviceorigin.Scope{App: "com.example.sensor", Service: "camera", DeploymentGeneration: 42}
	suffix, err := appconfig.WorldCgroupSuffix(scope.App, scope.Service, scope.DeploymentGeneration)
	if err != nil {
		t.Fatal(err)
	}
	path := appcgroup.ContainerPath(suffix, "", true)
	if !strings.HasPrefix(path, "/"+appcgroup.Parent+"/") {
		t.Fatalf("escaped app budget: %s", path)
	}
	if err := matchWorldCgroup("0::"+path, sharedenv.SystemdServiceName(), scope); err != nil {
		t.Fatal(err)
	}
	scope.Service = "gas"
	if matchWorldCgroup("0::"+path, sharedenv.SystemdServiceName(), scope) == nil {
		t.Fatal("camera attributed to sibling service")
	}
	scope.Service = "camera"
	scope.DeploymentGeneration++
	if matchWorldCgroup("0::"+path, sharedenv.SystemdServiceName(), scope) == nil {
		t.Fatal("old deployment attributed to replacement")
	}
}

func TestWorldManagerAbsentConfigurationLeavesAgentUnchanged(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewAppWorldSocketManager(t.Context(), filepath.Join(directory, "world.json"), nil)
	if err != nil || manager != nil {
		t.Fatalf("absent configuration activated World: manager=%v error=%v", manager, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		t.Fatalf("absent configuration created state: entries=%v error=%v", entries, err)
	}
}

func TestWorldManagerInvalidConfigurationDoesNotCreateAuthority(t *testing.T) {
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "world.json")
	if err := os.WriteFile(path, []byte(`{"unexpected":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewAppWorldSocketManager(t.Context(), path, nil)
	if err == nil || manager != nil {
		t.Fatalf("invalid configuration activated World: manager=%v error=%v", manager, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 1 {
		t.Fatalf("invalid configuration created state: entries=%v error=%v", entries, err)
	}
}
