//go:build linux

package localmesh

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/hashicorp/mdns"
	"github.com/miekg/dns"
	"go.uber.org/zap"
	"golang.org/x/net/ipv4"
	"golang.org/x/sys/unix"
)

const (
	LANPort            = 43023
	EthernetLinkCost   = uint16(64)
	WiFiLinkCost       = uint16(128)
	lanService         = "_wendy-mesh._tcp"
	lanInterfacePeriod = 5 * time.Second
)

// LANConfig enables discovery independently for physical Ethernet and
// infrastructure Wi-Fi. Advertisements are only hints: the QUIC TLS handshake
// pins the expected enrolled org and asset before Node admits a link.
type LANConfig struct {
	Credentials        *Credentials
	Node               *Node
	Ethernet           bool
	InfrastructureWiFi bool
	Selection          *PeerSelection
	Logger             *zap.Logger
	// Status receives local setup readiness/failure, not peer reachability.
	// The optional callback must return promptly.
	Status func(CarrierStatus)
}

func (c LANConfig) allow(asset int32, cost uint16) bool {
	return c.Selection == nil || c.Selection.AllowLAN(asset, cost)
}

type lanInterface struct {
	iface net.Interface
	ip    net.IP
	net   *net.IPNet
	cost  uint16
}

type lanClaim struct {
	asset int32
	cost  uint16
}

type lanClaims struct {
	mu     sync.Mutex
	active map[lanClaim]bool
}

func (c *lanClaims) claim(asset int32, cost uint16) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := lanClaim{asset, cost}
	if c.active[key] || len(c.active) >= 64 {
		return false
	}
	c.active[key] = true
	return true
}

func (c *lanClaims) release(asset int32, cost uint16) {
	c.mu.Lock()
	delete(c.active, lanClaim{asset, cost})
	c.mu.Unlock()
}

func (i lanInterface) key() string {
	return fmt.Sprintf("%d/%s/%d", i.iface.Index, i.ip.String(), i.cost)
}

// RunLAN watches eligible interfaces, so adding/removing an Ethernet cable or
// changing an infrastructure Wi-Fi address starts/stops the corresponding
// discovery listener without restarting the mesh agent.
func RunLAN(ctx context.Context, cfg LANConfig) error {
	if cfg.Credentials == nil || cfg.Node == nil {
		return errors.New("LAN mesh requires credentials and node")
	}
	return runLANWithScan(ctx, cfg, func() ([]lanInterface, error) {
		return scanLANInterfaces(cfg.Ethernet, cfg.InfrastructureWiFi)
	})
}

func runLANWithScan(ctx context.Context, cfg LANConfig, scan func() ([]lanInterface, error)) error {
	if cfg.Selection == nil {
		cfg.Selection = NewPeerSelection(cfg.Node.Snapshot)
	}
	claims := &lanClaims{active: make(map[lanClaim]bool)}
	health := &lanCarrierHealth{report: cfg.Status}
	type worker struct {
		cancel context.CancelFunc
		done   chan struct{}
	}
	workers := map[string]worker{}
	defer func() {
		for _, w := range workers {
			w.cancel()
		}
		for _, w := range workers {
			<-w.done
		}
	}()
	ticker := time.NewTicker(lanInterfacePeriod)
	defer ticker.Stop()
	for {
		interfaces, err := scan()
		if err != nil {
			return err
		}
		health.wanted(interfaces)
		wanted := make(map[string]lanInterface, len(interfaces))
		for _, iface := range interfaces {
			wanted[iface.key()] = iface
		}
		for key, w := range workers {
			_, wantedNow := wanted[key]
			stopped := false
			select {
			case <-w.done:
				stopped = true
			default:
			}
			if !wantedNow || stopped {
				w.cancel()
				<-w.done
				delete(workers, key)
			}
		}
		for key, iface := range wanted {
			if _, ok := workers[key]; ok {
				continue
			}
			workerCtx, cancel := context.WithCancel(ctx)
			done := make(chan struct{})
			workers[key] = worker{cancel, done}
			workerConfig := cfg
			workerConfig.Status = health.begin(key)
			go func() {
				defer close(done)
				// A failed bind or mDNS join can recover on the next scan.
				err := runLANInterface(workerCtx, workerConfig, iface, claims)
				if workerCtx.Err() == nil {
					if err == nil {
						err = errors.New("LAN interface worker exited unexpectedly")
					}
					workerConfig.Status(CarrierStatus{Err: err})
					if cfg.Logger != nil {
						cfg.Logger.Warn("LAN mesh interface stopped", zap.String("interface", iface.iface.Name), zap.Error(err))
					}
				}
			}()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func scanLANInterfaces(ethernet, wifi bool) ([]lanInterface, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var result []lanInterface
	for _, iface := range interfaces {
		if iface.Flags&(net.FlagUp|net.FlagRunning|net.FlagMulticast) != net.FlagUp|net.FlagRunning|net.FlagMulticast {
			continue
		}
		cost, ok := classifyLANInterface(iface.Name, "/sys/class/net", ethernet, wifi, managedWiFi)
		if !ok {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || !ip.IsGlobalUnicast() || ip.IsLinkLocalUnicast() {
				continue
			}
			result = append(result, lanInterface{iface: iface, ip: append(net.IP(nil), ip...), net: ipnet, cost: cost})
		}
	}
	return result, nil
}

func classifyLANInterface(name, sysfs string, ethernet, wifi bool, isManaged func(string) bool) (uint16, bool) {
	base := filepath.Join(sysfs, name)
	if _, err := os.Stat(filepath.Join(base, "wireless")); err == nil {
		if wifi && isManaged(name) {
			return WiFiLinkCost, true
		}
		return 0, false
	}
	if _, err := os.Stat(filepath.Join(base, "phy80211")); err == nil {
		if wifi && isManaged(name) {
			return WiFiLinkCost, true
		}
		return 0, false
	}
	// A device-backed ARPHRD_ETHER interface excludes veth, bridges, dummy,
	// TUN and other virtual interfaces used by apps or mesh carriers.
	if !ethernet {
		return 0, false
	}
	kind, err := os.ReadFile(filepath.Join(base, "type"))
	if err != nil || strings.TrimSpace(string(kind)) != "1" {
		return 0, false
	}
	if _, err := os.Stat(filepath.Join(base, "device")); err != nil {
		return 0, false
	}
	return EthernetLinkCost, true
}

func managedWiFi(name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "iw", "dev", name, "info").Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		if strings.TrimSpace(line) == "type managed" {
			link, err := exec.CommandContext(ctx, "iw", "dev", name, "link").Output()
			return err == nil && strings.Contains(string(link), "Connected to ")
		}
	}
	return false
}

func bindToInterface(name string) func(string, string, syscall.RawConn) error {
	return func(_, _ string, raw syscall.RawConn) error {
		var bindErr error
		if err := raw.Control(func(fd uintptr) {
			bindErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, name)
		}); err != nil {
			return err
		}
		return bindErr
	}
}

func runLANInterface(ctx context.Context, cfg LANConfig, selected lanInterface, claims *lanClaims) error {
	address := net.JoinHostPort(selected.ip.String(), strconv.Itoa(LANPort))
	listener, err := (&net.ListenConfig{Control: bindToInterface(selected.iface.Name)}).Listen(ctx, "tcp4", address)
	if err != nil {
		return fmt.Errorf("LAN %s listen: %w", selected.iface.Name, err)
	}
	defer listener.Close()
	service, err := lanMDNSService(cfg.Credentials, selected)
	if err != nil {
		return err
	}
	mdnsConn, err := listenLANMDNS(ctx, selected)
	if err != nil {
		return fmt.Errorf("LAN %s mDNS: %w", selected.iface.Name, err)
	}
	defer mdnsConn.Close()
	mdnsDone := make(chan error, 1)
	go func() { mdnsDone <- serveLANMDNS(ctx, selected, mdnsConn, service) }()
	if cfg.Status != nil {
		cfg.Status(CarrierStatus{Ready: true})
	}
	if cfg.Logger != nil {
		cfg.Logger.Info("LAN mesh interface ready", zap.String("interface", selected.iface.Name), zap.String("address", address), zap.Uint16("cost", selected.cost))
	}
	var wg sync.WaitGroup
	acceptDone := make(chan struct{})
	defer func() {
		listener.Close()
		<-acceptDone
		wg.Wait()
	}()
	go func() {
		defer close(acceptDone)
		sem := make(chan struct{}, 64)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			select {
			case sem <- struct{}{}:
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() { <-sem }()
					defer conn.Close()
					asset, err := readTCPPreface(conn, cfg.Credentials.Org)
					if err != nil || asset <= 0 || asset >= cfg.Credentials.Asset || !cfg.allow(asset, selected.cost) || !claims.claim(asset, selected.cost) {
						return
					}
					defer claims.release(asset, selected.cost)
					_ = attachLAN(ctx, cfg, selected, asset, conn, true)
				}()
			default:
				conn.Close()
			}
		}
	}()
	go func() {
		<-ctx.Done()
		listener.Close()
	}()
	for ctx.Err() == nil {
		select {
		case err := <-mdnsDone:
			return fmt.Errorf("LAN mDNS responder stopped: %w", err)
		default:
		}
		entries, err := queryLAN(ctx, selected)
		if err != nil && ctx.Err() == nil && cfg.Logger != nil {
			cfg.Logger.Debug("LAN mDNS query failed", zap.String("interface", selected.iface.Name), zap.Error(err))
		}
		for _, entry := range entries {
			asset, ip, ok := parseLANEntry(entry, cfg.Credentials.Org, cfg.Credentials.Asset)
			if !ok || selected.net == nil || !selected.net.Contains(ip) || cfg.Node.HasCheaperLink(asset, selected.cost) || !cfg.allow(asset, selected.cost) || !claims.claim(asset, selected.cost) {
				continue
			}
			if cfg.Logger != nil {
				cfg.Logger.Debug("LAN mesh peer discovered", zap.Int32("asset", asset), zap.String("interface", selected.iface.Name), zap.String("address", ip.String()))
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer claims.release(asset, selected.cost)
				dialer := &net.Dialer{
					Timeout:   4 * time.Second,
					LocalAddr: &net.TCPAddr{IP: selected.ip},
					Control:   bindToInterface(selected.iface.Name),
				}
				conn, err := dialer.DialContext(ctx, "tcp4", net.JoinHostPort(ip.String(), strconv.Itoa(LANPort)))
				if err != nil {
					if cfg.Logger != nil {
						cfg.Logger.Debug("LAN mesh peer dial failed", zap.Int32("asset", asset), zap.Error(err))
					}
					return
				}
				defer conn.Close()
				if writeTCPPreface(conn, cfg.Credentials.Org, cfg.Credentials.Asset) == nil {
					if err := attachLAN(ctx, cfg, selected, asset, conn, false); err != nil && ctx.Err() == nil && cfg.Logger != nil {
						cfg.Logger.Debug("LAN mesh peer link ended", zap.Int32("asset", asset), zap.Error(err))
					}
				}
			}()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(3 * time.Second):
		}
	}
	return nil
}

func listenLANMDNS(ctx context.Context, selected lanInterface) (*net.UDPConn, error) {
	control := func(network, address string, raw syscall.RawConn) error {
		var sockErr error
		if err := raw.Control(func(fd uintptr) {
			for _, option := range []int{unix.SO_REUSEADDR, unix.SO_REUSEPORT} {
				if sockErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, option, 1); sockErr != nil {
					return
				}
			}
			sockErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, selected.iface.Name)
		}); err != nil {
			return err
		}
		return sockErr
	}
	pc, err := (&net.ListenConfig{Control: control}).ListenPacket(ctx, "udp4", ":5353")
	if err != nil {
		return nil, err
	}
	conn := pc.(*net.UDPConn)
	p := ipv4.NewPacketConn(conn)
	if err := p.JoinGroup(&selected.iface, &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251)}); err != nil {
		conn.Close()
		return nil, err
	}
	if err := p.SetMulticastTTL(255); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func serveLANMDNS(ctx context.Context, selected lanInterface, conn *net.UDPConn, zone *mdns.MDNSService) error {
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	buf := make([]byte, 4096)
	for {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if selected.net == nil || !selected.net.Contains(from.IP) {
			continue
		}
		var query dns.Msg
		if err := query.Unpack(buf[:n]); err != nil || query.Response || query.Opcode != dns.OpcodeQuery {
			continue
		}
		for _, question := range query.Question {
			if question.Qclass&0x7fff != dns.ClassINET {
				continue
			}
			records := zone.Records(question)
			if len(records) == 0 {
				continue
			}
			response := new(dns.Msg)
			response.SetReply(&query)
			response.Authoritative = true
			response.RecursionDesired = false
			response.Compress = true
			response.Answer = records
			unicast := question.Qclass&(1<<15) != 0 || from.Port != 5353
			destination := &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}
			if unicast {
				destination = from
			} else {
				response.Id = 0
			}
			packet, err := response.Pack()
			if err == nil {
				_, _ = conn.WriteToUDP(packet, destination)
			}
		}
	}
}

// queryLAN uses an ephemeral, interface-bound UDP socket and requests unicast
// mDNS answers. This avoids relying on a default multicast route and avoids
// accepting responses heard by a wildcard multicast socket on another link.
func queryLAN(ctx context.Context, selected lanInterface) ([]*mdns.ServiceEntry, error) {
	pc, err := (&net.ListenConfig{Control: bindToInterface(selected.iface.Name)}).ListenPacket(ctx, "udp4", net.JoinHostPort(selected.ip.String(), "0"))
	if err != nil {
		return nil, err
	}
	defer pc.Close()
	conn := pc.(*net.UDPConn)
	if err := ipv4.NewPacketConn(conn).SetMulticastTTL(255); err != nil {
		return nil, err
	}
	question := new(dns.Msg)
	question.SetQuestion(lanService+".local.", dns.TypePTR)
	question.Question[0].Qclass |= 1 << 15 // prefer reply to our ephemeral port
	question.RecursionDesired = false
	packet, err := question.Pack()
	if err != nil {
		return nil, err
	}
	if _, err := conn.WriteToUDP(packet, &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}); err != nil {
		return nil, err
	}
	deadline := time.Now().Add(2 * time.Second)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return nil, err
	}
	var entries []*mdns.ServiceEntry
	buf := make([]byte, 4096)
	for len(entries) < 64 {
		n, from, err := conn.ReadFromUDP(buf)
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return entries, nil
			}
			return entries, err
		}
		if from.Port != 5353 || selected.net == nil || !selected.net.Contains(from.IP) {
			continue
		}
		var message dns.Msg
		if err := message.Unpack(buf[:n]); err != nil || !message.Response || message.Id != question.Id {
			continue
		}
		entries = append(entries, entriesFromLANResponse(&message)...)
	}
	return entries, nil
}

func entriesFromLANResponse(message *dns.Msg) []*mdns.ServiceEntry {
	serviceName := lanService + ".local."
	instances := map[string]bool{}
	srv := map[string]*dns.SRV{}
	txt := map[string]*dns.TXT{}
	addresses := map[string]net.IP{}
	for _, record := range append(message.Answer, message.Extra...) {
		switch rr := record.(type) {
		case *dns.PTR:
			if strings.EqualFold(rr.Hdr.Name, serviceName) {
				instances[strings.ToLower(rr.Ptr)] = true
			}
		case *dns.SRV:
			srv[strings.ToLower(rr.Hdr.Name)] = rr
		case *dns.TXT:
			txt[strings.ToLower(rr.Hdr.Name)] = rr
		case *dns.A:
			addresses[strings.ToLower(rr.Hdr.Name)] = rr.A
		}
	}
	var entries []*mdns.ServiceEntry
	for name := range instances {
		s, t := srv[name], txt[name]
		if s == nil || t == nil || addresses[strings.ToLower(s.Target)] == nil {
			continue
		}
		entries = append(entries, &mdns.ServiceEntry{
			Name: name, Host: s.Target, Port: int(s.Port),
			AddrV4: addresses[strings.ToLower(s.Target)], InfoFields: t.Txt,
		})
	}
	return entries
}

func attachLAN(ctx context.Context, cfg LANConfig, selected lanInterface, asset int32, conn net.Conn, server bool) error {
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if cfg.Node.HasCheaperLink(asset, selected.cost) || !cfg.allow(asset, selected.cost) {
					conn.Close()
					return
				}
			}
		}
	}()
	return attachTCPWithCost(ctx, cfg.Node, cfg.Credentials, asset, conn, server, selected.cost)
}

func lanMeshHash(org int32) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("org:%d:default", org)))
	return hex.EncodeToString(sum[:8])
}

func lanMDNSService(c *Credentials, selected lanInterface) (*mdns.MDNSService, error) {
	instance := fmt.Sprintf("wendy-mesh-%d-%d", c.Org, c.Asset)
	host := fmt.Sprintf("wendy-mesh-%d-%d-%d.local.", c.Org, c.Asset, selected.iface.Index)
	txt := []string{
		fmt.Sprintf("org=%d", c.Org),
		fmt.Sprintf("asset=%d", c.Asset),
		"mesh=" + lanMeshHash(c.Org),
	}
	return mdns.NewMDNSService(instance, lanService, "local.", host, LANPort, []net.IP{selected.ip}, txt)
}

func parseLANEntry(entry *mdns.ServiceEntry, org, self int32) (int32, net.IP, bool) {
	if entry == nil || entry.Port != LANPort || entry.AddrV4 == nil {
		return 0, nil, false
	}
	ip := entry.AddrV4.To4()
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsLinkLocalUnicast() {
		return 0, nil, false
	}
	fields := make(map[string]string, len(entry.InfoFields))
	for _, field := range entry.InfoFields {
		key, value, ok := strings.Cut(field, "=")
		if !ok || key == "" || fields[key] != "" {
			return 0, nil, false
		}
		fields[key] = value
	}
	if fields["org"] != strconv.Itoa(int(org)) || fields["mesh"] != lanMeshHash(org) {
		return 0, nil, false
	}
	asset, err := strconv.Atoi(fields["asset"])
	if err != nil || asset <= int(self) || asset > 65534 {
		return 0, nil, false
	}
	return int32(asset), ip, true
}
