package commands

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/cli/netmesh"
	"github.com/wendylabsinc/wendy/go/internal/cli/sessionbroker"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
	"github.com/wendylabsinc/wendy/go/internal/shared/models"
)

// BLE may carry the TLS certificate chain and RPC over several mesh hops.
const meshConnectTimeout = time.Minute

// Keep a recently reachable row through a short routing reconvergence. This
// affects presentation only: dialing still requires a currently authorized route.
const meshPresenceGrace = 15 * time.Second

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

// Go builds replace the executable atomically. Retire the old worker so an
// already-open discovery view reconnects to the locally rebuilt CLI.
func meshWorkerBinaryCurrent(path string, initial os.FileInfo) bool {
	current, err := os.Stat(path)
	return err == nil && os.SameFile(initial, current) && initial.Size() == current.Size() && initial.ModTime().Equal(current.ModTime())
}
func ensureMeshSession(ctx context.Context) (string, config.CertificateInfo, error) {
	certificate, ok := meshSelection("")
	if !ok {
		return "", certificate, nil
	}
	owner, err := sessionbroker.PrepareOperatorAutomatic(ctx, certificate)
	if err != nil {
		return "", certificate, err
	}
	_ = owner.Close()
	return certificate.PrincipalURI, certificate, nil
}

func readDesktopMeshStatus(ctx context.Context, principal string) (netmesh.Status, error) {
	certificate, ok := meshSelection("")
	if !ok || certificate.PrincipalURI != principal {
		return netmesh.Status{}, errors.New("selected organization changed")
	}
	snapshot, err := sessionbroker.ReadOperatorDirectoryAutomatic(ctx)
	if err != nil {
		return netmesh.Status{}, err
	}
	return desktopMeshStatus(snapshot), nil
}

func desktopMeshStatus(snapshot sessionbroker.OperatorDirectory) netmesh.Status {
	rows, _ := meshCollection(snapshot)
	result := netmesh.Status{}
	for _, row := range rows {
		if row.Platform == localmesh.PlatformLite {
			continue
		}
		id, err := uuid.Parse(row.Device)
		if err != nil {
			continue
		}
		address, err := netip.ParseAddrPort(row.Address)
		if err != nil {
			continue
		}
		result.Devices = append(result.Devices, netmesh.Device{Selector: row.Selector, Principal: row.Principal, ID: id, Name: row.Name, Address: address.Addr(), Port: address.Port()})
	}
	return result
}

func meshDeviceRow(d netmesh.Device) models.LANDevice {
	row := models.LANDevice{ID: d.ID.String(), DisplayName: meshDisplayName(d.Name), Hostname: "mesh:" + d.ID.String(), IPAddress: d.Address.String(), Port: int(d.Port), IsMTLS: true, IsWendyDevice: true, Mesh: true}
	if d.Selector != "" {
		row.Hostname = d.Selector
		row.PrincipalURI = d.Principal
	}
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
	}, readDesktopMeshStatus)
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
	if _, matched, err := sessionbroker.ParseMeshSelector(selector); matched && strings.HasPrefix(selector, "mesh://") {
		if err != nil {
			return nil, err
		}
		picked, _, err := connectNamedDeviceSelector(ctx, selector, true)
		return picked, err
	}
	name := strings.TrimPrefix(selector, "mesh:")
	picked, matched, err := connectMeshName(ctx, name, true)
	if err != nil {
		return nil, err
	}
	if !matched {
		return nil, fmt.Errorf("mesh device %q is not reachable", name)
	}
	return picked, nil
}

func connectDiscoveredMeshName(ctx context.Context, name string) (*SelectedDevice, bool, error) {
	return connectMeshName(ctx, name, true)
}
