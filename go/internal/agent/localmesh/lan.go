//go:build linux || darwin

package localmesh

import (
	"context"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hashicorp/mdns"
	"github.com/miekg/dns"
	quic "github.com/quic-go/quic-go"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshdiscovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/tsarelay"
	"go.uber.org/zap"
	"golang.org/x/net/ipv4"
)

const (
	LANPort             = 43023
	EthernetLinkCost    = uint16(64)
	WiFiLinkCost        = uint16(128)
	lanService          = "_wendy-net._udp"
	lanDiscoveryVersion = "1"
	lanProtocol         = "quic-v1"
	maxLANIncoming      = 64
	lanInterfacePeriod  = 5 * time.Second
)

// LANConfig enables discovery independently for physical Ethernet and
// infrastructure Wi-Fi. Advertisements are only hints: the QUIC TLS handshake
// authenticates the full enrolled tenant/device principal before Node admits a link.
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
	// OperatorCandidates supplies untrusted agent-discovery hints to a desktop
	// operator. Each hint still requires the native QUIC protocol and an exact
	// same-tenant TLS principal before any mesh traffic is exchanged.
	OperatorCandidates func() []LANPeer
}

type LANPeer struct {
	Principal string
	IP        net.IP
}

// Device-to-device links remain on-link. Operators may connect through routed
// Ethernet/Wi-Fi networks, but only after TLS proves both identity and tenant.
func lanPeerAddressAllowed(self, peer meshid.Device, ip net.IP, subnet *net.IPNet) bool {
	return subnet != nil && ip.To4() != nil &&
		self != peer && self.Tenant() == peer.Tenant() &&
		(subnet.Contains(ip) || (ip.IsGlobalUnicast() && !ip.IsLinkLocalUnicast() && (self.IsOperator() || peer.IsOperator())))
}

func operatorLANEntries(cfg LANConfig) []*mdns.ServiceEntry {
	if !cfg.Credentials.Device.IsOperator() || cfg.OperatorCandidates == nil {
		return nil
	}
	var entries []*mdns.ServiceEntry
	for _, hint := range cfg.OperatorCandidates() {
		peer, err := meshid.DeviceFromPrincipal(hint.Principal)
		if err != nil || peer.Tenant() != cfg.Credentials.Device.Tenant() {
			continue
		}
		entries = append(entries, &mdns.ServiceEntry{Port: LANPort, AddrV4: hint.IP,
			InfoFields: []string{"principal=" + peer.Principal(), "mesh=" + lanMeshHash(peer.Tenant()), "protocol=" + lanProtocol}})
		if len(entries) == 128 {
			break
		}
	}
	return entries
}

func (c LANConfig) allow(asset meshid.Device, cost uint16) bool {
	return c.Selection == nil || c.Selection.AllowLAN(asset, cost)
}

type lanInterface struct {
	iface     net.Interface
	ip        net.IP
	net       *net.IPNet
	cost      uint16
	transport string
}

type lanClaims struct {
	policy     *ConnectionPolicy
	candidates CandidateQueue
	incoming   atomic.Uint64
}

func (i lanInterface) key() string {
	return fmt.Sprintf("%d/%s/%s/%s/%d", i.iface.Index, i.iface.Name, i.ip.String(), i.net, i.cost)
}

// RunLAN watches eligible interfaces, so adding/removing an Ethernet cable or
// changing an infrastructure Wi-Fi address starts/stops the corresponding
// discovery listener without restarting the mesh agent.
func RunLAN(ctx context.Context, cfg LANConfig) error {
	if cfg.Credentials == nil || cfg.Node == nil {
		return errors.New("LAN carrier requires credentials and node")
	}
	return runLANWithScan(ctx, cfg, func() ([]lanInterface, error) {
		return scanLANInterfaces(cfg.Ethernet, cfg.InfrastructureWiFi)
	})
}

func runLANWithScan(ctx context.Context, cfg LANConfig, scan func() ([]lanInterface, error)) error {
	if cfg.Selection == nil {
		cfg.Selection = NewPeerSelection(cfg.Node.Snapshot)
	}
	claims := &lanClaims{policy: NewConnectionPolicy(cfg.Credentials.Device, true, ConnectionLimits{LANMaxOutgoing, LANMaxOutgoingAttempts, LANMaxConnections})}
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
						cfg.Logger.Warn("LAN carrier interface stopped", zap.String("interface", iface.iface.Name), zap.Error(err))
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

func runLANInterface(parent context.Context, cfg LANConfig, selected lanInterface, claims *lanClaims) error {
	if selected.transport == "usb" {
		return runUSBInterface(parent, cfg, selected, claims)
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	address := net.JoinHostPort(selected.ip.String(), strconv.Itoa(LANPort))
	socket, err := listenLANQUIC(ctx, selected, address)
	if err != nil {
		return fmt.Errorf("LAN %s listen: %w", selected.iface.Name, err)
	}
	defer socket.Close()
	service, err := lanMDNSService(cfg.Credentials, selected)
	if err != nil {
		return err
	}
	tcp, err := (&net.ListenConfig{Control: bindToInterface(selected.iface.Name)}).Listen(ctx, "tcp4", net.JoinHostPort(selected.ip.String(), strconv.Itoa(TLSLANPort)))
	if err != nil {
		return fmt.Errorf("LAN TLS listen: %w", err)
	}
	defer tcp.Close()
	timeListener, timeListenErr := (&net.ListenConfig{Control: bindToInterface(selected.iface.Name)}).Listen(ctx, "tcp4", net.JoinHostPort(selected.ip.String(), strconv.Itoa(tsarelay.Port)))
	var timePort uint16
	if timeListenErr == nil {
		defer timeListener.Close()
		timePort = tsarelay.Port
	} else if cfg.Logger != nil {
		cfg.Logger.Warn("LAN signed-time relay unavailable", zap.String("interface", selected.iface.Name), zap.Error(timeListenErr))
	}
	tlsZone, err := tlsLANMDNSService(cfg.Credentials, selected, "router", timePort)
	if err != nil {
		return err
	}
	mdnsConn, err := listenLANMDNS(ctx, selected)
	if err != nil {
		return fmt.Errorf("LAN %s mDNS: %w", selected.iface.Name, err)
	}
	defer mdnsConn.Close()
	tlsDone, scanDone := make(chan error, 1), make(chan struct{})
	timeDone := make(chan error, 1)
	if timeListener != nil {
		go func() { timeDone <- tsarelay.Serve(ctx, timeListener); cancel() }()
	} else {
		timeDone <- nil
	}
	go func() {
		err := runNetTLSListener(WithCarrier(ctx, selected.transport), cfg.Node, tcp, selected.cost, claims.policy, strconv.Itoa(int(selected.cost)))
		tlsDone <- err
		cancel()
	}()
	go func() { defer close(scanDone); scanTLSLAN(ctx, cfg, selected, claims) }()
	quicErr := serveLANInterface(ctx, cfg, selected, claims, socket, mdnsConn, lanZones{service, tlsZone})
	cancel()
	_ = tcp.Close()
	tlsErr := <-tlsDone
	timeErr := <-timeDone
	<-scanDone
	return errors.Join(quicErr, tlsErr, timeErr)
}

// listenLANQUIC binds the real UDP endpoint to both the selected address and
// physical interface. Simulator TCP is never used by the production LAN path.
func listenLANQUIC(ctx context.Context, selected lanInterface, address string) (net.PacketConn, error) {
	return (&net.ListenConfig{Control: bindToInterface(selected.iface.Name)}).ListenPacket(ctx, "udp4", address)
}

// Reserve the shared LAN budget before TLS allocates a connection. The peer
// stays unknown until verified TLS, so a spoofed hint cannot suppress its dials.
// QUIC's connection context ends after failed handshakes and peer teardown.
// Off-link sources must pass lanPeerAddressAllowed after TLS proves an operator.
func lanIncomingContext(selected lanInterface, claims *lanClaims) func(context.Context, *quic.ClientInfo) (context.Context, error) {
	return func(ctx context.Context, info *quic.ClientInfo) (context.Context, error) {
		source, ok := info.RemoteAddr.(*net.UDPAddr)
		if !ok || selected.net == nil || source.IP.To4() == nil || (!selected.net.Contains(source.IP) && (!source.IP.IsGlobalUnicast() || source.IP.IsLinkLocalUnicast())) {
			return nil, errors.New("LAN source is not an eligible IPv4 address")
		}
		lease := claims.policy.ReserveGroup(fmt.Sprintf("incoming:%d", claims.incoming.Add(1)), meshid.Device{}, false, strconv.Itoa(int(selected.cost)))
		if lease == nil {
			return nil, errors.New("LAN incoming connection limit")
		}
		context.AfterFunc(ctx, lease.Release)
		return WithConnectionLease(ctx, lease), nil
	}
}

// One interface owns one UDP socket and QUIC transport for all peer connections.
// A peer's end must never close that shared endpoint. Every worker exit stops
// discovery, pending handshakes and attached links before the scanner restarts it.
func serveLANInterface(parent context.Context, cfg LANConfig, selected lanInterface, claims *lanClaims, socket net.PacketConn, mdnsConn *net.UDPConn, service lanMDNSZone) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	defer socket.Close()
	defer mdnsConn.Close()
	transport := &quic.Transport{Conn: socket, ConnContext: lanIncomingContext(selected, claims), VerifySourceAddress: func(net.Addr) bool { return true }}
	defer transport.Close()
	tlsConfig, err := lanServerTLS(cfg.Credentials)
	if err != nil {
		return err
	}
	listener, err := transport.Listen(tlsConfig, QUICConfig())
	if err != nil {
		return fmt.Errorf("LAN QUIC listen: %w", err)
	}
	defer listener.Close()
	if ctx.Err() != nil {
		return nil
	}
	if cfg.Status != nil {
		cfg.Status(CarrierStatus{Ready: true})
	}
	if cfg.Logger != nil {
		cfg.Logger.Info("LAN UDP QUIC interface ready", zap.String("interface", selected.iface.Name), zap.String("address", socket.LocalAddr().String()), zap.Uint16("cost", selected.cost))
	}
	var wg sync.WaitGroup
	fatal := make(chan error, 1)
	fail := func(err error) {
		select {
		case fatal <- err:
		default:
		}
		cancel()
	}
	mdnsDone, acceptDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(mdnsDone)
		if err := serveLANMDNS(ctx, selected, mdnsConn, service); err != nil && ctx.Err() == nil {
			fail(fmt.Errorf("LAN mDNS responder stopped: %w", err))
		}
	}()
	go func() {
		defer close(acceptDone)
		for {
			conn, err := listener.Accept(ctx)
			if err != nil {
				if ctx.Err() == nil {
					fail(fmt.Errorf("LAN QUIC listener stopped: %w", err))
				}
				return
			}
			if conn == nil {
				fail(errors.New("LAN QUIC listener returned no authenticated connection"))
				return
			}
			lease, _ := conn.Context().Value(connectionLeaseKey{}).(*ConnectionLease)
			asset, err := lanAuthenticatedPeer(cfg.Credentials, conn.ConnectionState().TLS)
			remote, remoteOK := conn.RemoteAddr().(*net.UDPAddr)
			if err != nil || lease == nil || !remoteOK || !lanPeerAddressAllowed(cfg.Credentials.Device, asset, remote.IP, selected.net) || !cfg.allow(asset, selected.cost) {
				conn.CloseWithError(1, "LAN peer not admitted")
				continue
			}
			// The first identity associated with this inbound lease is the
			// authenticated full principal, never an mDNS address or preface.
			lease.Identify(asset)
			lease.Progress()
			lease.ObserveReady(func() { claims.candidates.Succeeded(asset.Principal()) })
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer lease.Release()
				defer func() {
					if lease.WasReady() && !lease.Retired() && ctx.Err() == nil && cfg.allow(asset, selected.cost) && !cfg.Node.HasCheaperLink(asset, selected.cost) {
						claims.candidates.Failed(asset.Principal(), time.Now())
					}
				}()
				_ = attachLAN(WithConnectionLease(ctx, lease), cfg, selected, asset, conn)
			}()
		}
	}()
	stop := context.AfterFunc(ctx, func() { transport.Close(); socket.Close() })
	defer func() {
		cancel()
		transport.Close()
		socket.Close()
		mdnsConn.Close()
		stop()
		<-acceptDone
		<-mdnsDone
		wg.Wait()
	}()
	for ctx.Err() == nil {
		entries, err := queryLAN(ctx, selected)
		if err != nil && ctx.Err() == nil && cfg.Logger != nil {
			cfg.Logger.Debug("LAN mDNS query failed", zap.String("interface", selected.iface.Name), zap.Error(err))
		}
		entries = append(operatorLANEntries(cfg), entries...)
		byKey, keys := lanCandidateEntries(entries, cfg.Credentials.Device, selected.net)
		for _, candidateKey := range claims.candidates.Order(keys, time.Now()) {
			entry := byKey[candidateKey]
			asset, ip, ok := parseLANEntry(entry, cfg.Credentials.Device)
			if !ok || ctx.Err() != nil || !lanPeerAddressAllowed(cfg.Credentials.Device, asset, ip, selected.net) || cfg.Node.HasCheaperLink(asset, selected.cost) || !cfg.allow(asset, selected.cost) {
				continue
			}
			lease := claims.policy.ReserveGroup(asset.Principal()+"/"+strconv.Itoa(int(selected.cost)), asset, true, strconv.Itoa(int(selected.cost)))
			if lease == nil {
				continue
			}
			lease.ObserveReady(func() { claims.candidates.Succeeded(candidateKey) })
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer lease.Release()
				attemptCtx := WithConnectionLease(ctx, lease)
				defer func() {
					if ctx.Err() == nil && !lease.Retired() && cfg.allow(asset, selected.cost) && !cfg.Node.HasCheaperLink(asset, selected.cost) {
						claims.candidates.Failed(candidateKey, time.Now())
					}
				}()
				tlsConfig, err := cfg.Credentials.PeerTLSWithTickets(asset, LinkALPN, LinkQUICSessionScope)
				if err != nil {
					return
				}
				handshake, cancel := context.WithTimeout(attemptCtx, 8*time.Second)
				conn, err := transport.Dial(handshake, &net.UDPAddr{IP: ip, Port: LANPort}, tlsConfig, QUICConfig())
				cancel()
				if err != nil {
					if ctx.Err() == nil && cfg.Logger != nil {
						cfg.Logger.Debug("LAN UDP QUIC dial failed", zap.String("device", asset.Principal()), zap.Error(err))
					}
					return
				}
				if err := attachLAN(attemptCtx, cfg, selected, asset, conn); err != nil && ctx.Err() == nil && cfg.Logger != nil {
					cfg.Logger.Debug("LAN peer link ended", zap.String("device", asset.Principal()), zap.Error(err))
				}
			}()
		}
		select {
		case <-ctx.Done():
		case <-time.After(3 * time.Second):
		}
	}
	if parent.Err() != nil {
		return nil
	}
	select {
	case err := <-fatal:
		return err
	default:
		return nil
	}
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

		}); err != nil {
			return err
		}
		if sockErr != nil {
			return sockErr
		}
		return bindToInterface(selected.iface.Name)(network, address, raw)
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

func serveLANMDNS(ctx context.Context, selected lanInterface, conn *net.UDPConn, zone lanMDNSZone) error {
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
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
	return queryLANService(ctx, selected, lanService)
}
func queryLANService(ctx context.Context, selected lanInterface, service string) ([]*mdns.ServiceEntry, error) {
	pc, err := (&net.ListenConfig{Control: bindToInterface(selected.iface.Name)}).ListenPacket(ctx, "udp4", net.JoinHostPort(selected.ip.String(), "0"))
	if err != nil {
		return nil, err
	}
	defer pc.Close()
	conn := pc.(*net.UDPConn)
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err := ipv4.NewPacketConn(conn).SetMulticastTTL(255); err != nil {
		return nil, err
	}
	question := new(dns.Msg)
	question.SetQuestion(service+".local.", dns.TypePTR)
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
		entries = append(entries, entriesFromLANServiceResponse(&message, service)...)
	}
	return entries, nil
}

func entriesFromLANResponse(message *dns.Msg) []*mdns.ServiceEntry {
	return entriesFromLANServiceResponse(message, lanService)
}
func entriesFromLANServiceResponse(message *dns.Msg, service string) []*mdns.ServiceEntry {
	serviceName := service + ".local."
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

func attachLAN(ctx context.Context, cfg LANConfig, selected lanInterface, asset meshid.Device, conn *quic.Conn) error {
	// Close only this peer connection; the interface retains the UDP transport.
	defer conn.CloseWithError(0, "LAN link ended")
	stop := context.AfterFunc(ctx, func() { conn.CloseWithError(0, "LAN interface stopped") })
	defer stop()
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				if cfg.Node.HasCheaperLink(asset, selected.cost) || !cfg.allow(asset, selected.cost) {
					conn.CloseWithError(0, "LAN peer selection changed")
					return
				}
			}
		}
	}()
	return cfg.Node.AttachWithCost(WithCarrier(ctx, selected.transport), asset, conn, selected.cost)
}

func lanMeshHash(tenant meshid.Tenant) string {
	return meshdiscovery.DefaultHint(tenant).String()
}
func lanMDNSService(c *Credentials, selected lanInterface) (*mdns.MDNSService, error) {
	instance := fmt.Sprintf("wendy-net-%s", c.Device.UUID())
	host := fmt.Sprintf("wendy-net-%s-%d.local.", c.Device.UUID(), selected.iface.Index)
	txt := []string{"platform=WendyOS", "protocol=" + lanProtocol, "principal=" + c.Device.Principal(), "mesh=" + lanMeshHash(c.Device.Tenant()), "version=" + lanDiscoveryVersion}
	return mdns.NewMDNSService(instance, lanService, "local.", host, LANPort, []net.IP{selected.ip}, txt)
}

func lanCandidateEntries(entries []*mdns.ServiceEntry, self meshid.Device, subnet *net.IPNet) (map[string]*mdns.ServiceEntry, []string) {
	byKey := make(map[string]*mdns.ServiceEntry)
	var keys []string
	for _, entry := range entries {
		asset, ip, ok := parseLANEntry(entry, self)
		// Filter before deduplication: a later off-subnet advertisement for
		// the same principal must not hide its usable local address.
		if !ok || !lanPeerAddressAllowed(self, asset, ip, subnet) {
			continue
		}
		key := asset.Principal()
		if _, exists := byKey[key]; !exists {
			keys = append(keys, key)
		}
		byKey[key] = entry
	}
	return byKey, keys
}

func parseLANEntry(entry *mdns.ServiceEntry, self meshid.Device) (meshid.Device, net.IP, bool) {
	if entry == nil || entry.Port != LANPort || entry.AddrV4 == nil {
		return meshid.Device{}, nil, false
	}
	ip := entry.AddrV4.To4()
	if ip == nil || !ip.IsGlobalUnicast() || ip.IsLinkLocalUnicast() {
		return meshid.Device{}, nil, false
	}
	fields := make(map[string]string, len(entry.InfoFields))
	for _, field := range entry.InfoFields {
		key, value, ok := strings.Cut(field, "=")
		// DNS-SD TXT keys are case-insensitive (RFC 6763, section 6.4).
		// Normalize before checking versions and duplicates so a differently
		// cased version cannot be mistaken for a versionless advertisement.
		key = strings.ToLower(key)
		if _, duplicate := fields[key]; !ok || key == "" || duplicate {
			return meshid.Device{}, nil, false
		}
		fields[key] = value
	}
	// Versionless advertisements still require this native UDP protocol and
	// principal/mesh contract. Unknown explicit versions never reach setup.
	if version, present := fields["version"]; present && version != lanDiscoveryVersion {
		return meshid.Device{}, nil, false
	}
	device, err := meshid.PeerFromPrincipal(fields["principal"])
	if err != nil || device.Tenant() != self.Tenant() || fields["mesh"] != lanMeshHash(self.Tenant()) || fields["protocol"] != lanProtocol || device == self {
		return meshid.Device{}, nil, false
	}
	return device, ip, true
}
