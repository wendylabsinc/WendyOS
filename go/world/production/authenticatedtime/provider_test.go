package authenticatedtime

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"math"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/production/internal/upstreamtime/roughtime"
	"github.com/wendylabsinc/wendy/go/world/production/trustedtime"
)

var testTime = time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)

type fakeClock struct {
	mu  sync.Mutex
	r   trustedtime.Reading
	err error
}

func (c *fakeClock) read() (trustedtime.Reading, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.r, c.err
}
func (c *fakeClock) set(r trustedtime.Reading) { c.mu.Lock(); defer c.mu.Unlock(); c.r = r }
func testClock() *fakeClock {
	return &fakeClock{r: trustedtime.Reading{BootID: "test-only-boot", Elapsed: 10 * time.Second}}
}
func config(servers ...Server) Config {
	return Config{Servers: servers, QueryTimeout: 200 * time.Millisecond, MaxHoldover: time.Minute, DriftPPM: 1000, MaxWidth: 30 * time.Second}
}
func u64(n uint64) []byte { b := make([]byte, 8); binary.LittleEndian.PutUint64(b, n); return b }
func u32(n uint32) []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, n); return b }

// Every test server owns an ephemeral, untrusted key and signs the actual random
// client nonce. No fixture trusts local wall time or installs credentials.
func server(t *testing.T, name, mode string, at time.Time) Server {
	t.Helper()
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, online, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	t.Cleanup(func() { _ = pc.Close(); <-done })
	go func() {
		defer close(done)
		var cached []byte
		buf := make([]byte, 8192)
		for {
			n, remote, e := pc.ReadFrom(buf)
			if e != nil {
				return
			}
			if mode == "silent" {
				continue
			}
			if n < 12 || string(buf[:8]) != "ROUGHTIM" {
				continue
			}
			req, e := roughtime.DecodeMessage(buf[12:n])
			if e != nil {
				continue
			}
			nonce := req[roughtime.TagNONC]
			if mode == "wrong-nonce" {
				nonce = make([]byte, 32)
			}
			leaf := sha512.Sum512(append([]byte{0}, nonce...))
			mid := uint64(at.UnixMicro())
			radius := uint32(100000)
			if mode == "wide" {
				radius = 11000000
			}
			if mode == "overflow" {
				mid = math.MaxUint64
			}
			srep := roughtime.EncodeMessage(map[uint32][]byte{roughtime.TagMIDP: u64(mid), roughtime.TagRADI: u32(radius), roughtime.TagROOT: leaf[:32]})
			sig := ed25519.Sign(online, append([]byte(roughtime.SigContext), srep...))
			if mode == "bad-signature" {
				sig[0] ^= 1
			}
			mint, maxt := uint64(at.Add(-time.Hour).UnixMicro()), uint64(at.Add(time.Hour).UnixMicro())
			if mode == "overflow" {
				mint = 0
				maxt = math.MaxUint64
			}
			if mode == "zero-delegation" {
				mint = 0
				maxt = 0
			}
			if mode == "expired-delegation" {
				maxt = uint64(at.Add(-time.Second).UnixMicro())
				mint = maxt - 1000000
			}
			dele := roughtime.EncodeMessage(map[uint32][]byte{roughtime.TagPUBK: online.Public().(ed25519.PublicKey), roughtime.TagMINT: u64(mint), roughtime.TagMAXT: u64(maxt)})
			cert := roughtime.EncodeMessage(map[uint32][]byte{roughtime.TagDELE: dele, roughtime.TagSIG: ed25519.Sign(key, append([]byte(roughtime.CertContext), dele...))})
			resp := roughtime.EncodeMessage(map[uint32][]byte{roughtime.TagSREP: srep, roughtime.TagSIG: sig, roughtime.TagCERT: cert, roughtime.TagINDX: u32(0)})
			if mode == "oversized" {
				resp = append(resp, make([]byte, 5000)...)
			}
			if mode == "replay" {
				if cached == nil {
					cached = append([]byte(nil), resp...)
				}
				resp = cached
			}
			_, _ = pc.WriteTo(resp, remote)
		}
	}()
	return Server{Name: name, Address: pc.LocalAddr().String(), PublicKey: pub}
}

func TestSignedQuorumHoldoverAndEvidenceOwnership(t *testing.T) {
	a, b := server(t, "a", "", testTime), server(t, "b", "", testTime)
	clock := testClock()
	p, err := acquire(context.Background(), config(a, b), clock.read)
	if err != nil {
		t.Fatal(err)
	}
	report := p.Report()
	if report.Source != Source || report.Quorum != 2 || report.Confidence != "degraded" || len(report.EvidenceID) != 64 {
		t.Fatal("bad authenticated report", report)
	}
	anchor := report.Anchor
	if !anchor.Earliest.Equal(testTime.Add(-100*time.Millisecond)) || !anchor.Latest.Equal(testTime.Add(100*time.Millisecond)) {
		t.Fatal("wrong nonce-bound interval", anchor)
	}
	for _, e := range report.Evidence {
		if !e.Included || len(e.Nonce) != 32 || len(e.RawResponse) == 0 || len(e.KeyID) != 64 {
			t.Fatal("missing verifiable evidence")
		}
	}
	report.Evidence[0].Nonce[0] ^= 1
	report.Evidence[0].RawResponse[0] ^= 1
	if p.Report().Evidence[0].Nonce[0] == report.Evidence[0].Nonce[0] {
		t.Fatal("report aliases retained evidence")
	}
	clock.set(trustedtime.Reading{BootID: "test-only-boot", Elapsed: 40 * time.Second})
	w, err := p.Now()
	if err != nil {
		t.Fatal(err)
	}
	if !w.Earliest.Equal(anchor.Earliest.Add(30*time.Second-30*time.Millisecond)) || !w.Latest.Equal(anchor.Latest.Add(30*time.Second+30*time.Millisecond)) {
		t.Fatal("suspend-inclusive drift was not applied", w)
	}
	clock.set(trustedtime.Reading{BootID: "test-only-boot", Elapsed: 71 * time.Second})
	if _, err = p.Now(); !errors.Is(err, trustedtime.ErrUntrusted) {
		t.Fatal("expired holdover accepted", err)
	}
	clock.set(trustedtime.Reading{BootID: "test-only-boot", Elapsed: 40 * time.Second})
	if _, err = p.Now(); err == nil {
		t.Fatal("failed holdover recovered without acquisition")
	}
}
func TestRejectedSignedEvidenceCannotMakeQuorum(t *testing.T) {
	for _, mode := range []string{"wrong-nonce", "bad-signature", "expired-delegation", "zero-delegation", "wide", "overflow", "oversized", "silent"} {
		t.Run(mode, func(t *testing.T) {
			a := server(t, "valid", "", testTime)
			b := server(t, "invalid", mode, testTime)
			if p, err := acquire(context.Background(), config(a, b), testClock().read); err == nil || p != nil {
				t.Fatal("invalid evidence became quorum")
			}
		})
	}
}
func TestOutlierAndUnavailableThirdSource(t *testing.T) {
	a, b := server(t, "a", "", testTime), server(t, "b", "", testTime)
	for _, mode := range []string{"outlier", "silent"} {
		t.Run(mode, func(t *testing.T) {
			c := server(t, "c", mode, testTime.Add(time.Hour))
			p, err := acquire(context.Background(), config(a, b, c), testClock().read)
			if err != nil {
				t.Fatal(err)
			}
			if p.Report().Quorum != 2 || p.Report().Evidence[2].Included {
				t.Fatal("outlier or unavailable source included")
			}
		})
	}
}
func TestNonceReplayAndCallerCancellation(t *testing.T) {
	a, b := server(t, "a", "replay", testTime), server(t, "b", "replay", testTime)
	if _, err := acquire(context.Background(), config(a, b), testClock().read); err != nil {
		t.Fatal(err)
	}
	if _, err := acquire(context.Background(), config(a, b), testClock().read); err == nil {
		t.Fatal("previous responses accepted for fresh nonces")
	}
	silent := server(t, "silent", "silent", testTime)
	ctx, cancel := context.WithCancel(context.Background())
	timer := time.AfterFunc(20*time.Millisecond, cancel)
	defer timer.Stop()
	start := time.Now()
	if _, err := acquire(ctx, config(a, silent), testClock().read); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("cancelled UDP read did not return promptly")
	}
}
func TestBootFaultsAndBoundsFailClosed(t *testing.T) {
	a, b := server(t, "a", "", testTime), server(t, "b", "", testTime)
	for _, mode := range []string{"reboot", "rollback", "unavailable", "width"} {
		t.Run(mode, func(t *testing.T) {
			clock := testClock()
			cfg := config(a, b)
			if mode == "width" {
				cfg.MaxWidth = time.Millisecond
			}
			p, err := acquire(context.Background(), cfg, clock.read)
			if mode == "width" {
				var bounds *IntervalBoundsError
				if !errors.As(err, &bounds) || bounds.Reason != "width" || bounds.Width <= bounds.MaxWidth || bounds.Quorum != 2 || p != nil {
					t.Fatal("refused width lacks safe diagnostic evidence", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "reboot":
				clock.set(trustedtime.Reading{BootID: "other-boot", Elapsed: time.Second})
			case "rollback":
				clock.set(trustedtime.Reading{BootID: "test-only-boot", Elapsed: time.Second})
			case "unavailable":
				clock.mu.Lock()
				clock.err = errors.New("no boot clock")
				clock.mu.Unlock()
			}
			if _, err = p.Now(); err == nil {
				t.Fatal("clock fault accepted")
			}
			clock.mu.Lock()
			clock.err = nil
			clock.mu.Unlock()
			clock.set(trustedtime.Reading{BootID: "test-only-boot", Elapsed: 10 * time.Second})
			if _, err = p.Now(); err == nil {
				t.Fatal("clock fault silently reset")
			}
		})
	}
}
func TestConfigurationRequiresIndependentSourcesAndBounds(t *testing.T) {
	a, b := server(t, "a", "", testTime), server(t, "b", "", testTime)
	for _, mode := range []string{"no-sources", "one-source", "same-key", "same-address", "same-name", "bad-key", "zero-drift", "zero-age", "zero-width", "long-query"} {
		t.Run(mode, func(t *testing.T) {
			cfg := config(a, b)
			cfg.Servers = cloneServers(cfg.Servers)
			switch mode {
			case "no-sources":
				cfg.Servers = nil
			case "one-source":
				cfg.Servers = cfg.Servers[:1]
			case "same-key":
				cfg.Servers[1].PublicKey = cfg.Servers[0].PublicKey
			case "same-address":
				cfg.Servers[1].Address = cfg.Servers[0].Address
			case "same-name":
				cfg.Servers[1].Name = cfg.Servers[0].Name
			case "bad-key":
				cfg.Servers[0].PublicKey = nil
			case "zero-drift":
				cfg.DriftPPM = 0
			case "zero-age":
				cfg.MaxHoldover = 0
			case "zero-width":
				cfg.MaxWidth = 0
			case "long-query":
				cfg.QueryTimeout = 6 * time.Second
			}
			if _, err := acquire(context.Background(), cfg, testClock().read); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
	saved := WendyServers()
	saved[0].PublicKey[0] ^= 1
	if saved[0].PublicKey[0] == WendyServers()[0].PublicKey[0] {
		t.Fatal("Wendy pins are caller-mutable")
	}
}
func TestNativeClockAndActualUDPAdapter(t *testing.T) {
	before, err := readNative()
	if err != nil {
		t.Fatal(err)
	}
	after, err := readNative()
	if err != nil || after.BootID == "" || before.BootID != after.BootID || after.Elapsed < before.Elapsed {
		t.Fatal("native clock unavailable or inconsistent", err)
	}
	a, b := server(t, "a", "", testTime), server(t, "b", "", testTime)
	p, err := Acquire(context.Background(), config(a, b))
	if err != nil {
		t.Fatal(err)
	}
	w, err := p.Now()
	if err != nil || w.BootID != before.BootID || w.Source != Source || !w.Authenticated {
		t.Fatal("native acquired provider invalid", err)
	}
}

func TestThreeDistinctSignedSourcesAreVerified(t *testing.T) {
	a, b, c := server(t, "a", "", testTime), server(t, "b", "", testTime), server(t, "c", "", testTime)
	p, err := acquire(context.Background(), config(a, b, c), testClock().read)
	if err != nil {
		t.Fatal(err)
	}
	if p.Report().Quorum != 3 || p.Report().Confidence != "verified" {
		t.Fatal("three-source consensus not retained")
	}
}

func TestCollectionBootFaultOrSuspendBudgetCannotCreateProvider(t *testing.T) {
	a, b := server(t, "a", "", testTime), server(t, "b", "", testTime)
	for _, mode := range []string{"reboot", "rollback", "suspend-budget"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			calls := 0
			read := func() (trustedtime.Reading, error) {
				mu.Lock()
				defer mu.Unlock()
				calls++
				r := trustedtime.Reading{BootID: "test-only-boot", Elapsed: 10 * time.Second}
				if calls > 1 {
					switch mode {
					case "reboot":
						r.BootID = "new-boot"
					case "rollback":
						r.Elapsed = time.Second
					case "suspend-budget":
						r.Elapsed += 10 * time.Second
					}
				}
				return r, nil
			}
			if p, err := acquire(context.Background(), config(a, b), read); err == nil || p != nil {
				t.Fatal("faulted or suspended collection created provider")
			}
		})
	}
}
