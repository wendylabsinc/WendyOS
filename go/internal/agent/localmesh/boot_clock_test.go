package localmesh

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/timesync"
)

func TestBootClockLocalAnchorsProgressWithoutChangingWallOrConfidence(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config")
	fallback := filepath.Join(root, "fallback")
	for _, dir := range []string{config, fallback} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	date := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if err := timesync.WriteFloor(config, date.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := timesync.WriteFloor(fallback, date.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	saved := filepath.Join(root, "clock")
	if err := os.WriteFile(saved, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(saved, date, date); err != nil {
		t.Fatal(err)
	}
	wall := date.Add(-598 * time.Second)
	boot := int64(10 * time.Hour)
	c, err := newBootClock(config, fallback, saved, func() time.Time { return wall }, func() (int64, error) { return boot, nil })
	if err != nil {
		t.Fatal(err)
	}
	p := c.Policy()
	if got := p.BaseTime(wall); !got.Equal(date) {
		t.Fatalf("saved anchor added boot uptime: %v", got)
	}
	if !p.Uncertain() {
		t.Fatal("historical anchor claimed fresh UTC")
	}
	if got := c.Snapshot(); got.Source != "systemd-saved-clock" || !got.ConfigFloor.Equal(date.Add(-time.Hour)) {
		t.Fatalf("wrong local provenance %+v", got)
	}
	boot += int64(30 * time.Second)
	if got := p.BaseTime(wall); !got.Equal(date.Add(30 * time.Second)) {
		t.Fatalf("anchor did not progress: %v", got)
	}
	if !wall.Equal(date.Add(-598 * time.Second)) {
		t.Fatal("policy changed actual UTC")
	}
	// Actual time ahead of a floor, or ns sampling jitter, does not revoke grace.
	wall = date.Add(10*time.Hour + time.Nanosecond)
	if !p.Uncertain() || !p.BaseTime(wall).Equal(wall) {
		t.Fatal("wall ordering silently changed uncertainty")
	}
	// A backward UTC adjustment does not rewind the local boot anchor.
	wall = date.Add(-time.Hour)
	if !p.BaseTime(wall).Equal(date.Add(30 * time.Second)) {
		t.Fatal("UTC rollback stretched or rewound anchor")
	}
	for _, bad := range []timesync.DirectSyncObservation{{Source: "direct", Quorum: 2}, {Source: "multicast", Quorum: 2, UTCWithinVerifiedInterval: true}, {Source: "direct", Quorum: 1, UTCWithinVerifiedInterval: true}, {Source: "direct", Quorum: 2, UTCWithinVerifiedInterval: true, ApplyError: errors.New("failed")}} {
		c.ObserveDirectSync(bad)
		if !p.Uncertain() {
			t.Fatal("unproven direct correction cleared uncertainty")
		}
	}
	c.ObserveDirectSync(timesync.DirectSyncObservation{Source: "direct", Quorum: 2, UTCWithinVerifiedInterval: true})
	if p.Uncertain() {
		t.Fatal("direct verified UTC failed to revoke grace")
	}
	boot += int64(600 * time.Second)
	if p.Uncertain() {
		t.Fatal("ordinary TSA age resurrected boot uncertainty")
	}
}

func TestBootClockProvisionedCertificateAndFallbackBoundaries(t *testing.T) {
	root := t.TempDir()
	fallback := filepath.Join(root, "fallback")
	if err := os.Mkdir(fallback, 0700); err != nil {
		t.Fatal(err)
	}
	date := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	if err := timesync.WriteFloor(fallback, date); err != nil {
		t.Fatal(err)
	}
	wall := date.Add(-time.Hour)
	boot := int64(100 * time.Second)
	c, err := newBootClock(root, fallback, filepath.Join(root, "absent"), func() time.Time { return wall }, func() (int64, error) { return boot, nil })
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Snapshot(); got.Source != "config-partition-floor" || !got.ConfigFloor.IsZero() {
		t.Fatalf("fallback misreported startup ApplyFloor source: %+v", got)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: date.Add(time.Hour), NotAfter: date.Add(24 * time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	boot += int64(5 * time.Second)
	if err := c.AddProvisionedCertificate(string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))); err != nil {
		t.Fatal(err)
	}
	if got := c.Snapshot(); got.Source != "provisioned-certificate" || !got.Base.Equal(cert.NotBefore) {
		t.Fatalf("own provisioned anchor wrong: %+v", got)
	}
	boot += int64(time.Second)
	if !c.Policy().BaseTime(wall).Equal(cert.NotBefore.Add(time.Second)) {
		t.Fatal("certificate anchor did not progress from capture")
	}
	if err := c.AddProvisionedCertificate("not a certificate"); err == nil {
		t.Fatal("malformed local identity accepted")
	}
	for _, bad := range []time.Time{time.Unix(0, 0), time.Date(2200, 1, 1, 0, 0, 0, 0, time.UTC)} {
		if err := timesync.WriteFloor(root, bad); err != nil {
			t.Fatal(err)
		}
		if !readBootClockFloor(filepath.Join(root, "clock_floor")).IsZero() {
			t.Fatal("implausible encoded floor accepted")
		}
	}
}
