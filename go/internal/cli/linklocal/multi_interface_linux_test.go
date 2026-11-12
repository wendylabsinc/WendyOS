package linklocal

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// The shell owns a fresh mount/network/PID namespace and creates both veth
// cables there. No physical USB adapter, host route or Windows API is changed.
func TestLinkLocalTwoInterfacesKeepCorrectDeviceAndReplyPath(t *testing.T) {
	if os.Getenv("WENDY_LINKLOCAL_NAMESPACE") != "1" {
		t.Skip("run test-linklocal-multi-interface.sh on the existing Linux network runner")
	}
	for _, kind := range []string{"mnt", "net", "pid"} {
		current, err := os.Readlink("/proc/self/ns/" + kind)
		parent := os.Getenv("WENDY_LINKLOCAL_PARENT_" + kind)
		if err != nil || parent == "" || current == parent || os.Geteuid() != 0 {
			t.Fatalf("missing private %s namespace", kind)
		}
	}
	runIP := func(args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "ip", args...).CombinedOutput(); err != nil {
			t.Fatalf("fixture ip %v: %v: %s", args, err, out)
		}
	}
	type target struct {
		name, device, host string
		conn               net.Conn
		reader             *bufio.Reader
	}
	devices := []target{{name: "a", device: "169.254.100.2", host: "169.254.100.1"}, {name: "b", device: "169.254.200.2", host: "169.254.200.1"}}
	if got := routedSource(devices[1].device).String(); got != devices[0].host {
		t.Fatalf("fixture did not assign b's ordinary route to a: %s", got)
	}
	for _, d := range devices {
		startLinkLocalPeer(t, d.name, d.device)
	}
	dial := func(d target) net.Conn {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		conn, err := Dial(ctx, net.JoinHostPort(d.device, "18475"))
		if err != nil {
			t.Fatalf("wrong-route device %s was unreachable through production link-local dial: %v", d.name, err)
		}
		if conn.LocalAddr().(*net.TCPAddr).IP.String() != d.host {
			conn.Close()
			t.Fatalf("device %s used wrong source/interface: %v", d.name, conn.LocalAddr())
		}
		t.Cleanup(func() { conn.Close() })
		return conn
	}
	exchange := func(d *target, marker string) {
		t.Helper()
		d.conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := fmt.Fprintln(d.conn, marker); err != nil {
			t.Fatal(err)
		}
		answer, err := d.reader.ReadString('\n')
		if err != nil || answer != d.name+":"+marker+"\n" {
			t.Fatalf("device %s traffic crossed or stopped: %q: %v", d.name, answer, err)
		}
	}
	// Keep both sessions open, including the cable that does not own the best
	// overlapping /16 route. Distinct peer responses prove endpoint identity.
	for i := range devices {
		d := &devices[i]
		d.conn = dial(*d)
		d.reader = bufio.NewReader(d.conn)
		exchange(d, "initial-"+d.name)
	}
	verifySource := func(d target) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		source, ok := SourceIP(ctx, d.device, 18476) // Closed port: a real refusal proves the cable.
		if !ok || source != d.host {
			t.Fatalf("device %s refusal probe selected %q, %v; want %s", d.name, source, ok, d.host)
		}
	}
	for _, d := range devices {
		verifySource(d)
	}
	reverse := func(d *target) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		listener, err := Listen(ctx, net.JoinHostPort(d.host, "0"))
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		listener.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Second))
		finished := make(chan error, 1)
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				finished <- err
				return
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(2 * time.Second))
			if conn.RemoteAddr().(*net.TCPAddr).IP.String() != d.device {
				finished <- fmt.Errorf("unexpected peer %s", conn.RemoteAddr())
				return
			}
			message, err := bufio.NewReader(conn).ReadString('\n')
			if err == nil && message != "callback-"+d.name+"\n" {
				err = fmt.Errorf("crossed callback %q", message)
			}
			if err == nil {
				_, err = fmt.Fprintln(conn, "received-"+d.name)
			}
			finished <- err
		}()
		// The peer initiates this independent connection over its own veth cable.
		d.conn.SetDeadline(time.Now().Add(4 * time.Second))
		_, writeErr := fmt.Fprintf(d.conn, "DIAL %s callback-%s received-%s\n", listener.Addr(), d.name, d.name)
		answer, readErr := d.reader.ReadString('\n')
		err = <-finished
		if writeErr != nil || readErr != nil || err != nil || answer != "reverse-ok\n" {
			t.Fatalf("wrong-route listener reply did not return on device %s interface: answer=%q write=%v read=%v callback=%v", d.name, answer, writeErr, readErr, err)
		}
	}
	reverse(&devices[1]) // Best route belongs to a; b's listener must pin its replies.
	runIP("route", "del", "169.254.0.0/16", "dev", "wusb0", "metric", "10")
	runIP("route", "del", "169.254.0.0/16", "dev", "wusb1", "metric", "20")
	runIP("route", "add", "169.254.0.0/16", "dev", "wusb1", "src", devices[1].host, "metric", "10")
	runIP("route", "add", "169.254.0.0/16", "dev", "wusb0", "src", devices[0].host, "metric", "20")
	if got := routedSource(devices[0].device).String(); got != devices[1].host {
		t.Fatalf("fixture did not reverse a's ordinary route to b: %s", got)
	}
	for i := range devices {
		d := &devices[i]
		exchange(d, "same-session-after-route-change-"+d.name)
		verifySource(*d)
		fresh := dial(*d)
		copy := *d
		copy.conn = fresh
		copy.reader = bufio.NewReader(fresh)
		exchange(&copy, "fresh-after-route-change-"+d.name)
		fresh.Close()
	}
	reverse(&devices[0])
	for i := range devices {
		exchange(&devices[i], "final-"+devices[i].name)
		devices[i].conn.Close()
	}
	t.Log("two overlapping link-local cables retained exact peers, source probes, bound replies and TCP sessions through route reversal")
}

func startLinkLocalPeer(t *testing.T, name, address string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	executable, err := os.Executable()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, "ip", "netns", "exec", "peer-"+name, executable, "-test.run=^TestLinkLocalPeerProcess$", "-test.v", "-test.timeout=40s")
	cmd.Env = append(os.Environ(), "WENDY_LINKLOCAL_PEER="+name, "WENDY_LINKLOCAL_PEER_ADDRESS="+address)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	linesDone := make(chan struct{})
	ready := make(chan struct{})
	go func() {
		defer close(linesDone)
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			if scanner.Text() == "LINKLOCAL_READY "+name {
				close(ready)
			}
		}
	}()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("owned link-local peer did not stop")
		}
		select {
		case <-linesDone:
		case <-time.After(time.Second):
			t.Error("owned link-local peer output did not drain")
		}
	})
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("owned link-local peer did not listen")
	}
}

// A separate real network namespace supplies each device endpoint. The process
// is killed only by its owning exec context or the outer private PID namespace.
func TestLinkLocalPeerProcess(t *testing.T) {
	name := os.Getenv("WENDY_LINKLOCAL_PEER")
	if name != "a" && name != "b" {
		t.Skip("owned namespace subprocess only")
	}
	address := os.Getenv("WENDY_LINKLOCAL_PEER_ADDRESS")
	if address != "169.254.100.2" && address != "169.254.200.2" {
		t.Fatal("invalid owned peer address")
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort(address, "18475"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	fmt.Println("LINKLOCAL_READY " + name)
	var workers sync.WaitGroup
	// Parent cancellation/outer PID namespace ends this long-lived device process.
	for {
		conn, err := listener.Accept()
		if errors.Is(err, net.ErrClosed) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer conn.Close()
			reader := bufio.NewReader(conn)
			for {
				line, err := reader.ReadString('\n')
				if err == io.EOF {
					return
				}
				if err != nil {
					return
				}
				fields := strings.Fields(line)
				if len(fields) == 4 && fields[0] == "DIAL" {
					target, err := net.DialTimeout("tcp4", fields[1], 2*time.Second)
					if err == nil {
						target.SetDeadline(time.Now().Add(2 * time.Second))
						_, err = fmt.Fprintln(target, fields[2])
						if err == nil {
							var got string
							got, err = bufio.NewReader(target).ReadString('\n')
							if err == nil && got != fields[3]+"\n" {
								err = fmt.Errorf("wrong callback response %q", got)
							}
						}
						target.Close()
					}
					if err != nil {
						fmt.Fprintln(conn, "reverse-error", err)
					} else {
						fmt.Fprintln(conn, "reverse-ok")
					}
				} else {
					fmt.Fprintf(conn, "%s:%s", name, line)
				}
			}
		}()
	}
	workers.Wait()
}
