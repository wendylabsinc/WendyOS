package commands

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	"github.com/wendylabsinc/wendy/go/internal/cli/localkey"
)

// `wendy os install --close`: the config partition carries an access file the
// agent loads as Closed, with this computer's local key as its only owner, on
// every install path (resolveSeedAgent feeds Orin, Thor and Dragonwing;
// provisionConfigPartition the disk installs).
func TestInstallCloseSeedsThisComputerAsOwner(t *testing.T) {
	keyPath := filepath.Join(t.TempDir(), localkey.FileName)
	orig := localKeyPath
	localKeyPath = func() (string, error) { return keyPath, nil }
	t.Cleanup(func() { localKeyPath = orig })

	seed, err := buildOwnerKeysSeed(io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	agent, _ := resolveSeedAgent(io.Discard, func(string) {}, agentSeed{local: &localInstallAgent{data: []byte("bin")}, ownerKeys: seed})
	dir := t.TempDir()
	if err := writeConfigFiles(dir, agent, nil, "", nil); err != nil {
		t.Fatal(err)
	}

	written := filepath.Join(dir, ownerKeysSeedFile)
	if fi, err := os.Stat(written); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("seed on the config partition: %v (mode %v), want 0600", err, fi)
	}
	store, err := trustedkeys.Open(written)
	if err != nil {
		t.Fatalf("the agent would refuse the seed: %v", err)
	}
	key, err := localkey.LoadOrCreate(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	spki, _ := localkey.PublicKeyDER(key)
	if role, _ := store.Lookup(spki); !store.Closed() || store.KeylessApps() || role != trustedkeys.RoleOwner || len(store.List()) != 1 {
		t.Fatalf("Closed=%v keyless=%v role=%v keys=%d; want Closed, keyless off, this computer the only owner",
			store.Closed(), store.KeylessApps(), role, len(store.List()))
	}

	// Without --close nothing is seeded and the device stays Open.
	plain := t.TempDir()
	if err := writeConfigFiles(plain, configAgent{data: []byte("bin")}, nil, "", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(plain, ownerKeysSeedFile)); !os.IsNotExist(err) {
		t.Fatal("access file written without --close")
	}
}

func TestInstallCloseRefusedWhereNoConfigPartitionIsWritten(t *testing.T) {
	if err := refuseClose(preEnrollOptions{ownerKeys: []byte("{}")}, "Wendy Lite"); err == nil || !strings.Contains(err.Error(), "wendy device access close") {
		t.Fatalf("err = %v, want a refusal naming wendy device access close", err)
	}
	if err := refuseClose(preEnrollOptions{}, "Wendy Lite"); err != nil {
		t.Fatalf("refused without --close: %v", err)
	}
	if err := executeRootForError(t, "os", "install", "--close", "image.img", "/dev/null"); err == nil || !strings.Contains(err.Error(), "--close cannot be combined") {
		t.Fatalf("--close with positional image and drive: %v", err)
	}
}
