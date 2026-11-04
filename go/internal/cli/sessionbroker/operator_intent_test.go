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
		recipe := map[string]any{"version": 1, "principal": principal, "routerAddress": "192.0.2.10:43024", "routerUuid": "22222222-2222-4222-8222-222222222222"}
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
	if *launches != 1 || selected.Operator == nil || selected.Operator.Version != 2 || !selected.Operator.Enabled {
		t.Fatalf("explicit automatic connect reused pinned selection: launches=%d spec=%+v", *launches, selected.Operator)
	}
	recipe, err := readOperatorRecipe(operatorRecipePath(directory, selected.Operator.Principal))
	if err != nil || recipe.Version != 2 || !recipe.Enabled {
		t.Fatalf("saved automatic intent: %+v %v", recipe, err)
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
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "legacy"}[legacy], func(t *testing.T) {
			certificate, directory, launches, selected, stopped := operatorIntentFixture(t, legacy)
			if err := DisconnectOperator(context.Background()); err != nil {
				t.Fatal(err)
			}
			_, err := EnableOperatorAutomatic(context.Background(), certificate)
			if !errors.Is(err, stopped) || *launches != 1 {
				t.Fatalf("explicit connect did not launch: %d %v", *launches, err)
			}
			if selected.Operator.Version != 2 || selected.Operator.Disconnected || !selected.Operator.Enabled {
				t.Fatalf("resume intent: %+v", selected.Operator)
			}
			if _, err := os.Stat(operatorRecipePath(directory, selected.Operator.Principal)); err != nil {
				t.Fatal("selected intent not persisted", err)
			}
		})
	}
}

func TestLegacyDisconnectedRecipeDoesNotRejoin(t *testing.T) {
	certificate, directory, launches, _, _ := operatorIntentFixture(t, true)
	operator, err := operatorPrincipal(certificate)
	if err != nil {
		t.Fatal(err)
	}
	path := operatorRecipePath(directory, operator.Principal())
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	legacy["disconnected"] = true
	data, _ = json.Marshal(legacy)
	if err := writePrivateFile(path, data); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareOperatorAutomatic(context.Background(), certificate); !errors.Is(err, errOperatorDisconnected) {
		t.Fatal("legacy stop intent lost", err)
	}
	if *launches != 0 {
		t.Fatal("legacy disconnected recipe launched a broker")
	}
}
