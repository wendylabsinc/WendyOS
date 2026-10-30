//go:build darwin

package bleprovider

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
	"github.com/wendylabsinc/wendy/go/internal/shared/ble/central"
	"github.com/wendylabsinc/wendy/go/internal/shared/ble/permission"
	"github.com/wendylabsinc/wendy/go/internal/shared/ble/scan"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

// macOS joins as a CoreBluetooth central. Existing mesh nodes advertise the
// tenant service and L2CAP PSM; certificates authenticate the complete peer.
func Run(ctx context.Context, cfg Config) error {
	if err := cfg.defaults(); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	updates, err := scan.DiscoverBluetoothContinuous(ctx, scan.Options{Services: []string{ServiceUUID(cfg.Credentials.Device.Tenant())}, MaxDevices: 256, Preflight: permission.Preflight})
	if err != nil {
		return err
	}
	policy := localmesh.NewConnectionPolicy(cfg.Credentials.Device, true, localmesh.ConnectionLimits{Outgoing: 2, Attempts: 1, Total: 2})
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	var candidates localmesh.CandidateQueue
	var authenticatedRadios darwinPeerCache
	var diagnosticMu sync.Mutex
	lastError := ""
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	latest := map[string]scan.BLEDeviceInfo{}
	if cfg.Status != nil {
		cfg.Status(localmesh.CarrierStatus{Ready: true, Detail: "CoreBluetooth central scanning"})
	}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case devices, ok := <-updates:
			if !ok {
				return errors.New("Bluetooth scan ended")
			}
			latest = map[string]scan.BLEDeviceInfo{}
			for _, d := range devices {
				latest[d.Address] = d
			}
		case <-ticker.C:
			keys := []string{}
			matches := 0
			for address, d := range latest {
				if time.Since(d.LastSeen) <= 30*time.Second {
					keys = append(keys, address)
				}
			}
			for _, address := range candidates.Order(keys, time.Now()) {
				d := latest[address]
				adv, ok := matchingServiceData(d.ServiceData, ServiceUUID(cfg.Credentials.Device.Tenant()), cfg.NetworkName)
				if !ok {
					var err error
					adv, err = ParseLocalName(d.Name, cfg.NetworkName)
					if err != nil {
						continue
					}
				}
				matches++
				if authenticatedRadios.suppressed(address, adv, cfg.Selection, time.Now()) {
					continue
				}
				lease := policy.Reserve(address, meshid.Device{}, true)
				if lease == nil {
					continue
				}
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer lease.Release()
					err := dialDarwin(ctx, cfg, address, adv, lease, func(peer meshid.Device) {
						authenticatedRadios.remember(address, adv, peer, time.Now())
					})
					diagnosticMu.Lock()
					if err != nil {
						lastError = err.Error()
					} else {
						lastError = ""
					}
					diagnosticMu.Unlock()
					if err != nil {
						candidates.Failed(address, time.Now())
					} else {
						candidates.Succeeded(address)
					}
				}()
			}
			if cfg.Status != nil {
				counts := policy.Counts()
				diagnosticMu.Lock()
				detail := fmt.Sprintf("CoreBluetooth: %d mesh advertisements, %d links, %d connecting", matches, counts.Ready, counts.Attempts)
				if lastError != "" {
					detail += "; last link error: " + lastError
				}
				diagnosticMu.Unlock()
				cfg.Status(localmesh.CarrierStatus{Ready: true, Detail: detail})
			}
		}
	}
}
func dialDarwin(ctx context.Context, cfg Config, address string, adv Advertisement, lease *localmesh.ConnectionLease, authenticated func(meshid.Device)) error {
	connection, err := central.Connect(address, 5)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	if err = connection.OpenL2CAP(adv.PSM, 5); err != nil {
		connection.Close()
		return fmt.Errorf("open L2CAP: %w", err)
	}
	raw := central.NewL2CAPStream(connection)
	defer raw.Close()
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	var peer meshid.Device
	secure := tls.Client(raw, &tls.Config{
		Certificates: []tls.Certificate{cfg.Credentials.Certificate}, MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13, NextProtos: []string{ALPN},
		// Wendy's verifier checks issuer trust and tenant/principal instead of DNS.
		InsecureSkipVerify: true,
		CurvePreferences:   []tls.CurveID{tls.X25519},
		VerifyConnection: func(state tls.ConnectionState) error {
			chain := make([][]byte, len(state.PeerCertificates))
			for i, c := range state.PeerCertificates {
				chain[i] = c.Raw
			}
			identity, err := cfg.Credentials.Verify(chain, time.Now())
			if err != nil {
				return err
			}
			peer = identity.Device
			if peer == cfg.Credentials.Device || peer.Tenant() != cfg.Credentials.Device.Tenant() || deviceHint(peer.UUID()) != adv.DeviceHint || state.NegotiatedProtocol != ALPN {
				return errors.New("BLE mesh peer mismatch")
			}
			return nil
		},
	})
	_, err = handshakeMeshOutbound(ctx, secure)
	if err != nil {
		return fmt.Errorf("TLS handshake: %w", err)
	}
	defer secure.Close()
	if authenticated != nil {
		authenticated(peer)
	}
	if cfg.Selection != nil && !cfg.Selection.AllowRadio(peer, localmesh.RadioBLE) {
		return nil
	}
	if err := cfg.Node.AttachStream(localmesh.WithCarrier(localmesh.WithConnectionLease(ctx, lease), "ble"), peer, secure, LinkCost); err != nil {
		return fmt.Errorf("mesh stream: %w", err)
	}
	return nil
}
