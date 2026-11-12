package services

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

type provisioningCrashFixture struct {
	State provisioningState
	Key   []byte
	Mode  string
	Phase string
}

// Exit, rather than return an error, so deferred cleanup cannot mask an
// interrupted migration or enrollment. Every recovery uses a new OS process.
func TestProvisioningProcessCrash(t *testing.T) {
	if path := os.Getenv("WENDY_PROVISIONING_CRASH_FIXTURE"); path != "" {
		runProvisioningCrashChild(t, path, os.Getenv("WENDY_PROVISIONING_CRASH_ACTION"))
		return
	}
	for _, mode := range []string{"enroll", "repair", "legacy"} {
		for _, phase := range []string{"before:device-key.pem", "after:device-key.pem", "before:provisioning.json", "after:provisioning.json"} {
			t.Run(mode+"/"+phase, func(t *testing.T) {
				identity := newPBFixture(t)
				fixture := provisioningCrashFixture{
					State: provisioningState{Enrolled: true, CloudHost: "fixture.invalid:443", OrgID: 7, AssetID: 42,
						PrincipalURI: identity.uri, CertPEM: identity.certPEM, ChainPEM: identity.chainPEM},
					Key: []byte(identity.keyPEM), Mode: mode, Phase: phase,
				}
				dir := t.TempDir()
				config := filepath.Join(dir, "config")
				if err := os.Mkdir(config, 0700); err != nil {
					t.Fatal(err)
				}
				if mode != "enroll" {
					svc := NewProvisioningService(zap.NewNop(), config)
					if _, err := svc.persistProvisioning(&fixture.State, fixture.Key); err != nil {
						t.Fatal(err)
					}
					if mode == "repair" {
						for _, name := range []string{"device-key.pem", "provisioning.json"} {
							if err := os.Chmod(filepath.Join(config, name), 0644); err != nil {
								t.Fatal(err)
							}
						}
					} else {
						legacy := fixture.State
						legacy.KeyPEM = string(fixture.Key)
						data, err := json.Marshal(legacy)
						if err != nil {
							t.Fatal(err)
						}
						if err := os.WriteFile(filepath.Join(config, "provisioning.json"), data, 0600); err != nil {
							t.Fatal(err)
						}
						if err := os.Remove(filepath.Join(config, "device-key.pem")); err != nil {
							t.Fatal(err)
						}
					}
				}
				data, err := json.Marshal(fixture)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "fixture.json")
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				for _, action := range []string{"crash", "recover", "verify"} {
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestProvisioningProcessCrash$", "-test.count=1")
					cmd.Env = append(os.Environ(), "WENDY_PROVISIONING_CRASH_FIXTURE="+path, "WENDY_PROVISIONING_CRASH_ACTION="+action)
					out, err := cmd.CombinedOutput()
					contextErr := ctx.Err()
					cancel()
					code := 0
					if err != nil {
						var exit *exec.ExitError
						if !errors.As(err, &exit) {
							t.Fatalf("child: %v: %s", err, out)
						}
						code = exit.ExitCode()
					}
					want := 0
					if action == "crash" {
						want = 73
					}
					if contextErr != nil || code != want {
						t.Fatalf("%s exit=%d want=%d context=%v: %s", action, code, want, contextErr, out)
					}
				}
			})
		}
	}
}

func runProvisioningCrashChild(t *testing.T, path, action string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fixture provisioningCrashFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(filepath.Dir(path), "config")
	if action == "crash" {
		provisioningWriteFile = func(path string, data []byte, perm os.FileMode) error {
			name := filepath.Base(path)
			if fixture.Phase == "before:"+name {
				os.Exit(73)
			}
			if err := syncWriteFile(path, data, perm); err != nil {
				return err
			}
			if fixture.Phase == "after:"+name {
				os.Exit(73)
			}
			return nil
		}
		svc := NewProvisioningService(zap.NewNop(), config)
		if fixture.Mode == "enroll" {
			if _, err := svc.persistProvisioning(&fixture.State, fixture.Key); err != nil {
				t.Fatal(err)
			}
		}
		t.Fatal("requested crash boundary was not reached")
	}

	// Check disk state before recovery can repair it. A committed enrollment
	// must retain a complete key, either in the dedicated file or legacy JSON.
	stateBytes, stateErr := os.ReadFile(filepath.Join(config, "provisioning.json"))
	if stateErr == nil {
		var state provisioningState
		if err := json.Unmarshal(stateBytes, &state); err != nil {
			t.Fatalf("torn enrollment JSON: %v", err)
		}
		key, _ := os.ReadFile(filepath.Join(config, "device-key.pem"))
		if !bytes.Equal(key, fixture.Key) && state.KeyPEM != string(fixture.Key) {
			t.Fatal("committed enrollment lost its private key")
		}
	} else if !os.IsNotExist(stateErr) {
		t.Fatal(stateErr)
	}
	svc := NewProvisioningService(zap.NewNop(), config)
	if action == "recover" && fixture.Mode == "enroll" && fixture.Phase != "after:provisioning.json" {
		if svc.enrolled || stateErr == nil {
			t.Fatal("interrupted initial enrollment falsely committed")
		}
		if _, err := svc.persistProvisioning(&fixture.State, fixture.Key); err != nil {
			t.Fatal(err)
		}
	}
	if !svc.enrolled || svc.cloudHost != fixture.State.CloudHost || svc.orgID != fixture.State.OrgID ||
		svc.assetID != fixture.State.AssetID || svc.principalURI != fixture.State.PrincipalURI ||
		svc.certPEM != fixture.State.CertPEM || svc.chainPEM != fixture.State.ChainPEM || !bytes.Equal(svc.keyPEM, fixture.Key) {
		t.Fatal("restarted agent changed or lost its enrolled identity")
	}
	before := map[string]os.FileInfo{}
	for _, name := range []string{"device-key.pem", "provisioning.json"} {
		info := provisioningStat(t, filepath.Join(config, name))
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			t.Fatalf("unrepaired %s: %v", name, info.Mode())
		}
		before[name] = info
	}
	stateBytes, err = os.ReadFile(filepath.Join(config, "provisioning.json"))
	if err != nil || strings.Contains(strings.ToLower(string(stateBytes)), "keypem") {
		t.Fatal("recovered JSON retained the private key")
	}
	NewProvisioningService(zap.NewNop(), config)
	for name, info := range before {
		assertProvisioningUnchanged(t, filepath.Join(config, name), info)
	}
	assertNoLegacyProvisioningFiles(t, config)
}
