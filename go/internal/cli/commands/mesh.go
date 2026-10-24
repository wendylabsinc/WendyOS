package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/cli/netmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
	"github.com/wendylabsinc/wendy/go/proto/gen/agentpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
)

const meshWorkerEnv = "WENDY_INTERNAL_MESH_SESSION"
const meshEndpointEnv = "WENDY_INTERNAL_MESH_ENDPOINT"

// BLE may carry the TLS certificate chain and RPC over several mesh hops.
const meshConnectTimeout = time.Minute

// Keep a recently reachable row through a short routing reconvergence. This
// affects presentation only: dialing still requires a currently authorized route.
const meshPresenceGrace = 15 * time.Second

var meshStartMu sync.Mutex

// Embedders supply the bundled CLI because their executable is not a CLI worker.
var meshWorkerExecutable = os.Executable

// selectedMeshCertificate is deliberately non-interactive and never enrolls or
// signs in. Only the selected organization's existing operator session can join.
func selectedMeshCertificate(cfg *config.Config, endpoint string) (config.CertificateInfo, bool) {
	auth, err := config.ResolveAuth(cfg, endpoint, nil)
	if err != nil {
		return config.CertificateInfo{}, false
	}
	for _, certificate := range auth.Certificates {
		peer, err := meshid.PeerFromPrincipal(certificate.PrincipalURI)
		if err == nil && peer.IsOperator() && certificate.PemCertificate != "" && certificate.PemPrivateKey != "" && certificate.PemCertificateChain != "" {
			return certificate, true
		}
	}
	return config.CertificateInfo{}, false
}
func meshFingerprint(c config.CertificateInfo) string {
	sum := sha256.Sum256([]byte(c.PrincipalURI + "\x00" + c.PemCertificate + "\x00" + c.PemCertificateChain))
	return hex.EncodeToString(sum[:])
}
func meshDirectory(c config.CertificateInfo) (string, error) {
	base, err := config.ConfigDir()
	if err != nil {
		return "", err
	}
	principal := sha256.Sum256([]byte(c.PrincipalURI))
	return filepath.Join(base, "mesh", hex.EncodeToString(principal[:12])), nil
}
func meshSelection(endpoint string) (config.CertificateInfo, bool) {
	cfg, err := config.Load()
	if err != nil {
		return config.CertificateInfo{}, false
	}
	return selectedMeshCertificate(cfg, endpoint)
}

// Refresh only the selected operator session before starting discovery. The
// worker's repeated selection checks remain read-only; the caller owns renewal.
func prepareMeshCertificate(ctx context.Context, endpoint string) (config.CertificateInfo, bool, error) {
	cfg, err := config.Load()
	if err != nil {
		return config.CertificateInfo{}, false, err
	}
	if _, ok := selectedMeshCertificate(cfg, endpoint); !ok {
		return config.CertificateInfo{}, false, nil
	}
	auth, err := config.ResolveAuth(cfg, endpoint, nil)
	if err != nil {
		return config.CertificateInfo{}, false, err
	}
	// This serializes renewal with other auth writers, reloads the session,
	// and persists rotated OAuth tokens even when certificate issuance fails.
	if err := prepareDefaultCloudCertificate(ctx, auth); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return config.CertificateInfo{}, false, err
		}
		return config.CertificateInfo{}, false, fmt.Errorf("refreshing Wendy Net operator certificate: %w; run 'wendy auth refresh-certs' or sign in again with 'wendy auth login'", err)
	}
	certificate, ok := selectedMeshCertificate(cfg, endpoint)
	return certificate, ok, nil
}

// RunMeshWorker handles an internal re-exec, before command construction. There
// is no user command or separately enrolled desktop identity. The worker reads
// credentials from the same protected configuration as the calling CLI.
func RunMeshWorker() bool {
	fingerprint := os.Getenv(meshWorkerEnv)
	if fingerprint == "" || len(os.Args) != 1 {
		return false
	}
	endpoint := os.Getenv(meshEndpointEnv)
	certificate, ok := meshSelection(endpoint)
	if !ok || meshFingerprint(certificate) != fingerprint {
		return true
	}
	credentials, err := localmesh.NewOperatorCredentials(certificate.PemCertificate, certificate.PemCertificateChain, certificate.PemPrivateKey)
	if err != nil {
		return true
	}
	if credentials.Device.Principal() != certificate.PrincipalURI {
		return true
	}
	dir, err := meshDirectory(certificate)
	if err != nil {
		return true
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	// Bound the session to the earliest expiry in the verified chain.
	identity, err := credentials.Verify(credentials.Certificate.Certificate, time.Now())
	if err != nil {
		return true
	}
	executable, err := os.Executable()
	if err != nil {
		return true
	}
	workerBinary, err := os.Stat(executable)
	if err != nil {
		return true
	}
	valid := func() bool {
		if !meshWorkerBinaryCurrent(executable, workerBinary) {
			return false
		}
		current, ok := meshSelection(endpoint)
		return ok && meshFingerprint(current) == fingerprint && time.Now().Before(identity.NotAfter)
	}
	_ = netmesh.Serve(ctx, dir, func(ctx context.Context) (*netmesh.Client, error) { return netmesh.New(ctx, dir, credentials) }, valid)
	return true
}

// Go builds replace the executable atomically. Retire the old worker so an
// already-open discovery view reconnects to the locally rebuilt CLI.
func meshWorkerBinaryCurrent(path string, initial os.FileInfo) bool {
	current, err := os.Stat(path)
	return err == nil && os.SameFile(initial, current) && initial.Size() == current.Size() && initial.ModTime().Equal(current.ModTime())
}
func ensureMeshSession(ctx context.Context) (string, config.CertificateInfo, error) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return "", config.CertificateInfo{}, nil
	}
	certificate, ok, err := prepareMeshCertificate(ctx, "")
	if err != nil {
		return "", certificate, err
	}
	if !ok {
		return "", certificate, nil
	}
	// Validate before any radio or worker starts, including certificate principal,
	// issuer lifetime, key possession, and organization scope.
	credentials, err := localmesh.NewOperatorCredentials(certificate.PemCertificate, certificate.PemCertificateChain, certificate.PemPrivateKey)
	if err != nil {
		return "", certificate, err
	}
	if credentials.Device.Principal() != certificate.PrincipalURI {
		return "", certificate, errors.New("operator certificate does not match selected organization identity")
	}
	dir, err := meshDirectory(certificate)
	if err != nil {
		return "", certificate, err
	}
	meshStartMu.Lock()
	defer meshStartMu.Unlock()
	sum := sha256.Sum256(credentials.Certificate.Certificate[0])
	identity := hex.EncodeToString(sum[:])
	probe, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	status, err := netmesh.ReadStatus(probe, dir)
	cancel()
	if err == nil && status.Identity == identity {
		return dir, certificate, nil
	}
	if err = netmesh.PrivateDirectory(dir); err != nil {
		return "", certificate, err
	}
	exe, err := meshWorkerExecutable()
	if err != nil {
		return "", certificate, err
	}
	worker := exec.Command(exe)
	worker.Env = append(os.Environ(), meshWorkerEnv+"="+meshFingerprint(certificate), meshEndpointEnv+"=")
	detachMeshWorker(worker)
	if err = worker.Start(); err != nil {
		return "", certificate, err
	}
	go func() { _ = worker.Wait() }()
	waitCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-waitCtx.Done():
			return "", certificate, fmt.Errorf("starting organization mesh: %w", waitCtx.Err())
		case <-ticker.C:
			probe, cancel := context.WithTimeout(waitCtx, 300*time.Millisecond)
			status, err := netmesh.ReadStatus(probe, dir)
			cancel()
			if err == nil && status.Identity == identity {
				return dir, certificate, nil
			}
		}
	}
}
func meshDeviceRow(d netmesh.Device) models.LANDevice {
	row := models.LANDevice{ID: d.ID.String(), DisplayName: meshDisplayName(d.Name), Hostname: "mesh:" + d.ID.String(), IPAddress: d.Address.String(), Port: int(d.Port), IsMTLS: true, IsWendyDevice: true, Mesh: true}
	if a := d.Agent; a != nil {
		row.AgentVersion, row.OS, row.OSVersion = a.Version, a.OS, a.OSVersion
		row.DeviceType, row.CPUArchitecture = a.DeviceType, a.CPUArchitecture
	}
	return row
}

func meshDisplayName(name string) string {
	name = discovery.SanitiseDisplayName(name)
	name = strings.TrimPrefix(strings.TrimSuffix(name, ".local"), "wendyos-")
	words := strings.Fields(strings.ReplaceAll(name, "-", " "))
	for i, word := range words {
		chars := []rune(word)
		chars[0] = unicode.ToUpper(chars[0])
		words[i] = string(chars)
	}
	return strings.Join(words, " ")
}

func meshDiscoveryStream(ctx context.Context) <-chan discovery.LANEvent {
	events := meshDiscoveryStreamWith(ctx, time.Second, func(ctx context.Context) (string, error) {
		dir, _, err := ensureMeshSession(ctx)
		return dir, err
	}, netmesh.ReadStatus)
	return probeMeshDiscovery(ctx, events, lanProber, 30*time.Second)
}

// A transient IPC failure must not permanently detach a running discovery view.
// Re-resolve the organization before reconnecting, and retract stale routes while
// the worker is unavailable. An empty selection ends participation.
func meshDiscoveryStreamWith(ctx context.Context, interval time.Duration, ensure func(context.Context) (string, error), read func(context.Context, string) (netmesh.Status, error)) <-chan discovery.LANEvent {
	out := make(chan discovery.LANEvent)
	go func() {
		defer close(out)
		dir := ""
		seen := map[string]models.LANDevice{}
		missing := map[string]time.Time{}
		send := func(event discovery.LANEvent) bool {
			select {
			case <-ctx.Done():
				return false
			case out <- event:
				return true
			}
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			var err error
			if dir == "" {
				dir, err = ensure(ctx)
				if err == nil && dir == "" {
					return
				}
			}
			var status netmesh.Status
			if err == nil {
				probe, cancel := context.WithTimeout(ctx, 2*time.Second)
				status, err = read(probe, dir)
				cancel()
			}
			if err != nil {
				for _, row := range seen {
					if !send(discovery.LANEvent{Kind: discovery.LANRetracted, Device: row}) {
						return
					}
				}
				seen = map[string]models.LANDevice{}
				clear(missing)
				dir = ""
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					continue
				}
			}
			next := map[string]models.LANDevice{}
			for _, d := range status.Devices {
				row := meshDeviceRow(d)
				delete(missing, row.ID)
				next[row.ID] = row
				if old, ok := seen[row.ID]; !ok || old.DisplayName != row.DisplayName || old.Port != row.Port || old.AgentVersion != row.AgentVersion || old.OS != row.OS || old.OSVersion != row.OSVersion || old.DeviceType != row.DeviceType || old.CPUArchitecture != row.CPUArchitecture {
					if !send(discovery.LANEvent{Kind: discovery.LANFound, Device: row, Probed: d.Agent != nil}) {
						return
					}
				}
			}
			for id, row := range seen {
				if _, ok := next[id]; !ok {
					if missing[id].IsZero() {
						missing[id] = time.Now()
					}
					if time.Since(missing[id]) < meshPresenceGrace {
						next[id] = row
						continue
					}
					delete(missing, id)
					if !send(discovery.LANEvent{Kind: discovery.LANRetracted, Device: row}) {
						return
					}
				}
			}
			seen = next
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return out
}
func connectMeshSelector(ctx context.Context, selector string) (*SelectedDevice, error) {
	name := strings.TrimPrefix(selector, "mesh:")
	if name == "" || strings.ContainsAny(name, "/\\\r\n\t") {
		return nil, errors.New("invalid mesh device selector")
	}
	dir, certificate, err := ensureMeshSession(ctx)
	if err != nil {
		return nil, err
	}
	if dir == "" {
		return nil, errors.New("mesh access requires a selected organization and operator certificate")
	}
	waitCtx, cancel := context.WithTimeout(ctx, meshConnectTimeout)
	defer cancel()
	var device netmesh.Device
	for {
		status, err := netmesh.ReadStatus(waitCtx, dir)
		if err != nil {
			return nil, err
		}
		device, err = netmesh.Resolve(status.Devices, name)
		if err == nil {
			break
		}
		if strings.Contains(err.Error(), "ambiguous") {
			return nil, err
		}
		select {
		case <-waitCtx.Done():
			return nil, err
		case <-time.After(300 * time.Millisecond):
		}
	}
	expected, err := certs.ParseDevicePrincipal(certs.DeviceSPIFFEURI(certificate.TenantUUID(), device.ID.String()))
	if err != nil {
		return nil, err
	}
	address := netip.AddrPortFrom(device.Address, device.Port).String()
	dial := func(ctx context.Context, _ string) (net.Conn, error) {
		return netmesh.Dial(ctx, dir, device.ID.String(), device.Port)
	}
	connection, err := grpcclient.ConnectWithTLSExpecting(waitCtx, address, &certificate, openPinStore(), &expected,
		grpc.WithContextDialer(dial),
		grpc.WithConnectParams(grpc.ConnectParams{Backoff: backoff.DefaultConfig, MinConnectTimeout: meshConnectTimeout}))
	if err != nil {
		return nil, err
	}
	version, err := connection.AgentService.GetAgentVersion(waitCtx, &agentpb.GetAgentVersionRequest{})
	if err != nil {
		connection.Close()
		return nil, err
	}
	connection.CacheAgentVersion(version)
	connection.RegistryDialer = func(ctx context.Context, port int) (net.Conn, error) {
		if port <= 0 || port > 65535 {
			return nil, errors.New("invalid registry port")
		}
		return netmesh.Dial(ctx, dir, device.ID.String(), uint16(port))
	}
	connection.Reconnect = func(ctx context.Context) (*grpcclient.AgentConnection, error) {
		picked, err := connectMeshSelector(ctx, "mesh:"+device.ID.String())
		if err != nil {
			return nil, err
		}
		return picked.Agent, nil
	}
	picked := &SelectedDevice{Agent: connection, DefaultSelector: "mesh:" + device.ID.String(), PinKey: "mesh:" + certificate.TenantUUID() + ":" + name}
	if err := enforceSelectedDevicePin(picked); err != nil {
		return nil, err
	}
	return picked, nil
}

// A bare name selected from discovery can reuse its authenticated mesh roster.
// Unknown names retain the existing LAN/cloud resolution behavior.
func connectDiscoveredMeshName(ctx context.Context, name string) (*SelectedDevice, bool, error) {
	if name == "" || strings.ContainsAny(name, ".:/\\") {
		return nil, false, nil
	}
	certificate, ok := meshSelection("")
	if !ok {
		return nil, false, nil
	}
	dir, err := meshDirectory(certificate)
	if err != nil {
		return nil, false, nil
	}
	probe, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	status, err := netmesh.ReadStatus(probe, dir)
	if err != nil {
		return nil, false, nil
	}
	matches := 0
	var selectedID string
	for _, device := range status.Devices {
		if device.Name == name || meshDisplayName(device.Name) == name || device.ID.String() == name {
			matches++
			selectedID = device.ID.String()
		}
	}
	if matches == 0 {
		return nil, false, nil
	}
	if matches > 1 {
		return nil, true, fmt.Errorf("ambiguous mesh device %q; use its UUID", name)
	}
	picked, err := connectMeshSelector(ctx, "mesh:"+selectedID)
	return picked, true, err
}
