//go:build linux

// babeltest is an experimental oracle adapter, NOT a production daemon.
// Run only in disposable network namespaces; it changes kernel routes.
package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	babel "github.com/wendylabsinc/WendyOS/babel"
)

type list []string

func (l *list) String() string     { return strings.Join(*l, ";") }
func (l *list) Set(s string) error { *l = append(*l, s); return nil }

type link struct {
	babel.Link
	name  string
	index int
}
type incoming struct {
	data   []byte
	source netip.Addr
	index  int
}

func main() {
	var specs, prefixes list
	flag.Var(&specs, "link", "interface,local-link-local,peer-link-local[,local-ipv4]")
	flag.Var(&prefixes, "originate", "local prefix")
	id := flag.Uint64("id", 1, "router ID")
	withdraw := flag.Duration("withdraw-after", 0, "withdraw local prefixes")
	duration := flag.Duration("duration", 30*time.Second, "run duration")
	flag.Parse()
	e, err := babel.New(babel.Config{RouterID: babel.RouterID(*id), HelloInterval: time.Second, UpdateInterval: 4 * time.Second, IPv4ViaIPv6: true})
	check(err)
	conn, err := net.ListenUDP("udp6", &net.UDPAddr{Port: 6696})
	check(err)
	defer conn.Close()
	links := map[babel.LinkID]link{}
	byIndex := map[int]babel.LinkID{}
	raw, err := conn.SyscallConn()
	check(err)
	var sockErr error
	check(raw.Control(func(fd uintptr) {
		for _, opt := range []int{syscall.IPV6_RECVPKTINFO} {
			if err := syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, opt, 1); err != nil {
				sockErr = err
			}
		}
		for _, opt := range []int{syscall.IPV6_UNICAST_HOPS, syscall.IPV6_MULTICAST_HOPS} {
			if err := syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, opt, 1); err != nil {
				sockErr = err
			}
		}
	}))
	check(sockErr)
	for i, s := range specs {
		parts := strings.Split(s, ",")
		if len(parts) < 3 || len(parts) > 4 {
			log.Fatal("bad link")
		}
		iface, err := net.InterfaceByName(parts[0])
		check(err)
		l := link{Link: babel.Link{ID: babel.LinkID(i + 1), Local: netip.MustParseAddr(parts[1]), Peer: netip.MustParseAddr(parts[2]), Cost: 96, MaxPacket: iface.MTU - 48}, name: parts[0], index: iface.Index}
		if len(parts) == 4 {
			l.IPv4 = netip.MustParseAddr(parts[3])
		}
		links[l.ID] = l
		byIndex[iface.Index] = l.ID
		check(raw.Control(func(fd uintptr) {
			group := netip.MustParseAddr("ff02::1:6").As16()
			sockErr = syscall.SetsockoptIPv6Mreq(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_JOIN_GROUP, &syscall.IPv6Mreq{Multiaddr: group, Interface: uint32(iface.Index)})
		}))
		check(sockErr)
	}
	var installed []babel.Route
	send := func(ds []babel.Datagram) {
		for _, d := range ds {
			l := links[d.Link]
			oob := make([]byte, syscall.CmsgSpace(20))
			h := (*syscall.Cmsghdr)(unsafe.Pointer(&oob[0]))
			h.Level = syscall.IPPROTO_IPV6
			h.Type = syscall.IPV6_PKTINFO
			h.SetLen(syscall.CmsgLen(20))
			data := oob[syscall.CmsgLen(0):]
			a := l.Local.As16()
			copy(data, a[:])
			binary.NativeEndian.PutUint32(data[16:], uint32(l.index))
			_, _, err := conn.WriteMsgUDP(d.Payload, oob, &net.UDPAddr{IP: net.IP(l.Peer.AsSlice()), Port: 6696, Zone: l.name})
			if err != nil {
				log.Printf("send: %v", err)
			}
		}
	}
	apply := func(fx babel.Effects) {
		if fx.Revision != 0 {
			for _, r := range fx.Routes {
				if r.Local {
					continue
				}
				args := []string{family(r.Prefix), "route", "replace"}
				if r.Unreachable {
					args = append(args, "unreachable", r.Prefix.String())
				} else {
					args = append(args, r.Prefix.String(), "via")
					if r.Prefix.Addr().Is4() && r.NextHop.Is6() {
						args = append(args, "inet6")
					}
					args = append(args, r.NextHop.String(), "dev", links[r.Link].name)
				}
				args = append(args, "proto", "201", "metric", "10000")
				run(args...)
			}
			for _, old := range installed {
				if old.Local {
					continue
				}
				found := false
				for _, r := range fx.Routes {
					if r.Prefix == old.Prefix && !r.Local {
						found = true
					}
				}
				if !found {
					run(family(old.Prefix), "route", "del", old.Prefix.String(), "proto", "201", "metric", "10000")
				}
			}
			installed = fx.Routes
			var err error
			fx, err = e.Commit(fx.Revision, true)
			check(err)
		}
		send(fx.Datagrams)
	}
	start := time.Now()
	step := func(ev babel.Event) { fx, err := e.Step(time.Since(start), ev); check(err); apply(fx) }
	for i := 1; i <= len(links); i++ {
		step(babel.AddLink{Link: links[babel.LinkID(i)].Link})
	}
	for _, p := range prefixes {
		step(babel.Originate{Prefix: netip.MustParsePrefix(p)})
	}
	ch := make(chan incoming, 256)
	go func() {
		for {
			buf := make([]byte, 65535)
			oob := make([]byte, 256)
			n, on, flags, from, err := conn.ReadMsgUDP(buf, oob)
			if err != nil {
				return
			}
			if flags&(syscall.MSG_TRUNC|syscall.MSG_CTRUNC) != 0 || from.Port != 6696 {
				continue
			}
			a, ok := netip.AddrFromSlice(from.IP)
			if !ok || !a.IsLinkLocalUnicast() {
				continue
			}
			msgs, err := syscall.ParseSocketControlMessage(oob[:on])
			if err != nil {
				continue
			}
			for _, m := range msgs {
				if m.Header.Level == syscall.IPPROTO_IPV6 && m.Header.Type == syscall.IPV6_PKTINFO && len(m.Data) >= 20 {
					idx := int(binary.NativeEndian.Uint32(m.Data[16:]))
					ch <- incoming{buf[:n], a, idx}
				}
			}
		}
	}()
	timer := time.NewTimer(time.Millisecond)
	defer timer.Stop()
	report := time.NewTicker(time.Second)
	defer report.Stop()
	end := time.NewTimer(*duration)
	defer end.Stop()
	var withdrawal <-chan time.Time
	if *withdraw > 0 {
		t := time.NewTimer(*withdraw)
		defer t.Stop()
		withdrawal = t.C
	}
	for {
		d := time.Second
		if deadline, ok := e.NextDeadline(); ok {
			d = max(time.Millisecond, deadline-time.Since(start))
		}
		timer.Reset(d)
		select {
		case p := <-ch:
			lid, ok := byIndex[p.index]
			if ok && links[lid].Peer == p.source {
				step(babel.Receive{ID: lid, Packet: p.data})
			}
		case <-timer.C:
			step(babel.Tick{})
		case <-report.C:
			check(json.NewEncoder(os.Stdout).Encode(e.Snapshot()))
		case <-withdrawal:
			for _, p := range prefixes {
				step(babel.Withdraw{Prefix: netip.MustParsePrefix(p)})
			}
			withdrawal = nil
		case <-end.C:
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
}
func family(p netip.Prefix) string {
	if p.Addr().Is4() {
		return "-4"
	}
	return "-6"
}
func check(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
func run(args ...string) {
	out, err := exec.Command("ip", args...).CombinedOutput()
	if err != nil {
		log.Fatal(fmt.Sprint(args), ": ", err, " ", strconv.Quote(string(out)))
	}
}
