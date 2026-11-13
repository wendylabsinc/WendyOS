//go:build linux

package nanprovider

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	quic "github.com/quic-go/quic-go"
	"github.com/vishvananda/netlink"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

const partialCreateHelper = "/usr/sbin/wendyos-nan"

// The real Provider calls this subprocess through its ordinary helper path.
// It creates a real kernel interface, then reports failure just as a helper
// interrupted after interface_add can do. Only a disposable container installs
// this test binary at the helper path; no radio hardware is required.
func init() {
	if os.Args[0] != partialCreateHelper || os.Getenv("WENDY_NAN_PARTIAL_CREATE_HELPER") != "1" {
		return
	}
	fail := func(err error) { fmt.Fprintln(os.Stderr, err); os.Exit(1) }
	if len(os.Args) < 2 {
		fail(fmt.Errorf("missing helper command"))
	}
	if path := os.Getenv("WENDY_NAN_FIXTURE_COMMANDS"); path != "" {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
		if err != nil {
			fail(err)
		}
		_, err = fmt.Fprintln(file, os.Args[1])
		closeErr := file.Close()
		if err != nil {
			fail(err)
		}
		if closeErr != nil {
			fail(closeErr)
		}
	}
	switch os.Args[1] {
	case "status":
		state := os.Getenv("WENDY_NAN_FIXTURE_EXTERNAL")
		if state == "" {
			fmt.Println("nan0 is not present")
			os.Exit(1)
		}
		fmt.Println("nan_started=" + state)
	case "start", "schedule-default":
	case "ndi-create":
		if err := netlink.LinkAdd(&netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: ndiName}}); err != nil {
			fail(err)
		}
		if os.Getenv("WENDY_NAN_PARTIAL_CREATE_WAIT") == "1" {
			fmt.Fprintln(os.Stderr, "partial-create-fault")
			time.Sleep(time.Hour) // CommandContext must cancel this child.
		}
		fail(fmt.Errorf("partial-create-fault"))
	case "ndi-remove":
		link, err := netlink.LinkByName(ndiName)
		if _, absent := err.(netlink.LinkNotFoundError); !absent {
			if err != nil {
				fail(err)
			}
			if err = netlink.LinkDel(link); err != nil {
				fail(err)
			}
		}
	case "stop":
		if os.Getenv("WENDY_NAN_FIXTURE_EXTERNAL") != "" {
			fail(fmt.Errorf("foreign-management-stop-fault"))
		}
		if _, err := netlink.LinkByName(ndiName); err == nil {
			fail(fmt.Errorf("remove NDI before stopping NAN"))
		}
	default:
		fail(fmt.Errorf("unexpected helper command %q", os.Args[1]))
	}
	os.Exit(0)
}

type unusedPartialCreateNode struct{}

func (unusedPartialCreateNode) AttachWithCost(context.Context, meshid.Device, *quic.Conn, uint16) error {
	return fmt.Errorf("unexpected peer attachment")
}

func TestProviderRemovesNDIAfterPartialCreateFailure(t *testing.T) {
	for _, cancelAfterCreate := range []bool{false, true} {
		name := "helper reports failure"
		if cancelAfterCreate {
			name = "parent cancels after interface creation"
		}
		t.Run(name, func(t *testing.T) { checkPartialCreateCleanup(t, cancelAfterCreate, "", false, false) })
	}
}

func TestProviderPreservesExternalNANAfterPrivateNDIReclaim(t *testing.T) {
	checkPartialCreateCleanup(t, false, "1", true, false)
}

func TestProviderPreservesInactiveExternalNAN(t *testing.T) {
	checkPartialCreateCleanup(t, false, "0", false, false)
}

func TestProviderRelinquishesOwnedClusterWhenAppNDIExists(t *testing.T) {
	checkPartialCreateCleanup(t, false, "1", false, true)
}

func checkPartialCreateCleanup(t *testing.T, cancelAfterCreate bool, externalState string, reclaimedNDI, appSession bool) {
	t.Helper()
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("disposable privileged container only")
	}
	protected := []string{partialCreateHelper, nanControlPath, sessionOwnerPath, "/run/wendy-nan-provider.lock", "/run/systemd/network/05-wendy-nan-provider.network", "/run/NetworkManager/conf.d/05-wendy-nan-provider.conf", discoveryOwnerPath}
	for _, path := range protected {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Skipf("preserve pre-existing host state: %s", path)
		}
	}
	if _, err := netlink.LinkByName(ndiName); err == nil {
		t.Skip("preserve pre-existing NDI")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(partialCreateHelper), 0755); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(exe, partialCreateHelper); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(partialCreateHelper) })
	t.Setenv("WENDY_NAN_PARTIAL_CREATE_HELPER", "1")
	t.Setenv("WENDY_NAN_FIXTURE_EXTERNAL", externalState)
	commandsPath := filepath.Join(t.TempDir(), "commands")
	t.Setenv("WENDY_NAN_FIXTURE_COMMANDS", commandsPath)
	if err = os.MkdirAll(filepath.Dir(nanControlPath), 0700); err != nil {
		t.Fatal(err)
	}
	control, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: nanControlPath, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = control.Close(); _ = os.Remove(nanControlPath) })
	id := underlayDevice(t, 0x1234)
	beforeSocket, err := nanControlSocketIdentity()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if link, err := netlink.LinkByName(ndiName); err == nil && link.Type() == "dummy" {
			_ = netlink.LinkDel(link)
		}
		_ = clearSessionOwner(id)
		for _, path := range protected[3:] {
			_ = os.Remove(path)
		}
	})
	if appSession {
		// A prior provider owned this exact management socket, but a running
		// application now depends on its shared cluster. Startup must drop the
		// marker and avoid both abandoned-session reset and deferred stop.
		appNDI := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "wa123456abcdef"}}
		if err := netlink.LinkAdd(appNDI); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = netlink.LinkDel(appNDI) })
		if err := markSessionOwner(id, beforeSocket); err != nil {
			t.Fatal(err)
		}
	}
	if reclaimedNDI {
		link := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: ndiName}}
		if err := netlink.LinkAdd(link); err != nil {
			t.Fatal(err)
		}
		addr, err := netlink.ParseAddr("10.89.18.52/16")
		if err != nil {
			t.Fatal(err)
		}
		if err := netlink.AddrReplace(link, addr); err != nil {
			t.Fatal(err)
		}
	}
	provider := Provider{Credentials: &localmesh.Credentials{Device: id}, Node: unusedPartialCreateNode{}}
	if cancelAfterCreate {
		t.Setenv("WENDY_NAN_PARTIAL_CREATE_WAIT", "1")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- provider.Run(ctx) }()
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, lookupErr := netlink.LinkByName(ndiName); lookupErr == nil {
				break
			}
			select {
			case err := <-done:
				t.Fatalf("provider exited before creating its interface: %v", err)
			default:
			}
			if time.Now().After(deadline) {
				cancel()
				<-done
				t.Fatal("helper did not reach partial interface creation")
			}
			time.Sleep(time.Millisecond)
		}
		cancel()
		err = <-done
	} else {
		err = provider.Run(context.Background())
	}
	if err == nil || !cancelAfterCreate && !strings.Contains(err.Error(), "partial-create-fault") {
		t.Fatalf("did not reach the real partial-create fault: %v", err)
	}
	if _, err = netlink.LinkByName(ndiName); err == nil {
		t.Error("failed helper left its unaddressed NDI behind, preventing the next provider from starting")
	}
	if _, err = os.Lstat(sessionOwnerPath); !os.IsNotExist(err) {
		t.Errorf("NAN cleanup could not finish after partial interface creation: %v", err)
	}
	if externalState != "" {
		commands, err := os.ReadFile(commandsPath)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains("\n"+string(commands), "\nstop\n") {
			t.Error("provider tried to stop an external NAN management interface")
		}
		afterSocket, err := nanControlSocketIdentity()
		if err != nil || afterSocket != beforeSocket {
			t.Errorf("external control socket changed: %v", err)
		}
	}
}
