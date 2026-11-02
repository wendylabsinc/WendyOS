//go:build !windows

package sessionbroker

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/cli/operatormesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// These cases stop at the detached-process boundary: no real profile, broker,
// network or radio is used. They exercise the same saved intent that the
// independent CLI processes use when resolving an explicit mesh selector.
func operatorIntentFixture(t *testing.T, pinned bool) (config.CertificateInfo, string, *int, *Spec, error) {
	t.Helper()
	directory := shortTempDir(t)
	principal := "spiffe://wendy.sh/tenant/11111111-1111-4111-8111-111111111111/operator/intent"
	leaf, _ := liteManagementCertificate(t, nil, nil, "operator intent", principal, false)
	certificate := config.CertificateInfo{PemCertificate: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leaf.Raw})), PrincipalURI: principal}
	previousDir, previousConfig, previousLaunch := configDir, loadConfig, launchOperatorBroker
	t.Cleanup(func() { configDir, loadConfig, launchOperatorBroker = previousDir, previousConfig, previousLaunch })
	configDir = func() (string, error) { return directory, nil }
	loadConfig = func() (*config.Config, error) {
		return &config.Config{CurrentContext: "intent", Auth: []config.AuthConfig{{Name: "intent", CloudGRPC: "fixture.invalid:443", Certificates: []config.CertificateInfo{certificate}}}}, nil
	}
	launches, selected := new(int), new(Spec)
	stopped := errors.New("fixture stops before detached broker launch")
	launchOperatorBroker = func(spec Spec) error { *launches++; *selected = spec; return stopped }
	if pinned {
		recipe := OperatorSpec{Version: 1, Principal: principal, RouterAddress: "192.0.2.10:43024", RouterUUID: "22222222-2222-4222-8222-222222222222"}
		path := operatorRecipePath(directory, principal)
		if err := ensurePrivateDir(filepath.Dir(path)); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(recipe)
		if err != nil {
			t.Fatal(err)
		}
		if err := writePrivateFile(path, data); err != nil {
			t.Fatal(err)
		}
	}
	return certificate, directory, launches, selected, stopped
}

func TestOperatorExplicitAutomaticDoesNotReuseSavedPinnedFirstHop(t *testing.T) {
	certificate, directory, launches, selected, stopped := operatorIntentFixture(t, true)
	if _, err := EnableOperatorAutomatic(context.Background(), certificate); !errors.Is(err, stopped) {
		t.Fatal(err)
	}
	if *launches != 1 || selected.Operator == nil || !selected.Operator.Automatic || selected.Operator.RouterAddress != "" || selected.Operator.RouterUUID != "" || !selected.Operator.Enabled {
		t.Fatalf("explicit automatic connect reused pinned selection: launches=%d spec=%+v", *launches, selected.Operator)
	}
	recipe, err := readOperatorRecipe(operatorRecipePath(directory, selected.Operator.Principal))
	if err != nil || !recipe.Automatic || recipe.RouterAddress != "" {
		t.Fatalf("saved automatic intent: %+v %v", recipe, err)
	}
}

func TestOperatorExplicitAutomaticDoesNotReplaceLivePinnedOwner(t *testing.T) {
	certificate, directory, launches, _, _ := operatorIntentFixture(t, true)
	operator, err := operatorPrincipal(certificate)
	if err != nil {
		t.Fatal(err)
	}
	path := operatorRecipePath(directory, operator.Principal())
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := brokerDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	socket, _ := paths(dir, operatorKey(operator.Principal()), certs.WendyIdentity{})
	owner, locked, err := acquireIdentityLock(socket)
	if err != nil || !locked {
		t.Fatal("fixture could not retain pinned owner", err)
	}
	defer func() { releaseLock(owner); _ = owner.Close() }()
	if _, err := EnableOperatorAutomatic(context.Background(), certificate); err == nil || !strings.Contains(err.Error(), "disconnect") {
		t.Fatal("live mode change did not require disconnect", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(before) != string(after) || *launches != 0 {
		t.Fatalf("live pinned context was changed: launches=%d err=%v", *launches, err)
	}
}

func TestOperatorDisconnectBlocksImplicitRejoin(t *testing.T) {
	certificate, _, launches, _, _ := operatorIntentFixture(t, true)
	if err := DisconnectOperator(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []struct {
		name string
		run  func() error
	}{
		{"mesh target", func() error {
			_, err := ConnectOperator(context.Background(), "mesh://11111111-1111-4111-8111-111111111111/22222222-2222-4222-8222-222222222222")
			return err
		}},
		{"passive automatic", func() error { _, err := PrepareOperatorAutomatic(context.Background(), certificate); return err }},
		{"directory discovery", func() error { _, err := prepareOperatorDirectory(context.Background()); return err }},
	} {
		t.Run(operation.name, func(t *testing.T) {
			if err := operation.run(); err == nil || !strings.Contains(err.Error(), "disconnected") || !strings.Contains(err.Error(), "wendy device wendy-net connect") {
				t.Fatalf("missing explicit reconnect guidance: %v", err)
			}
		})
	}
	if *launches != 0 {
		t.Fatalf("disconnect was undone by %d implicit launches", *launches)
	}
	if err := DisconnectOperator(context.Background()); err != nil {
		t.Fatalf("repeated disconnect: %v", err)
	}
}

func TestOperatorDisconnectBeforeFirstConnectDisablesImplicitStart(t *testing.T) {
	_, _, launches, _, _ := operatorIntentFixture(t, false)
	if err := DisconnectOperator(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := ConnectOperator(context.Background(), "mesh://11111111-1111-4111-8111-111111111111/22222222-2222-4222-8222-222222222222"); err == nil || !strings.Contains(err.Error(), "disconnected") {
		t.Fatal(err)
	}
	if *launches != 0 {
		t.Fatal("fresh disconnect still started a broker")
	}
}

func TestOperatorExplicitConnectResumesAfterDisconnect(t *testing.T) {
	for _, automatic := range []bool{false, true} {
		t.Run(map[bool]string{false: "pinned", true: "automatic"}[automatic], func(t *testing.T) {
			certificate, directory, launches, selected, stopped := operatorIntentFixture(t, true)
			if err := DisconnectOperator(context.Background()); err != nil {
				t.Fatal(err)
			}
			var err error
			if automatic {
				_, err = EnableOperatorAutomatic(context.Background(), certificate)
			} else {
				_, err = PrepareOperator(context.Background(), certificate, "192.0.2.20:43024", "33333333-3333-4333-8333-333333333333")
			}
			if !errors.Is(err, stopped) || *launches != 1 {
				t.Fatalf("explicit connect did not launch: %d %v", *launches, err)
			}
			if selected.Operator.Automatic != automatic {
				t.Fatal("explicit mode differs")
			}
			if _, err := os.Stat(operatorRecipePath(directory, selected.Operator.Principal)); err != nil {
				t.Fatal("selected intent not persisted", err)
			}
		})
	}
}

func TestOperatorAutomaticFallbackPreservesDiscoveryIntent(t *testing.T) {
	certificate, directory, _, _, _ := operatorIntentFixture(t, false)
	operator, err := operatorPrincipal(certificate)
	if err != nil {
		t.Fatal(err)
	}
	recipe := OperatorSpec{Version: 1, Principal: operator.Principal(), Automatic: true, Enabled: true}
	spec, err := operatorSpec(recipe, certificate)
	if err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateDir(filepath.Dir(operatorRecipePath(directory, operator.Principal()))); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(recipe)
	if err != nil {
		t.Fatal(err)
	}
	if err := writePrivateFile(operatorRecipePath(directory, operator.Principal()), data); err != nil {
		t.Fatal(err)
	}
	previous := openOperatorSession
	t.Cleanup(func() { openOperatorSession = previous })
	var selected operatormesh.Options
	stopped := errors.New("fixture stops before opening a fallback carrier")
	openOperatorSession = func(_ context.Context, options operatormesh.Options) (*operatormesh.Session, error) {
		selected = options
		return nil, stopped
	}
	// Discovery produces a concrete first-hop hint, not the retained context's
	// automatic/pinned policy. Its next generation must keep automatic sources.
	candidate := OperatorSpec{Version: 1, Principal: operator.Principal(), RouterAddress: "192.0.2.20:43024", RouterUUID: "33333333-3333-4333-8333-333333333333"}
	attachment := &operatorAttachment{directory: directory, spec: spec, certificate: certificate}
	if _, err := attachment.open(context.Background(), candidate); !errors.Is(err, stopped) {
		t.Fatal(err)
	}
	if !selected.Automatic || selected.Discover == nil || selected.RouterAddress != candidate.RouterAddress || selected.RouterUUID != candidate.RouterUUID {
		t.Fatal("fallback changed automatic policy or selected first hop")
	}
}
