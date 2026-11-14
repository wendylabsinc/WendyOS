//go:build windows && regression_windows_linklocal

package linklocal

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/icmp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

type windowsLinkPlan struct {
	Nonce         string               `json:"nonce"`
	DLL           string               `json:"dll"`
	NetworkScript string               `json:"network_script"`
	Permit        string               `json:"permit"`
	Adapters      []windowsLinkAddress `json:"adapters"`
}
type windowsLinkAddress struct {
	Role, Name, GUID, Host, Peer string
	Metric                       int
}
type windowsLinkPeer struct {
	address          windowsLinkAddress
	dll              *windows.DLL
	adapter, session uintptr
	endpoint         *channel.Endpoint
	stack            *stack.Stack
	cancel           context.CancelFunc
	workers          sync.WaitGroup
	failure          chan error
}

func (p *windowsLinkPeer) call(name string, args ...uintptr) (uintptr, error) {
	proc, err := p.dll.FindProc(name)
	if err != nil {
		return 0, err
	}
	result, _, callErr := proc.Call(args...)
	return result, callErr
}
func newWindowsLinkPeer(t *testing.T, dll *windows.DLL, address windowsLinkAddress) *windowsLinkPeer {
	t.Helper()
	p := &windowsLinkPeer{address: address, dll: dll, failure: make(chan error, 2)}
	name, err := windows.UTF16PtrFromString(address.Name)
	if err != nil {
		t.Fatal(err)
	}
	kind, _ := windows.UTF16PtrFromString("Wintun")
	guid, err := windows.GUIDFromString(address.GUID)
	if err != nil {
		t.Fatal(err)
	}
	p.adapter, err = p.call("WintunCreateAdapter", uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(kind)), uintptr(unsafe.Pointer(&guid)))
	runtime.KeepAlive(name)
	runtime.KeepAlive(kind)
	runtime.KeepAlive(guid)
	if p.adapter == 0 {
		t.Fatalf("create owned Wintun adapter: %v", err)
	}
	t.Cleanup(func() {
		if p.cancel != nil {
			p.cancel()
		}
		p.workers.Wait()
		if p.stack != nil {
			p.stack.Close()
			p.stack.Wait()
		}
		if p.session != 0 {
			p.call("WintunEndSession", p.session)
		}
		p.call("WintunCloseAdapter", p.adapter)
		select {
		case err := <-p.failure:
			t.Error(err)
		default:
		}
	})
	p.session, err = p.call("WintunStartSession", p.adapter, 2<<20)
	if p.session == 0 {
		t.Fatalf("start owned Wintun session: %v", err)
	}
	return p
}
func (p *windowsLinkPeer) start(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.stack = stack.New(stack.Options{NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol}, TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, icmp.NewProtocol4}})
	p.endpoint = channel.New(128, 1500, "")
	if err := p.stack.CreateNIC(1, p.endpoint); err != nil {
		t.Fatal(err)
	}
	addr := tcpip.AddrFromSlice(net.ParseIP(p.address.Peer).To4())
	if err := p.stack.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddressWithPrefix{Address: addr, PrefixLen: 16}}, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	p.stack.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})
	event, err := p.call("WintunGetReadWaitEvent", p.session)
	if event == 0 {
		t.Fatal(err)
	}
	p.workers.Add(2)
	go func() {
		defer p.workers.Done()
		for ctx.Err() == nil {
			var size uint32
			packet, err := p.call("WintunReceivePacket", p.session, uintptr(unsafe.Pointer(&size)))
			if packet == 0 {
				if errors.Is(err, windows.ERROR_NO_MORE_ITEMS) {
					windows.WaitForSingleObject(windows.Handle(event), 100)
					continue
				}
				p.failure <- fmt.Errorf("Wintun receive: %w", err)
				return
			}
			if size == 0 || size > 65535 {
				p.call("WintunReleaseReceivePacket", p.session, packet)
				p.failure <- errors.New("invalid Wintun packet size")
				return
			}
			data := append([]byte(nil), unsafe.Slice((*byte)(unsafe.Pointer(packet)), int(size))...)
			p.call("WintunReleaseReceivePacket", p.session, packet)
			if len(data) > 0 && data[0]>>4 == 4 {
				buf := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(data)})
				p.endpoint.InjectInbound(ipv4.ProtocolNumber, buf)
				buf.DecRef()
			}
		}
	}()
	go func() {
		defer p.workers.Done()
		for {
			packet := p.endpoint.ReadContext(ctx)
			if packet == nil {
				return
			}
			view := packet.ToView()
			data := append([]byte(nil), view.AsSlice()...)
			view.Release()
			packet.DecRef()
			target, err := p.call("WintunAllocateSendPacket", p.session, uintptr(len(data)))
			if target == 0 {
				p.failure <- fmt.Errorf("Wintun allocate send: %w", err)
				return
			}
			copy(unsafe.Slice((*byte)(unsafe.Pointer(target)), len(data)), data)
			p.call("WintunSendPacket", p.session, target)
		}
	}()
	listener, err := gonet.ListenTCP(p.stack, tcpip.FullAddress{NIC: 1, Addr: addr, Port: 18475}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	// Listener workers are canceled by closing every accepted connection before
	// the stack/session teardown registered at adapter creation.
	var mu sync.Mutex
	connections := []net.Conn{}
	var server sync.WaitGroup
	acceptDone := make(chan struct{})
	server.Add(1)
	go func() {
		defer server.Done()
		defer close(acceptDone)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			connections = append(connections, conn)
			mu.Unlock()
			server.Add(1)
			go func() {
				defer server.Done()
				defer conn.Close()
				reader := bufio.NewReader(conn)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						return
					}
					if _, err = fmt.Fprintf(conn, "%s:%s", p.address.Role, line); err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-acceptDone
		mu.Lock()
		for _, conn := range connections {
			conn.Close()
		}
		mu.Unlock()
		server.Wait()
	})
}
func TestWindowsLinkLocalCompetingInterfaces(t *testing.T) {
	path := os.Getenv("WENDY_WINDOWS_LINKLOCAL_PLAN")
	if path == "" {
		t.Skip("run test-linklocal-windows.py in the existing hosted Windows job")
	}
	if os.Getenv("GITHUB_ACTIONS") != "true" || os.Getenv("RUNNER_ENVIRONMENT") != "github-hosted" {
		t.Fatal("requires disposable hosted Windows runner")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var plan windowsLinkPlan
	if err = json.Unmarshal(data, &plan); err != nil || len(plan.Adapters) != 2 || len(plan.Nonce) != 32 {
		t.Fatal("invalid owned adapter plan")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		permit, _ := os.ReadFile(plan.Permit)
		if string(permit) == plan.Nonce {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Windows Job admission missing")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !filepath.IsAbs(plan.DLL) || !filepath.IsAbs(plan.NetworkScript) {
		t.Fatal("absolute pinned fixture paths required")
	}
	dll, err := windows.LoadDLL(plan.DLL)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the DLL loaded until process exit: adapter close may schedule its
	// own asynchronous orphan cleanup thread. No foreign Wintun exists here.
	peers := []*windowsLinkPeer{newWindowsLinkPeer(t, dll, plan.Adapters[0]), newWindowsLinkPeer(t, dll, plan.Adapters[1])}
	configure := func(action string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "pwsh", "-NoProfile", "-NonInteractive", "-File", plan.NetworkScript, "-Action", action, "-Plan", path).CombinedOutput()
		if err != nil {
			t.Fatalf("owned Windows %s: %v: %s", action, err, out)
		}
	}
	configure("Configure")
	for _, peer := range peers {
		peer.start(t)
	}
	deadline = time.Now().Add(10 * time.Second)
	for len(systemLinks()) != 2 {
		if time.Now().After(deadline) {
			t.Fatal("both owned link-local addresses did not become ready")
		}
		time.Sleep(50 * time.Millisecond)
	}
	for _, peer := range peers {
		ifi, err := net.InterfaceByName(peer.address.Name)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("owned Windows interface %s index=%d host=%s peer=%s", peer.address.Name, ifi.Index, peer.address.Host, peer.address.Peer)
	}
	type session struct {
		conn   net.Conn
		reader *bufio.Reader
		peer   *windowsLinkPeer
	}
	sessions := []session{}
	dial := func(peer *windowsLinkPeer) session {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		conn, err := Dial(ctx, net.JoinHostPort(peer.address.Peer, "18475"))
		if err != nil {
			t.Fatalf("wrong-route Windows device %s unreachable through production Dial: %v", peer.address.Role, err)
		}
		t.Cleanup(func() { conn.Close() })
		if conn.LocalAddr().(*net.TCPAddr).IP.String() != peer.address.Host {
			t.Fatal("Windows peer selected wrong source/interface")
		}
		return session{conn, bufio.NewReader(conn), peer}
	}
	echo := func(s session, marker string) {
		t.Helper()
		s.conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := fmt.Fprintln(s.conn, marker); err != nil {
			t.Fatal(err)
		}
		reply, err := s.reader.ReadString('\n')
		if err != nil || reply != s.peer.address.Role+":"+marker+"\n" {
			t.Fatalf("Windows TCP crossed/stopped: %q: %v", reply, err)
		}
	}
	if routedSource(peers[1].address.Peer).String() != peers[0].address.Host {
		t.Fatal("Windows fixture lacks competing /16 wrong route")
	}
	for _, peer := range peers {
		s := dial(peer)
		echo(s, "initial")
		sessions = append(sessions, s)
	}
	for _, peer := range peers {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		source, ok := SourceIP(ctx, peer.address.Peer, 18476)
		cancel()
		if !ok || source != peer.address.Host {
			t.Fatal("Windows refusal probe lost interface identity")
		}
	}
	reverse := func(peer *windowsLinkPeer) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		listener, err := Listen(ctx, net.JoinHostPort(peer.address.Host, "18477"))
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		tcpListener := listener.(*net.TCPListener)
		ifi, err := net.InterfaceByName(peer.address.Name)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := tcpListener.SyscallConn()
		if err != nil {
			t.Fatal(err)
		}
		var boundIndex int
		var optionErr error
		if err := raw.Control(func(fd uintptr) {
			boundIndex, optionErr = windows.GetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, ipUnicastIF)
		}); err != nil {
			t.Fatalf("cannot inspect Windows listener socket: %v", err)
		}
		if optionErr != nil {
			t.Fatalf("cannot read Windows listener IP_UNICAST_IF: %v", optionErr)
		}
		// Winsock returns IP_UNICAST_IF in host byte order, although setting it
		// requires network byte order. An exact-address listener can route its
		// replies correctly without this option, so traffic alone does not prove
		// that production pinned the socket to the intended interface.
		// https://learn.microsoft.com/en-us/windows/win32/winsock/ipproto-ip-socket-options
		if boundIndex != ifi.Index {
			t.Fatalf("Windows listener did not pin expected interface: device %s got %d want %d", peer.address.Role, boundIndex, ifi.Index)
		}
		tcpListener.SetDeadline(time.Now().Add(3 * time.Second))
		result := make(chan error, 1)
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				result <- err
				return
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(2 * time.Second))
			data := make([]byte, 4)
			_, err = io.ReadFull(conn, data)
			if err == nil && string(data) != "ping" {
				err = errors.New("wrong peer callback")
			}
			if err == nil {
				_, err = conn.Write([]byte("pong"))
			}
			result <- err
		}()
		target := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFromSlice(net.ParseIP(peer.address.Host).To4()), Port: 18477}
		conn, dialErr := gonet.DialContextTCP(ctx, peer.stack, target, ipv4.ProtocolNumber)
		var responseErr error
		if dialErr == nil {
			conn.SetDeadline(time.Now().Add(2 * time.Second))
			_, responseErr = conn.Write([]byte("ping"))
			if responseErr == nil {
				data := make([]byte, 4)
				_, responseErr = io.ReadFull(conn, data)
				if responseErr == nil && string(data) != "pong" {
					responseErr = errors.New("wrong reply")
				}
			}
			conn.Close()
		}
		acceptedErr := <-result
		if dialErr != nil || responseErr != nil || acceptedErr != nil {
			t.Fatalf("wrong-route Windows listener reply for %s failed: dial=%v response=%v accept=%v", peer.address.Role, dialErr, responseErr, acceptedErr)
		}
		t.Logf("WINDOWS_LISTENER_INTERFACE_VERIFIED: device %s interface %d and actual callback", peer.address.Role, boundIndex)
	}
	reverse(peers[1])
	configure("Reverse")
	if routedSource(peers[0].address.Peer).String() != peers[1].address.Host {
		t.Fatal("Windows fixture did not reverse competing /16 routes")
	}
	for _, s := range sessions {
		echo(s, "same-session-after-route-reversal")
		fresh := dial(s.peer)
		echo(fresh, "fresh-after-route-reversal")
		fresh.conn.Close()
	}
	reverse(peers[0])
	for _, s := range sessions {
		s.conn.Close()
	}
	t.Log("WINDOWS_LINKLOCAL_FULL_16_VERIFIED: exact peers, production interface selection, listener replies and live TCP route reversal")
}
