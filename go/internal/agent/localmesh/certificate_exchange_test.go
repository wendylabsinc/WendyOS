package localmesh

import (
	"bytes"
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/agent/netwire"
	"reflect"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

type certificateFixture struct {
	now    time.Time
	chains map[string][][]byte
	ids    map[string]meshid.Device
}

func newCertificateFixture(t *testing.T) *certificateFixture {
	t.Helper()
	f := &certificateFixture{now: time.Now().Truncate(time.Second), chains: make(map[string][][]byte), ids: make(map[string]meshid.Device)}
	common := [][]byte{bytes.Repeat([]byte{11}, 5495), bytes.Repeat([]byte{12}, 6827), bytes.Repeat([]byte{13}, 7465)}
	for i, device := range []meshid.Device{testDevice(t, testPrincipal(testTenantUUID, testDeviceAID)), testDevice(t, testPrincipal(testTenantUUID, testDeviceBID)), testDevice(t, testPrincipal(testTenantUUID, testDeviceCID))} {
		leaf := bytes.Repeat([]byte{byte(i + 1)}, 3953)
		f.chains[device.Principal()] = append([][]byte{leaf}, cloneChain(common)...)
		f.ids[string(leaf)] = device
	}
	return f
}

func (f *certificateFixture) verify(chain [][]byte, now time.Time) (Identity, error) {
	if len(chain) == 0 {
		return Identity{}, errors.New("missing chain")
	}
	id, ok := f.ids[string(chain[0])]
	if !ok || !reflect.DeepEqual(f.chains[id.Principal()], chain) {
		return Identity{}, errors.New("untrusted or incomplete chain")
	}
	return Identity{Device: id, NotAfter: f.now.Add(time.Hour)}, nil
}

func (f *certificateFixture) cache(t *testing.T, own string, pool *CertificatePool) *IdentityCache {
	t.Helper()
	if pool == nil {
		pool = NewCertificatePool()
	}
	c, err := OpenIdentityCacheWithCertificates("", DefaultCacheLimits(), f.verify, f.now, pool)
	if err != nil {
		t.Fatal(err)
	}
	if own != "" {
		if _, err := c.Put(f.chains[own], f.now); err != nil {
			t.Fatal(err)
		}
	}
	return c
}

// driveCertificateExchange exercises the actual request/response protocol,
// including its wire validation and per-chunk limits, without bypassing caches.
func driveCertificateExchange(t *testing.T, source, receiver *CertificateExchange, first *CertificateMessage, now time.Time) (requests []CertificateRequest, bytesReceived int, completed []string) {
	t.Helper()
	replies, done, err := receiver.Receive(*first, now)
	if err != nil {
		t.Fatal(err)
	}
	completed = append(completed, done...)
	for steps := 0; len(replies) > 0; steps++ {
		if steps > 1024 {
			t.Fatal("certificate exchange did not terminate")
		}
		var responses []*CertificateMessage
		for _, reply := range replies {
			if reply.Request != nil {
				requests = append(requests, *reply.Request)
			}
			out, _, err := source.Receive(*reply, now)
			if err != nil {
				t.Fatal(err)
			}
			responses = append(responses, out...)
		}
		replies = nil
		for _, response := range responses {
			wire, err := netwire.Marshal(response)
			if err != nil || len(wire) > 1800 {
				t.Fatal("oversized certificate frame", len(wire), err)
			}
			var decoded CertificateMessage
			if err := netwire.Unmarshal(wire, &decoded); err != nil {
				t.Fatal(err)
			}
			if decoded.Chunk != nil {
				bytesReceived += len(decoded.Chunk.Data)
			}
			out, done, err := receiver.Receive(decoded, now)
			if err != nil {
				t.Fatal(err)
			}
			replies = append(replies, out...)
			completed = append(completed, done...)
		}
	}
	return
}

func TestCertificateExchangeRequestsOnlyMissingLeaf(t *testing.T) {
	f := newCertificateFixture(t)
	a, b := testPrincipal(testTenantUUID, testDeviceAID), testPrincipal(testTenantUUID, testDeviceBID)
	source := NewCertificateExchange(f.cache(t, a, nil), testTenant(t))
	c := f.cache(t, b, nil)
	receiver := NewCertificateExchange(c, testTenant(t))
	descriptor := source.Describe(a, f.now)
	requests, received, done := driveCertificateExchange(t, source, receiver, descriptor, f.now)
	if received != 3953 || len(requests) != 4 || !reflect.DeepEqual(done, []string{a}) {
		t.Fatalf("requests=%v received=%d done=%v", requests, received, done)
	}
	for i, q := range requests {
		if q.Hash != certificateHash(f.chains[a][0]) || q.Offset != i*certificateChunkBytes {
			t.Fatal("requested an already-held issuer or skipped a chunk", q)
		}
	}
	if _, _, ok := c.Get(a, f.now); !ok {
		t.Fatal("verified origin missing")
	}
	requests, received, done = driveCertificateExchange(t, source, receiver, descriptor, f.now)
	if len(requests) != 0 || received != 0 || len(done) != 1 {
		t.Fatal("repeated certificate bytes for a complete cached chain", requests, received, done)
	}
}

func TestCertificateExchangeColdChainFetchedSequentially(t *testing.T) {
	f := newCertificateFixture(t)
	a := testPrincipal(testTenantUUID, testDeviceAID)
	source := NewCertificateExchange(f.cache(t, a, nil), testTenant(t))
	receiver := NewCertificateExchange(f.cache(t, "", nil), testTenant(t))
	requests, received, done := driveCertificateExchange(t, source, receiver, source.Describe(a, f.now), f.now)
	if received != chainSize(f.chains[a]) || len(done) != 1 {
		t.Fatal(received, done)
	}
	i := 0
	for _, cert := range f.chains[a] {
		for offset := 0; offset < len(cert); offset += certificateChunkBytes {
			if i >= len(requests) || requests[i] != (CertificateRequest{Hash: certificateHash(cert), Offset: offset}) {
				t.Fatal("did not request each missing element sequentially", requests)
			}
			i++
		}
	}
	if i != len(requests) {
		t.Fatal("extra requests", requests[i:])
	}
}

func TestCertificateExchangeCoalescesNeighboursAndCaches(t *testing.T) {
	f := newCertificateFixture(t)
	a, b := testPrincipal(testTenantUUID, testDeviceAID), testPrincipal(testTenantUUID, testDeviceBID)
	source := NewCertificateExchange(f.cache(t, a, nil), testTenant(t))
	pool := NewCertificatePool()
	first := NewCertificateExchange(f.cache(t, b, pool), testTenant(t))
	otherCache := f.cache(t, b, pool)
	other := NewCertificateExchange(otherCache, testTenant(t))
	descriptor := source.Describe(a, f.now)
	q, _, err := first.Receive(*descriptor, f.now)
	if err != nil || len(q) != 1 {
		t.Fatal(q, err)
	}
	duplicate, done, err := other.Receive(*descriptor, f.now)
	if err != nil || len(duplicate) != 0 || len(done) != 0 {
		t.Fatal("second neighbour requested an element already being fetched", duplicate, done, err)
	}
	// Replay the description to drive the first exchange with its original
	// outstanding request; the second supplier must stay idle throughout.
	for len(q) > 0 {
		out, _, err := source.Receive(*q[0], f.now)
		if err != nil || len(out) != 1 {
			t.Fatal(out, err)
		}
		q, _, err = first.Receive(*out[0], f.now)
		if err != nil {
			t.Fatal(err)
		}
		duplicate, done, err = other.Poll(f.now)
		if err != nil || len(duplicate) != 0 {
			t.Fatal("duplicate fetch through other cache", duplicate, err)
		}
	}
	if _, _, ok := otherCache.Get(a, f.now); !ok || len(done) != 1 {
		t.Fatal("second cache did not independently admit the assembled chain", done)
	}
}

func TestCertificateExchangeDisconnectReleasesFirstSupplier(t *testing.T) {
	f := newCertificateFixture(t)
	a, b := testPrincipal(testTenantUUID, testDeviceAID), testPrincipal(testTenantUUID, testDeviceBID)
	source := NewCertificateExchange(f.cache(t, a, nil), testTenant(t))
	c := f.cache(t, b, nil)
	first, other := NewCertificateExchange(c, testTenant(t)), NewCertificateExchange(c, testTenant(t))
	descriptor := source.Describe(a, f.now)
	q, _, _ := first.Receive(*descriptor, f.now)
	out, _, _ := source.Receive(*q[0], f.now)
	if _, _, err := first.Receive(*out[0], f.now); err != nil {
		t.Fatal(err)
	}
	if replies, _, err := other.Receive(*descriptor, f.now); err != nil || len(replies) != 0 {
		t.Fatal(replies, err)
	}
	first.Close()
	q, _, err := other.Poll(f.now)
	if err != nil || len(q) != 1 || q[0].Request.Offset != 0 {
		t.Fatal("new supplier inherited unverified partial bytes or remained blocked", q, err)
	}
	if _, _, err := first.Receive(*out[0], f.now); err == nil {
		t.Fatal("old incarnation accepted a late response")
	}
}

func TestCertificateExchangeSilentSupplierYieldsToAnotherNeighbour(t *testing.T) {
	f := newCertificateFixture(t)
	a, b := testPrincipal(testTenantUUID, testDeviceAID), testPrincipal(testTenantUUID, testDeviceBID)
	source := NewCertificateExchange(f.cache(t, a, nil), testTenant(t))
	c := f.cache(t, b, nil)
	first, other := NewCertificateExchange(c, testTenant(t)), NewCertificateExchange(c, testTenant(t))
	descriptor := source.Describe(a, f.now)
	if q, _, err := first.Receive(*descriptor, f.now); err != nil || len(q) != 1 {
		t.Fatal(q, err)
	}
	if q, _, err := other.Receive(*descriptor, f.now); err != nil || len(q) != 0 {
		t.Fatal(q, err)
	}
	// Poll the stalled supplier first: map/maintenance iteration order must
	// not let it retain ownership forever after missing its progress bound.
	now := f.now.Add(certificateFetchTTL)
	if q, _, err := first.Poll(now); err != nil || len(q) != 0 {
		t.Fatal("silent first supplier immediately reclaimed the fetch", q, err)
	}
	if q, _, err := other.Poll(now); err != nil || len(q) != 1 || q[0].Request.Offset != 0 {
		t.Fatal("another supplier could not recover the fetch", q, err)
	}
	first.Close()
	other.Close()
	if len(c.parts.pending) != 0 {
		t.Fatal("closed suppliers retained fetch state")
	}
}

func TestCertificateExchangeRejectsWrongBytesAndPrincipal(t *testing.T) {
	for _, mode := range []string{"wrong-bytes", "wrong-principal", "untrusted-chain"} {
		t.Run(mode, func(t *testing.T) {
			f := newCertificateFixture(t)
			a, b := testPrincipal(testTenantUUID, testDeviceAID), testPrincipal(testTenantUUID, testDeviceBID)
			source := NewCertificateExchange(f.cache(t, a, nil), testTenant(t))
			c := f.cache(t, b, nil)
			receiver := NewCertificateExchange(c, testTenant(t))
			descriptor := source.Describe(a, f.now)
			if mode == "wrong-principal" {
				descriptor.Chain.Principal = testPrincipal(testTenantUUID, testDeviceCID)
			}
			if mode == "untrusted-chain" {
				descriptor.Chain.Hashes[1], descriptor.Chain.Hashes[2] = descriptor.Chain.Hashes[2], descriptor.Chain.Hashes[1]
			}
			q, _, err := receiver.Receive(*descriptor, f.now)
			for err == nil && len(q) > 0 {
				out, _, sourceErr := source.Receive(*q[0], f.now)
				if sourceErr != nil || len(out) != 1 {
					t.Fatal(out, sourceErr)
				}
				if mode == "wrong-bytes" {
					out[0].Chunk.Data[0] ^= 1
				}
				q, _, err = receiver.Receive(*out[0], f.now)
			}
			if err == nil {
				t.Fatal("invalid identity evidence admitted")
			}
			if _, _, ok := c.Get(a, f.now); ok {
				t.Fatal("invalid exchange mutated the authorized identity cache")
			}
			receiver.Close()
		})
	}
}

func TestCertificateExchangeUnsolicitedAndBounds(t *testing.T) {
	f := newCertificateFixture(t)
	c := f.cache(t, "", nil)
	e := NewCertificateExchange(c, testTenant(t))
	data := bytes.Repeat([]byte{1}, 1024)
	m := CertificateMessage{Chunk: &CertificateChunk{Hash: certificateHash(data), Total: len(data), Data: data}}
	if q, done, err := e.Receive(m, f.now); err != nil || len(q) != 0 || len(done) != 0 || c.parts.bytes != 0 {
		t.Fatal("unsolicited bytes retained", q, done, err, c.parts.bytes)
	}
	for _, bad := range []CertificateMessage{
		{},
		{Request: &CertificateRequest{Hash: "invalid"}},
		{Request: &CertificateRequest{Hash: certificateHash(data), Offset: maxCertificateBytes}},
		{Chunk: &CertificateChunk{Hash: certificateHash(data), Total: maxCertificateBytes + 1, Data: data}},
		{Chunk: &CertificateChunk{Hash: certificateHash(data), Total: 1024, Offset: -1, Data: data}},
		{Chunk: &CertificateChunk{Hash: certificateHash(data), Total: 2048, Data: data[:1]}},
	} {
		if _, _, err := e.Receive(bad, f.now); err == nil {
			t.Fatal("invalid allocation bound accepted", bad)
		}
	}
	if c.parts.bytes != 0 {
		t.Fatal("malformed messages allocated certificate state")
	}
}

func TestCertificatePoolMemoryExpiryAndLateResponse(t *testing.T) {
	pool := NewCertificatePool()
	now := time.Now()
	owners := make([]uint64, maxCertificatePending)
	for i := range owners {
		owners[i] = pool.newOwner()
		data := bytes.Repeat([]byte{byte(i)}, maxCertificateBytes)
		hash := certificateHash(data)
		if !pool.claim(hash, owners[i], now) {
			t.Fatal("could not claim within pending bound", i)
		}
		_, accepted, err := pool.accept(owners[i], CertificateChunk{Hash: hash, Total: len(data), Data: data[:certificateChunkBytes]}, now)
		if err != nil || !accepted || pool.bytes > maxCertificatePool {
			t.Fatal("unbounded partial allocation", i, pool.bytes, err)
		}
	}
	if pool.bytes != maxCertificatePool || len(pool.pending) != maxCertificatePending {
		t.Fatal(pool.bytes, len(pool.pending))
	}
	data := bytes.Repeat([]byte{255}, maxCertificateBytes)
	hash := certificateHash(data)
	if pool.claim(hash, pool.newOwner(), now) {
		t.Fatal("pending count exceeded")
	}
	pool.mu.Lock()
	pool.prune(now.Add(certificateFetchTTL))
	pool.mu.Unlock()
	if pool.bytes != 0 || len(pool.pending) != 0 {
		t.Fatal("expired assemblies retained", pool.bytes, len(pool.pending))
	}
	_, accepted, err := pool.accept(owners[0], CertificateChunk{Hash: certificateHash(bytes.Repeat([]byte{0}, maxCertificateBytes)), Offset: certificateChunkBytes, Total: maxCertificateBytes, Data: data[:certificateChunkBytes]}, now.Add(certificateFetchTTL))
	if err != nil || accepted || pool.bytes != 0 {
		t.Fatal("late data revived expired state", accepted, err)
	}
}

func TestCertificateExchangeUsesRealChainVerifier(t *testing.T) {
	// The two peers have different intermediate chains under a common root.
	// Missing intermediates must be fetched and verified, never made roots.
	a, b := largeChainCredentials(t, 6500)
	now := time.Now()
	open := func(own *Credentials) *IdentityCache {
		c, err := OpenIdentityCache("", DefaultCacheLimits(), own.Verify, now)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Put(own.Certificate.Certificate, now); err != nil {
			t.Fatal(err)
		}
		return c
	}
	source := NewCertificateExchange(open(a), a.Device.Tenant())
	c := open(b)
	receiver := NewCertificateExchange(c, b.Device.Tenant())
	requests, received, completed := driveCertificateExchange(t, source, receiver, source.Describe(a.Device.Principal(), now), now)
	if len(requests) <= 4 || received != chainSize(a.Certificate.Certificate) || !reflect.DeepEqual(completed, []string{a.Device.Principal()}) {
		t.Fatal("full unknown chain was not exercised", len(requests), received, completed)
	}
	if _, id, ok := c.Get(a.Device.Principal(), now); !ok || id.Device != a.Device {
		t.Fatal("assembled chain did not pass current-trust verification")
	}
	// Reuse the exact fetched bytes with a verifier that no longer trusts
	// them. Content-addressed storage must not bypass a new trust decision.
	rejected, err := OpenIdentityCacheWithCertificates("", DefaultCacheLimits(), func([][]byte, time.Time) (Identity, error) {
		return Identity{}, errors.New("trust withdrawn")
	}, now, c.Certificates())
	if err != nil {
		t.Fatal(err)
	}
	other := NewCertificateExchange(rejected, b.Device.Tenant())
	if _, _, err := other.Receive(*source.Describe(a.Device.Principal(), now), now); err == nil {
		t.Fatal("byte cache authorized a chain under changed trust")
	}
}
