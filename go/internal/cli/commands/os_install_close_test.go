//go:build !windows

package commands

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/trustedkeys"
	"github.com/wendylabsinc/wendy/go/internal/cli/closedtest"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/localkey"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
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

// --close must never leave a device Open silently: a drive install (whose
// first boot this computer cannot see) is refused, and a USB-flashed board is
// checked after first boot: Closed with this computer's key as owner passes;
// an Open device or an agent that predates access shapes fails the install.
func TestInstallCloseNeverLeavesTheDeviceOpenSilently(t *testing.T) {
	seed := preEnrollOptions{ownerKeys: []byte("{}")}
	if err := installLinuxImage(context.Background(), "", pickerDevice{}, false, "", "", false, false, false, false, "", wifiCLIOptions{}, "box", seed, nil); !errors.Is(err, errCloseOnDrive) {
		t.Fatalf("drive install with --close: %v, want the refusal", err)
	}
	if err := executeRootForError(t, "os", "install", "--close"); err == nil || !strings.Contains(err.Error(), "--device-name") {
		t.Fatalf("--close without --device-name: %v", err)
	}

	mine := useLocalKey(t)
	origDial, origUSB, origWait := dialInstallDriverDevice, usbDirectCandidatesFn, closeBootWait
	t.Cleanup(func() { dialInstallDriverDevice, usbDirectCandidatesFn, closeBootWait = origDial, origUSB, origWait })
	usbDirectCandidatesFn = func() []discovery.USBDirectCandidate { return nil }
	closeBootWait = 3 * time.Second
	at := func(addr string) {
		dialInstallDriverDevice = func(ctx context.Context, _ string) (*grpcclient.AgentConnection, error) {
			return connectWithAutoTLS(ctx, addr)
		}
	}
	oldAgent := grpc.NewServer()
	agentpb.RegisterWendyAgentServiceServer(oldAgent, localVersionAgent{})

	for _, c := range []struct {
		name, addr, want string
	}{
		{"closed, this computer owns it", startLocalOnlyAgent(t, mine), ""},
		{"open", closedtest.Start(t, t.TempDir()).Addr, "Open, not Closed"},
		{"agent without access shapes", serveTest(t, oldAgent), "does not support access shapes"},
	} {
		// Each case is a different device on 127.0.0.1: start from no pins.
		setTempConfig(t, &config.Config{})
		setPinCache(t)
		at(c.addr)
		err := verifyClosedAfterBoot(context.Background(), "box")
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "wendy device access close") {
			t.Errorf("%s: err=%v, want a failure saying %q with the next step", c.name, err, c.want)
		}
	}
}

// A board reinstalled under a name this computer pinned to its old device key
// is refused at once with the unpin hint, not after the whole boot wait.
func TestInstallCloseSurfacesAPinMismatchAtOnce(t *testing.T) {
	setTempConfig(t, &config.Config{})
	setPinCache(t)
	mine := useLocalKey(t)
	origDial, origUSB, origWait := dialInstallDriverDevice, usbDirectCandidatesFn, closeBootWait
	t.Cleanup(func() { dialInstallDriverDevice, usbDirectCandidatesFn, closeBootWait = origDial, origUSB, origWait })
	usbDirectCandidatesFn = func() []discovery.USBDirectCandidate { return nil }
	closeBootWait = time.Minute
	at := func(addr string) {
		dialInstallDriverDevice = func(ctx context.Context, _ string) (*grpcclient.AgentConnection, error) {
			return connectWithAutoTLS(ctx, addr)
		}
	}
	at(startLocalOnlyAgent(t, mine))
	if err := verifyClosedAfterBoot(context.Background(), "box"); err != nil {
		t.Fatal(err)
	}

	at(startLocalOnlyAgent(t, mine)) // a new device key, same address
	start := time.Now()
	err := verifyClosedAfterBoot(context.Background(), "box")
	if err == nil || !strings.Contains(err.Error(), "wendy device unpin") {
		t.Fatalf("err = %v, want the pin refusal with its unpin hint", err)
	}
	if waited := time.Since(start); waited > 20*time.Second {
		t.Errorf("refusal took %s, want it at once", waited)
	}
}

// --close keeps a device to this computer's key; pre-enrolling it would
// enroll it in the background and fail the Closed check after first boot.
func TestInstallCloseRefusesPreEnroll(t *testing.T) {
	err := executeRootForError(t, "os", "install", "--close", "--pre-enroll", "--device-name", "box")
	if err == nil || !strings.Contains(err.Error(), "--pre-enroll") || !strings.Contains(err.Error(), "wendy device enroll") {
		t.Fatalf("err = %v, want the --close/--pre-enroll refusal", err)
	}
}
