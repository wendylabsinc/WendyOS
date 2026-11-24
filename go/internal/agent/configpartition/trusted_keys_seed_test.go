package configpartition

import (
	"bytes"
	"crypto/mldsa"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
)

func testSeed(t *testing.T) ([]byte, []byte) {
	t.Helper()
	k, err := mldsa.GenerateKey(mldsa.MLDSA65())
	if err != nil {
		t.Fatal(err)
	}
	spki, err := x509.MarshalPKIXPublicKey(k.Public())
	if err != nil {
		t.Fatal(err)
	}
	seed, err := trustedkeys.Seed(spki, "laptop", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return spki, seed
}

// The installer's seed becomes the first owner and the device starts Closed.
func TestApplyTrustedKeysSeed_DeviceStartsClosedWithTheSeededOwner(t *testing.T) {
	cfgDir := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "wendy-agent")
	spki, seed := testSeed(t)
	if err := os.WriteFile(filepath.Join(cfgDir, trustedKeysFile), seed, 0o644); err != nil { // FAT32: no modes
		t.Fatal(err)
	}

	if err := applyTrustedKeysSeed(zap.NewNop(), cfgDir, configPath); err != nil {
		t.Fatal(err)
	}

	store, err := trustedkeys.Open(filepath.Join(configPath, trustedkeys.FileName))
	if err != nil {
		t.Fatalf("agent would refuse the installed seed: %v", err)
	}
	if !store.Closed() || store.KeylessApps() {
		t.Fatalf("Closed=%v KeylessApps=%v, want Closed with keyless apps off", store.Closed(), store.KeylessApps())
	}
	if role, _ := store.Lookup(spki); role != trustedkeys.RoleOwner {
		t.Fatalf("seeded key role = %v, want owner", role)
	}
	if got := store.List()[0].AddedBy; got != trustedkeys.AddedByInstall {
		t.Errorf("added_by = %q, want %q", got, trustedkeys.AddedByInstall)
	}
	if _, err := os.Stat(filepath.Join(cfgDir, trustedKeysFile)); !os.IsNotExist(err) {
		t.Error("seed left on the config partition")
	}
}

func TestApplyTrustedKeysSeed_NeverReplacesExistingKeys(t *testing.T) {
	cfgDir := t.TempDir()
	configPath := t.TempDir()
	_, existing := testSeed(t)
	_, attacker := testSeed(t)
	if err := os.WriteFile(filepath.Join(configPath, trustedKeysFile), existing, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, trustedKeysFile), attacker, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := applyTrustedKeysSeed(zap.NewNop(), cfgDir, configPath); err != nil {
		t.Fatal(err)
	}

	if got, _ := os.ReadFile(filepath.Join(configPath, trustedKeysFile)); !bytes.Equal(got, existing) {
		t.Error("existing trusted keys replaced by the seed")
	}
	if _, err := os.Stat(filepath.Join(cfgDir, trustedKeysFile)); !os.IsNotExist(err) {
		t.Error("ignored seed left on the config partition")
	}
}

func TestApplyTrustedKeysSeed_NoSeedIsANoOp(t *testing.T) {
	configPath := t.TempDir()
	if err := applyTrustedKeysSeed(zap.NewNop(), t.TempDir(), configPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(configPath, trustedKeysFile)); !os.IsNotExist(err) {
		t.Error("access file created without a seed; the device must stay Open")
	}
}

func TestApplyTrustedKeysSeed_FailsWhenTheSeedCannotBeInstalled(t *testing.T) {
	cfgDir := t.TempDir()
	_, seed := testSeed(t)
	if err := os.WriteFile(filepath.Join(cfgDir, trustedKeysFile), seed, 0o644); err != nil {
		t.Fatal(err)
	}
	// configPath under a regular file: the config dir cannot be created.
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := applyTrustedKeysSeed(zap.NewNop(), cfgDir, filepath.Join(blocker, "wendy-agent")); err == nil {
		t.Fatal("a seed that was not installed reported success")
	}
	if _, err := os.Stat(filepath.Join(cfgDir, trustedKeysFile)); err != nil {
		t.Error("seed removed although it was not installed")
	}
}

// A seed the agent could not load is not installed and stays on the config
// partition for the installer to replace; Apply reports it so the agent keeps
// the device Closed with no key instead of Open.
func TestApplyTrustedKeysSeed_LeavesAnUnusableSeedInPlace(t *testing.T) {
	_, good := testSeed(t)
	for name, seed := range map[string][]byte{
		"truncated":     good[:len(good)/2],
		"bom":           append([]byte{0xef, 0xbb, 0xbf}, good...),
		"unknown field": bytes.Replace(good, []byte(`"shape"`), []byte(`"future": 1, "shape"`), 1),
	} {
		cfgDir, configPath := t.TempDir(), t.TempDir()
		src := filepath.Join(cfgDir, trustedKeysFile)
		if err := os.WriteFile(src, seed, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := applyTrustedKeysSeed(zap.NewNop(), cfgDir, configPath); err == nil {
			t.Errorf("%s: unusable seed reported installed", name)
		}
		if _, err := os.Stat(filepath.Join(configPath, trustedKeysFile)); !os.IsNotExist(err) {
			t.Errorf("%s: unusable seed installed", name)
		}
		if _, err := os.Stat(src); err != nil {
			t.Errorf("%s: unusable seed removed: %v", name, err)
		}
	}
}

// A seed whose removal failed is not installed again once the owner reopens
// the device.
func TestApplyTrustedKeysSeed_NotReappliedAfterReopen(t *testing.T) {
	cfgDir, configPath := t.TempDir(), t.TempDir()
	_, seed := testSeed(t)
	src := filepath.Join(cfgDir, trustedKeysFile)
	if err := os.WriteFile(src, seed, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := applyTrustedKeysSeed(zap.NewNop(), cfgDir, configPath); err != nil {
		t.Fatal(err)
	}
	// The removal "failed": the seed is back, and the owner reopened the device.
	if err := os.WriteFile(src, seed, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(configPath, trustedKeysFile)); err != nil {
		t.Fatal(err)
	}
	if err := applyTrustedKeysSeed(zap.NewNop(), cfgDir, configPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(configPath, trustedKeysFile)); !os.IsNotExist(err) {
		t.Error("a reopened device was closed again by the old seed")
	}
}
