//go:build linux

package nanprovider

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshdiscovery"
)

func TestDiscoveryPublishesBinaryPresenceOnBothHandles(t *testing.T) {
	id := testDevice(t, 1)
	mesh := meshdiscovery.DefaultHint(id.Tenant())
	ssi, err := encodePresence(presence{Device: id, Mesh: &mesh})
	if err != nil {
		t.Fatal(err)
	}
	c := &discoveryController{services: map[string]discoveryService{}}
	lease, err := startDiscovery(c, filepath.Join(t.TempDir(), "discovery"), id, socketIdentity{ino: 1}, ssi)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.release()
	seen := 0
	for _, command := range c.commands {
		if !strings.HasPrefix(command, "NAN_PUBLISH ") && !strings.HasPrefix(command, "NAN_SUBSCRIBE ") {
			continue
		}
		v := fields(command)
		peer, err := decodePresence(id.Tenant(), mesh, v["ssi"])
		if err != nil || peer != id || v["service_name"] != "wendy.mesh.v1" || len(v["ssi"]) != 92 {
			t.Fatalf("invalid binary discovery command: %s (%v)", command, err)
		}
		seen++
	}
	if seen != 2 {
		t.Fatalf("publish/subscribe presence count = %d", seen)
	}
}

type discoveryController struct {
	services                  map[string]discoveryService
	commands                  []string
	status                    string
	failSubscribe, failCancel bool
	onPublish                 func()
}

func (c *discoveryController) command(command string) (string, error) {
	c.commands = append(c.commands, command)
	v := fields(command)
	switch strings.Fields(command)[0] {
	case "NAN_STATUS":
		if c.status != "" {
			return c.status, nil
		}
		out := fmt.Sprintf("nan_started=1\nnum_services=%d\n", len(c.services))
		for id, service := range c.services {
			out += fmt.Sprintf("service=%s type=%s name=%s sync=1\n", id, service.kind, service.name)
		}
		return out, nil
	case "NAN_PUBLISH", "NAN_SUBSCRIBE":
		kind := "publish"
		if strings.HasPrefix(command, "NAN_SUBSCRIBE") {
			kind = "subscribe"
			if c.failSubscribe {
				return "", errors.New("subscribe failed")
			}
		}
		for i := 1; i <= 20; i++ {
			id := strconv.Itoa(i)
			if _, used := c.services[id]; !used {
				c.services[id] = discoveryService{kind, v["service_name"]}
				if kind == "publish" && c.onPublish != nil {
					c.onPublish()
				}
				return id, nil
			}
		}
		return "", errors.New("service capacity exhausted")
	case "NAN_CANCEL_PUBLISH", "NAN_CANCEL_SUBSCRIBE":
		if c.failCancel {
			return "", errors.New("cancel failed")
		}
		id := v["publish_id"]
		if id == "" {
			id = v["subscribe_id"]
		}
		delete(c.services, id)
		return "OK", nil
	}
	return "", fmt.Errorf("unexpected command %s", command)
}

func TestDiscoveryReclaimsOnlyRecordedHandlesAfterCrash(t *testing.T) {
	for _, replacedSocket := range []bool{false, true} {
		t.Run(fmt.Sprint("new socket=", replacedSocket), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "discovery")
			c := &discoveryController{services: map[string]discoveryService{"3": {"publish", "wendy.mesh.v1.foreign"}}}
			id, socket := testDevice(t, 1), socketIdentity{dev: 1, ino: 2, ctimeSec: 3}
			first, err := startDiscovery(c, path, id, socket, "00")
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("journal permissions: %v %v", info, err)
			}
			if replacedSocket {
				socket.ino++
				c.services[first.publish] = discoveryService{"publish", "foreign.reused.publish"}
				c.services[first.sub] = discoveryService{"subscribe", "foreign.reused.subscribe"}
			}
			c.commands = nil // First lease is deliberately abandoned, as with SIGKILL.
			second, err := startDiscovery(c, path, id, socket, "00")
			if err != nil {
				t.Fatal(err)
			}
			if c.services["3"].name != "wendy.mesh.v1.foreign" {
				t.Fatal("foreign publisher removed")
			}
			want := 3
			if replacedSocket {
				want = 5
				for _, cmd := range c.commands {
					if strings.HasPrefix(cmd, "NAN_CANCEL") {
						t.Fatalf("old socket handle canceled: %s", cmd)
					}
				}
			}
			if len(c.services) != want {
				t.Fatalf("discovery handles leaked: %v", c.services)
			}
			if err := second.release(); err != nil {
				t.Fatal(err)
			}
			if len(c.services) != want-2 {
				t.Fatalf("graceful cleanup removed foreign resources: %v", c.services)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("journal remains: %v", err)
			}
		})
	}
}

func TestDiscoveryPreservesReusedHandleAndUnrecordedService(t *testing.T) {
	path := filepath.Join(t.TempDir(), "discovery")
	c := &discoveryController{services: map[string]discoveryService{}}
	id, socket := testDevice(t, 1), socketIdentity{ino: 1}
	first, err := startDiscovery(c, path, id, socket, "00")
	if err != nil {
		t.Fatal(err)
	}
	c.services[first.publish] = discoveryService{"publish", "another.client"}
	second, err := startDiscovery(c, path, id, socket, "00")
	if err != nil {
		t.Fatal(err)
	}
	if c.services[first.publish].name != "another.client" {
		t.Fatal("reused foreign handle canceled")
	}
	if err := second.release(); err != nil {
		t.Fatal(err)
	}
	c.services["8"] = discoveryService{"publish", nanServiceName}
	c.commands = nil
	if _, err := startDiscovery(c, path, id, socket, "00"); err == nil {
		t.Fatal("unrecorded service accepted as ours")
	}
	for _, cmd := range c.commands {
		if strings.HasPrefix(cmd, "NAN_CANCEL") || strings.HasPrefix(cmd, "NAN_PUBLISH") {
			t.Fatalf("unrecorded service mutated: %s", cmd)
		}
	}
}

func TestDiscoveryCrashHistoryDoesNotExhaustSupplicantCapacity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "discovery")
	c := &discoveryController{services: map[string]discoveryService{"9": {"subscribe", "foreign"}}}
	var lease *discoveryLease
	for i := 0; i < 32; i++ {
		var err error
		lease, err = startDiscovery(c, path, testDevice(t, 1), socketIdentity{ino: 1}, "00")
		if err != nil {
			t.Fatalf("crash %d: %v", i, err)
		}
		if len(c.services) != 3 || c.services["9"].name != "foreign" {
			t.Fatalf("crash %d changed resource ownership: %v", i, c.services)
		}
	}
	if err := lease.release(); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoveryFailedCancellationRetainsJournalAndBlocksNewServices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "discovery")
	c := &discoveryController{services: map[string]discoveryService{}}
	id, socket := testDevice(t, 1), socketIdentity{ino: 1}
	if _, err := startDiscovery(c, path, id, socket, "00"); err != nil {
		t.Fatal(err)
	}
	c.failCancel = true
	if _, err := startDiscovery(c, path, id, socket, "00"); err == nil {
		t.Fatal("failed cancellation hidden")
	}
	if len(c.services) != 2 {
		t.Fatal("new services allocated after failed reclaim")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("lost cleanup journal", err)
	}
	c.failCancel = false
	d, err := startDiscovery(c, path, id, socket, "00")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.release(); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoveryPartialStartAndJournalFailureCleanReturnedHandles(t *testing.T) {
	for _, diskFailure := range []bool{false, true} {
		t.Run(fmt.Sprint("journal failure=", diskFailure), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "discovery")
			c := &discoveryController{services: map[string]discoveryService{"9": {"publish", "foreign"}}, failSubscribe: !diskFailure}
			if diskFailure {
				c.onPublish = func() {
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.Mkdir(path, 0700); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := startDiscovery(c, path, testDevice(t, 1), socketIdentity{ino: 1}, "00"); err == nil {
				t.Fatal("start failure hidden")
			}
			if len(c.services) != 1 || c.services["9"].name != "foreign" {
				t.Fatalf("partial start leaked or removed foreign service: %v", c.services)
			}
		})
	}
}

func TestDiscoveryRejectsIncompleteInventoryAndForeignJournal(t *testing.T) {
	for _, status := range []string{"nan_started=1", "num_services=2\nservice=1 type=publish name=wendy.mesh.v1", "num_services=2\nservice=1 type=publish\nservice=1 type=subscribe"} {
		c := &discoveryController{status: status}
		if _, err := discoveryServices(c); err == nil {
			t.Fatalf("accepted ambiguous inventory: %q", status)
		}
	}
	path := filepath.Join(t.TempDir(), "discovery")
	c := &discoveryController{services: map[string]discoveryService{}}
	if _, err := startDiscovery(c, path, testDevice(t, 1), socketIdentity{ino: 1}, "00"); err != nil {
		t.Fatal(err)
	}
	if _, err := startDiscovery(c, path, testDevice(t, 2), socketIdentity{ino: 1}, "00"); err == nil {
		t.Fatal("foreign principal journal accepted")
	}
	if len(c.services) != 2 {
		t.Fatal("foreign principal resources changed")
	}
}

func TestRadioControlRejectsTruncatedDatagram(t *testing.T) {
	for _, size := range []int{8192, 8193} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			dir := t.TempDir()
			server, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: filepath.Join(dir, "server"), Net: "unixgram"})
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			client, err := net.DialUnix("unixgram", &net.UnixAddr{Name: filepath.Join(dir, "client"), Net: "unixgram"}, server.LocalAddr().(*net.UnixAddr))
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			done := make(chan error, 1)
			go func() {
				b := make([]byte, 100)
				_, address, err := server.ReadFromUnix(b)
				if err == nil {
					_, err = server.WriteToUnix([]byte(strings.Repeat("x", size)), address)
				}
				done <- err
			}()
			c := radioControl{conn: client}
			reply, err := c.command("NAN_STATUS")
			if size == 8192 && (err != nil || len(reply) != size) {
				t.Fatalf("complete boundary reply: %d %v", len(reply), err)
			}
			if size > 8192 && err == nil {
				t.Fatal("truncated reply accepted")
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
