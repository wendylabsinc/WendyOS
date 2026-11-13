//go:build linux

package meshcatalog

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/miekg/dns"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

func (b *LANBrowser) Run(ctx context.Context) error {
	if b == nil || ctx == nil {
		return errors.New("missing LAN browser or context")
	}
	type worker struct {
		cancel context.CancelFunc
		done   chan struct{}
	}
	workers := make(map[string]worker)
	defer func() {
		for _, item := range workers {
			item.cancel()
		}
		for _, item := range workers {
			<-item.done
		}
		b.mu.Lock()
		b.current = nil
		b.mu.Unlock()
	}()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		interfaces, err := localmesh.ScanPhysicalLANInterfaces(b.ethernet, b.wifi)
		if err != nil {
			return err
		}
		wanted := make(map[string]localmesh.PhysicalLANInterface, len(interfaces))
		for _, iface := range interfaces {
			key := fmt.Sprintf("%d/%s", iface.Index, iface.IP)
			wanted[key] = iface
		}
		for key, item := range workers {
			_, keep := wanted[key]
			if keep {
				select {
				case <-item.done:
					keep = false
				default:
				}
			}
			if !keep {
				item.cancel()
				<-item.done
				delete(workers, key)
			}
		}
		for key, iface := range wanted {
			if _, exists := workers[key]; exists {
				continue
			}
			workerCtx, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			workers[key] = worker{cancel: cancel, done: done}
			go func() {
				defer close(done)
				_ = b.runInterface(workerCtx, iface)
			}()
		}
		b.mu.Lock()
		b.current = append([]localmesh.PhysicalLANInterface(nil), interfaces...)
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func lanSocketControl(name string) func(string, string, syscall.RawConn) error {
	return func(_, _ string, raw syscall.RawConn) error {
		var inner error
		if err := raw.Control(func(fd uintptr) {
			inner = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1)
			if inner == nil {
				inner = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
			}
			if inner == nil {
				inner = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, name)
			}
		}); err != nil {
			return err
		}
		return inner
	}
}

func (b *LANBrowser) runInterface(ctx context.Context, iface localmesh.PhysicalLANInterface) error {
	nic, err := net.InterfaceByIndex(iface.Index)
	if err != nil || nic.Name != iface.Name {
		return fmt.Errorf("LAN mDNS interface changed: %v", err)
	}
	listener, err := (&net.ListenConfig{Control: lanSocketControl(iface.Name)}).ListenPacket(ctx, "udp4", "0.0.0.0:5353")
	if err != nil {
		return err
	}
	defer listener.Close()
	multicast := ipv4.NewPacketConn(listener)
	if err := multicast.SetControlMessage(ipv4.FlagInterface, true); err != nil {
		return err
	}
	if err := multicast.JoinGroup(nic, &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251)}); err != nil {
		return err
	}
	defer multicast.LeaveGroup(nic, &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251)})
	query, err := (&net.ListenConfig{Control: lanSocketControl(iface.Name)}).ListenPacket(ctx, "udp4", net.JoinHostPort(iface.IP.String(), "0"))
	if err != nil {
		return err
	}
	defer query.Close()
	queries := ipv4.NewPacketConn(query)
	if err := queries.SetControlMessage(ipv4.FlagInterface, true); err != nil {
		return err
	}
	if err := queries.SetMulticastInterface(nic); err != nil {
		return err
	}
	var readers sync.WaitGroup
	read := func(conn *ipv4.PacketConn, done chan<- error) {
		defer readers.Done()
		buf := make([]byte, 9000)
		for ctx.Err() == nil {
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			n, cm, from, err := conn.ReadFrom(buf)
			if err != nil {
				if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
					continue
				}
				done <- err
				return
			}
			udp, ok := from.(*net.UDPAddr)
			if !ok || cm == nil || cm.IfIndex != iface.Index || udp.Port != 5353 {
				continue
			}
			var msg dns.Msg
			if msg.Unpack(buf[:n]) == nil {
				b.cache.Observe(iface, udp.IP, &msg, time.Now())
			}
		}
		done <- nil
	}
	done := make(chan error, 2)
	readers.Add(2)
	go read(multicast, done)
	go read(queries, done)
	defer func() { _ = query.Close(); _ = listener.Close(); readers.Wait() }()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		for _, typ := range append([]string{"_services._dns-sd._udp"}, b.cache.Types(iface, time.Now())...) {
			q := new(dns.Msg)
			q.SetQuestion(typ+".local.", dns.TypePTR)
			q.Question[0].Qclass |= 0x8000
			q.RecursionDesired = false
			packet, err := q.Pack()
			if err != nil {
				continue
			}
			if _, err := queries.WriteTo(packet, &ipv4.ControlMessage{IfIndex: iface.Index}, &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}); err != nil {
				return fmt.Errorf("LAN mDNS query on %s: %w", iface.Name, err)
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-done:
			if err != nil && !errors.Is(err, net.ErrClosed) {
				return err
			}
			return nil
		case <-ticker.C:
		}
	}
}
