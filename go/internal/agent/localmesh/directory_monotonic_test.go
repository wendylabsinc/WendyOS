package localmesh

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"math/big"
	"net/url"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type directoryElapsedClock struct{ now time.Time }

func (c *directoryElapsedClock) read() time.Time         { return c.now }
func (c *directoryElapsedClock) advance(d time.Duration) { c.now = c.now.Add(d) }

func monotonicDirectoryFixture(t *testing.T) (*Directory, [][]byte, ed25519.PrivateKey, meshid.Device, time.Time, *directoryElapsedClock) {
	t.Helper()
	d, chain, key, device, now := directoryFixture(t)
	clock := &directoryElapsedClock{now: now}
	d.monotonicNow = clock.read
	// Keep cache retention independent of the bounded wall-clock skew below.
	resetClockFixtureCache(t, d, chain, now.Add(-30*time.Minute))
	return d, chain, key, device, now, clock
}

func TestReceiverRelativeLeaseIgnoresWallSkewAndExpungesIdle(t *testing.T) {
	for _, wallOffset := range []time.Duration{-598 * time.Second, 598 * time.Second} {
		t.Run(wallOffset.String(), func(t *testing.T) {
			d, chain, key, device, now, clock := monotonicDirectoryFixture(t)
			m := routeManifest(device, now)
			m.Expires = now.Add(time.Second).UnixMilli()
			w, err := SignManifest(m, chain, key, now)
			if err != nil {
				t.Fatal(err)
			}
			wall := now.Add(wallOffset)
			if changed, err := d.Accept(w, wall); err != nil || !changed {
				t.Fatal(changed, err)
			}
			deadline := d.entries[device.Key()].activeUntil
			clock.advance(900 * time.Millisecond)
			wall = wall.Add(-2 * wallOffset)
			view := d.Snapshot(wall)
			if len(view) != 1 || view[0].LeaseRemaining(wall) != 100*time.Millisecond || view[0].Issued != m.Issued || view[0].Expires != m.Expires {
				t.Fatal("wall jump changed admitted lease or signed dates", view)
			}
			if changed, err := d.Accept(w, wall); err != nil || changed || d.entries[device.Key()].activeUntil != deadline {
				t.Fatal("duplicate changed receiver deadline", changed, err)
			}
			clock.advance(100 * time.Millisecond)
			if !d.Sweep(wall) || len(d.entries) != 0 || len(d.Records(wall)) != 0 || len(d.Snapshot(wall)) != 0 || len(d.RouteAuthorizations(wall)) != 0 {
				t.Fatal("idle expiry retained payload or derivative authorization")
			}
			if len(d.history) != 1 {
				t.Fatal("compact watermark was lost with full payload")
			}
			if _, err := d.Accept(w, wall); !errors.Is(err, ErrStaleManifest) {
				t.Fatal("same bytes renewed after expiry", err)
			}
			if _, ok := d.RecordDeadline(w, wall); ok {
				t.Fatal("expired payload exported deadline")
			}
		})
	}
}

func TestWatermarkTenDurationsDoesNotRefreshAndEventuallyForgets(t *testing.T) {
	d, chain, key, device, now, clock := monotonicDirectoryFixture(t)
	m := routeManifest(device, now)
	m.Expires = now.Add(MaxLease).UnixMilli()
	w, err := SignManifest(m, chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Accept(w, now); err != nil {
		t.Fatal(err)
	}
	until := d.history[device.Key()].Until
	clock.advance(149 * time.Minute)
	d.Sweep(now)
	if len(d.entries) != 0 || len(d.history) != 1 {
		t.Fatal("wrong active/watermark lifetime")
	}
	if _, err = d.Accept(w, now); !errors.Is(err, ErrStaleManifest) || d.history[device.Key()].Until != until {
		t.Fatal("late same bytes refreshed watermark", err)
	}
	clock.advance(time.Minute)
	if !d.Sweep(now) || len(d.history) != 0 {
		t.Fatal("150-minute watermark not physically pruned")
	}
	if changed, err := d.Accept(w, now); err != nil || !changed {
		t.Fatal("forgotten first-seen bytes could not learn local TTL", changed, err)
	}
}

func TestRelativeLeaseWithdrawalSupersessionAndResign(t *testing.T) {
	d, chain, key, device, now, clock := monotonicDirectoryFixture(t)
	m := routeManifest(device, now)
	m.Expires = now.Add(time.Second).UnixMilli()
	w, _ := SignManifest(m, chain, key, now)
	if _, err := d.Accept(w, now); err != nil {
		t.Fatal(err)
	}
	clock.advance(500 * time.Millisecond)
	resigned, _ := SignManifest(m, chain, key, now)
	deadline, watermark := d.entries[device.Key()].activeUntil, d.history[device.Key()]
	if changed, err := d.Accept(resigned, now); err != nil || changed || d.entries[device.Key()].activeUntil != deadline || d.history[device.Key()] != watermark {
		t.Fatal("resign extended lifetime", changed, err)
	}
	m.Revision++
	m.Withdraw = true
	withdrawal, _ := SignManifest(m, chain, key, now)
	if changed, err := d.Accept(withdrawal, now); err != nil || !changed {
		t.Fatal(changed, err)
	}
	if len(d.Snapshot(now)) != 0 || len(d.RouteAuthorizations(now)) != 0 {
		t.Fatal("withdrawal left active state")
	}
	clock.advance(time.Second)
	d.Sweep(now)
	if len(d.entries) != 0 {
		t.Fatal("withdrawal body retained beyond its local TTL")
	}
	if _, err := d.Accept(w, now); !errors.Is(err, ErrStaleManifest) {
		t.Fatal("old active bytes resurrected withdrawal", err)
	}
	m.Revision++
	m.Withdraw = false
	newer, _ := SignManifest(m, chain, key, now)
	if changed, err := d.Accept(newer, now); err != nil || !changed {
		t.Fatal("new verified revision did not supersede", changed, err)
	}
	if len(d.Snapshot(now)) != 1 {
		t.Fatal("newer presence missing")
	}
}

func TestLocalLeaseMetadataDoesNotChangeSignedV4Bytes(t *testing.T) {
	_, chain, key, device, now := directoryFixture(t)
	m := routeManifest(device, now)
	first, err := SignManifest(m, chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	m.AdmittedUntil = now.Add(7 * time.Second)
	second, err := SignManifest(m, chain, key, now)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Body, second.Body) || !bytes.Equal(first.Signature, second.Signature) {
		t.Fatal("local metadata changed signed v4 wire")
	}
	raw, err := netwire.Marshal(m)
	if err != nil || !bytes.Equal(raw, first.Body) {
		t.Fatal("local-only field leaked to CBOR", err)
	}
}

// Many distinct authenticated origin records share an ephemeral fixture CA.
// No hardware, real network or host clock is used by this stress fixture.
func stressDirectory(t *testing.T, payloadLimit int) (*Directory, func(uint32) SignedManifest, time.Time, *directoryElapsedClock) {
	t.Helper()
	now := time.Unix(1800000000, 0)
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "relative lease fixture"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	verify := func(chain [][]byte, _ time.Time) (Identity, error) {
		leaf, err := x509.ParseCertificate(chain[0])
		if err != nil {
			return Identity{}, err
		}
		if err = leaf.CheckSignatureFrom(ca); err != nil {
			return Identity{}, err
		}
		device, err := meshid.PeerFromCert(leaf)
		return Identity{Device: device, NotBefore: leaf.NotBefore, NotAfter: leaf.NotAfter}, err
	}
	cache, err := OpenIdentityCache("", CacheLimits{Entries: 2048, Bytes: 16 << 20, Age: 24 * time.Hour}, verify, now)
	if err != nil {
		t.Fatal(err)
	}
	clock := &directoryElapsedClock{now: now}
	d, err := NewDirectoryWithClock(testTenant(t), payloadLimit, cache, clock.read)
	if err != nil {
		t.Fatal(err)
	}
	makeWire := func(index uint32) SignedManifest {
		id := uuid.UUID{0x2b, 0x1c}
		binary.BigEndian.PutUint32(id[12:], index+1)
		device := meshid.NewDevice(testTenant(t), id)
		uri, _ := url.Parse(device.Principal())
		leafTemplate := &x509.Certificate{SerialNumber: big.NewInt(int64(index) + 2), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), URIs: []*url.URL{uri}, KeyUsage: x509.KeyUsageDigitalSignature}
		der, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, pub, key)
		if err != nil {
			t.Fatal(err)
		}
		chain := [][]byte{der, caDER}
		// Seed verified public fixture bundles directly: this test stresses the
		// directory rather than the cache's separate eviction/snapshot work.
		if _, err = verify(chain, now); err != nil {
			t.Fatal(err)
		}
		cache.entries[device.Principal()] = cachedIdentity{Chain: chain, Added: now, Used: now}
		m := routeManifest(device, now)
		m.Expires = now.Add(time.Second).UnixMilli()
		wire, err := SignManifest(m, chain, key, now)
		if err != nil {
			t.Fatal(err)
		}
		return wire
	}
	return d, makeWire, now, clock
}

func TestDynamicWatermarksExceedOldCountWithoutHoldingBodies(t *testing.T) {
	d, makeWire, now, clock := stressDirectory(t, 128)
	var index uint32
	for cycle := 0; cycle < 12; cycle++ {
		for n := 0; n < 128; n++ {
			w := makeWire(index)
			index++
			if _, err := d.Accept(w, now); err != nil {
				t.Fatal("dynamic watermark admission failed", cycle, n, err)
			}
		}
		clock.advance(time.Second)
		d.Sweep(now)
		if len(d.entries) != 0 {
			t.Fatal("full payload survived idle sweep")
		}
		if len(d.history) > 1280 {
			t.Fatal("watermark duration did not bound retention")
		}
	}
	if len(d.history) <= 1024 {
		t.Fatal("dynamic watermark test did not exceed old cap", len(d.history))
	}
	clock.advance(10 * time.Second)
	d.Sweep(now)
	if len(d.entries) != 0 || len(d.history) != 0 {
		t.Fatal("idle final sweep retained payloads or watermarks")
	}
}

func TestIdleShortLeaseCyclesReleaseAllPayloads(t *testing.T) {
	d, makeWire, now, clock := stressDirectory(t, 2)
	for cycle := uint32(0); cycle < 128; cycle++ {
		if cycle%16 == 0 {
			runtime.GC()
		}
		w := makeWire(cycle)
		if _, err := d.Accept(w, now); err != nil {
			t.Fatal(err)
		}
		clock.advance(time.Second)
		if !d.Sweep(now) || len(d.entries) != 0 || len(d.Records(now)) != 0 || len(d.Snapshot(now)) != 0 {
			t.Fatal("idle cycle retained full record", cycle)
		}
	}
	clock.advance(10 * time.Second)
	d.Sweep(now)
	if len(d.history) != 0 {
		t.Fatal("compact cycle metadata not freed")
	}
}

func TestRelativeLeaseRequiresBoundedDurationAndSigningCertificateWindow(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		issuedOffset, duration time.Duration
		valid                  bool
	}{
		{"inside leaf", -time.Hour, time.Minute, true},
		{"before later issuer but inside leaf", -40 * time.Minute, time.Minute, true},
		{"before signing leaf", -time.Hour - time.Millisecond, time.Minute, false},
		{"past selected expiry", time.Hour - time.Second, time.Minute, false},
		{"zero duration", 0, 0, false},
		{"negative duration", 0, -time.Second, false},
		{"over maximum duration", 0, MaxLease + time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _, key, device, now, _ := monotonicDirectoryFixture(t)
			original := d.cache.verify
			d.cache.verify = func(chain [][]byte, wall time.Time) (Identity, error) {
				id, err := original(chain, wall)
				id.NotBefore = now.Add(-30 * time.Minute)
				return id, err
			}
			m := routeManifest(device, now)
			m.Issued = now.Add(tc.issuedOffset).UnixMilli()
			m.Expires = now.Add(tc.issuedOffset + tc.duration).UnixMilli()
			w := rawRouteManifest(t, m, device, key)
			changed, err := d.Accept(w, now)
			if (err == nil) != tc.valid || changed != tc.valid {
				t.Fatal(changed, err)
			}
			if !tc.valid && (len(d.entries) != 0 || len(d.history) != 0) {
				t.Fatal("invalid signed window mutated lease state")
			}
		})
	}
}
