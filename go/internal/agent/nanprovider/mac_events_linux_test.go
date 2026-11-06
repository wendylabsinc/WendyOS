//go:build linux

package nanprovider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"go.uber.org/zap"
)

// Exercise the production event loop and Unix control transport, without a
// radio. Only a disposable privileged container may install the fixture paths.
func TestRadioInvalidEventsIssueNoSetupCommands(t *testing.T) {
	if os.Getenv("WENDY_LOCALMESH_ISOLATED_TEST") != "1" {
		t.Skip("disposable privileged container only")
	}
	for _, path := range []string{nanControlPath, discoveryOwnerPath} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("preserve pre-existing fixture path %s: %v", path, err)
		}
	}
	ndi := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: ndiName, HardwareAddr: net.HardwareAddr{2, 0, 0, 0, 0, 1}}}
	if err := netlink.LinkAdd(ndi); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = netlink.LinkDel(ndi) })
	if err := os.MkdirAll(filepath.Dir(nanControlPath), 0700); err != nil {
		t.Fatal(err)
	}
	server, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: nanControlPath, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan struct{})
	t.Cleanup(func() {
		_ = server.Close()
		<-serverDone
		_ = os.Remove(nanControlPath)
		_ = os.Remove(discoveryOwnerPath)
	})
	eventAddress := make(chan *net.UnixAddr, 1)
	ready := make(chan struct{})
	commands := make(chan string, 64)
	go func() {
		defer close(serverDone)
		controller := &discoveryController{services: map[string]discoveryService{}}
		buf := make([]byte, 8192)
		for {
			n, addr, err := server.ReadFromUnix(buf)
			if err != nil {
				return
			}
			cmd := string(buf[:n])
			reply := "OK"
			switch {
			case cmd == "ATTACH":
				eventAddress <- addr
			case cmd == "PING":
				reply = "PONG"
				commands <- cmd
			case strings.HasPrefix(cmd, "NAN_NDP_REQUEST "), strings.HasPrefix(cmd, "NAN_NDP_RESPONSE "):
				commands <- cmd
			default:
				reply, err = controller.command(cmd)
				if err != nil {
					t.Errorf("unexpected control command %q: %v", cmd, err)
					reply = "FAIL"
				}
			}
			if _, err := server.WriteToUnix([]byte(reply), addr); err != nil {
				t.Errorf("fixture reply: %v", err)
				return
			}
			if strings.HasPrefix(cmd, "NAN_SUBSCRIBE ") {
				close(ready)
			}
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	radioDone := make(chan error, 1)
	go func() {
		radioDone <- runRadio(ctx, testDevice(t, 100), make(chan radioPeer, 8), nil, nil, &nanRecovery{}, zap.NewNop(), nil)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-radioDone:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("radio shutdown: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("radio did not drain")
		}
	})
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("radio did not start discovery")
	}
	addr := <-eventAddress
	emit := func(event string) {
		t.Helper()
		if _, err := server.WriteToUnix([]byte("<3>"+event), addr); err != nil {
			t.Fatal(err)
		}
	}
	presence := func(n uint16) string {
		t.Helper()
		claim := presenceFixture(t, testTenant(t).String(), testDevice(t, n).UUID().String())
		ssi, err := encodePresence(claim)
		if err != nil {
			t.Fatal(err)
		}
		return ssi
	}
	for i, bad := range invalidNANTargets {
		if strings.ContainsAny(bad, " \t\n") {
			continue // Event fields are already whitespace delimited.
		}
		emit(fmt.Sprintf("NAN-DISCOVERY-RESULT subscribe_id=2 publish_id=9 address=%s ssi=%s", bad, presence(300+uint16(i))))
		emit(fmt.Sprintf("NAN-NDP-REQUEST publish_inst_id=1 peer_nmi=%s init_ndi=02:00:00:00:00:03 ndp_id=1 ssi=%s", bad, presence(20)))
		emit(fmt.Sprintf("NAN-NDP-REQUEST publish_inst_id=1 peer_nmi=02:00:00:00:00:05 init_ndi=%s ndp_id=1 ssi=%s", bad, presence(20)))
	}
	// Incoming responses are immediate; outgoing candidates are scheduled on
	// the ticker. Accept either valid command order, but no invalid target.
	// Each malformed discovery above claims a distinct device so known-peer
	// matching cannot hide it if it accidentally enters the candidate queue.
	emit(fmt.Sprintf("NAN-DISCOVERY-RESULT subscribe_id=2 publish_id=9 address=02:00:00:00:00:04 ssi=%s", presence(200)))
	emit(fmt.Sprintf("NAN-NDP-REQUEST publish_inst_id=1 peer_nmi=02:00:00:00:00:05 init_ndi=02:00:00:00:00:06 ndp_id=1 ssi=%s", presence(20)))
	want := map[string]bool{
		"NAN_NDP_REQUEST handle=2 ndi=wnanndi0 peer_nmi=02:00:00:00:00:04 ":         false,
		"NAN_NDP_RESPONSE accept handle=1 ndi=wnanndi0 peer_nmi=02:00:00:00:00:05 ": false,
	}
	remaining := len(want)
	deadline := time.After(8 * time.Second)
	for {
		select {
		case cmd := <-commands:
			if cmd == "PING" {
				if remaining == 0 {
					// Same-socket FIFO proves the previous candidate batch is
					// complete, including any invalid setup after the valid ones.
					return
				}
				continue
			}
			matched := false
			for prefix, seen := range want {
				if !seen && strings.HasPrefix(cmd, prefix) {
					want[prefix] = true
					remaining--
					matched = true
					break
				}
			}
			if !matched {
				t.Fatalf("invalid or duplicate setup command: %s", cmd)
			}
		case <-deadline:
			t.Fatalf("valid event/candidate batch did not complete; missing %d commands", remaining)
		}
	}
}
