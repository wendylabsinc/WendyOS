package incidentchange

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/gasreplica"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type pairFixture struct {
	cfg       Config
	records   []world.Component
	now       time.Time
	root, key ed25519.PrivateKey
	grant     authority.SignedGrant
	verifier  *authority.Verifier
}

func signedPairFixture(t *testing.T) *pairFixture {
	t.Helper()
	cfg, records := joinedFixture(t, "rising")
	pub, root, _ := ed25519.GenerateKey(rand.Reader)
	memberPub, key, _ := ed25519.GenerateKey(rand.Reader)
	f := &pairFixture{cfg: cfg, records: records, now: cfg.Spatial.Gas.Instant(2.1), root: root, key: key}
	g := cfg.Spatial.Gas
	grant, err := authority.SignGrant(authority.Grant{Version: authority.Version, ID: "paired-origin", Tenant: g.Tenant, Site: g.Field.Site, Domain: "simulation", Principal: "east", Kind: "app", Source: g.Gateways["east"], PublicKey: memberPub, Scopes: []string{"publish"}, PublishAudience: "node-1", NotBeforeUnixMS: g.Anchor.UnixMilli(), NotAfterUnixMS: g.Instant(20).UnixMilli()}, root)
	if err != nil {
		t.Fatal(err)
	}
	f.grant = grant
	f.verifier, err = authority.NewVerifier(authority.Config{RootPublicKey: pub, Tenant: g.Tenant, Site: g.Field.Site, Domain: "simulation", Audience: "reader", Now: func() time.Time { return f.now }, Replay: authority.NewMemoryReplay(4096), MaxBodyBytes: 64 << 10})
	if err != nil {
		t.Fatal(err)
	}
	f.revoke(t, 1, nil)
	return f
}
func (f *pairFixture) revoke(t *testing.T, revision uint64, ids []string) {
	t.Helper()
	g := f.cfg.Spatial.Gas
	r, err := authority.SignRevocations(authority.Revocations{Version: authority.Version, Tenant: g.Tenant, Site: g.Field.Site, Domain: "simulation", Revision: revision, IssuedAtUnixMS: g.Anchor.UnixMilli(), ExpiresAtUnixMS: g.Instant(20).UnixMilli(), GrantIDs: ids}, f.root)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.verifier.UpdateRevocations(r); err != nil {
		t.Fatal(err)
	}
}
func (f *pairFixture) verify(records []authority.SignedRecord) error {
	_, err := f.verifier.VerifyRecords(records)
	return err
}
func (f *pairFixture) pair(t *testing.T, sequence uint64) (world.Component, [][]byte) {
	t.Helper()
	members := [][]byte{}
	v := world.Version{Epoch: 1, Sequence: sequence}
	for _, index := range []int{0, 2} {
		c := own(f.records[index])
		c.Version = v
		s, err := authority.SignRecord(f.grant, f.key, c, f.now)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(s)
		members = append(members, raw)
	}
	c, err := MakePair(f.cfg, "east", v, members, f.now, f.verify)
	if err != nil {
		t.Fatal(err)
	}
	return c, members
}
func TestSignedPairPreservesMembersAndCompleteEnvelope(t *testing.T) {
	f := signedPairFixture(t)
	c, members := f.pair(t, 2)
	p, err := ValidatePair(f.cfg, c, f.now, f.verify)
	if err != nil {
		t.Fatal(err)
	}
	for i := range members {
		if !bytes.Equal(members[i], p.Members[i]) {
			t.Fatal("signed member bytes changed")
		}
	}
	s, _ := NewWithPairVerifier(f.cfg, f.verify)
	result, err := s.Step([]world.Component{c, f.records[1]}, f.now)
	if err != nil || result.State != "supported_rise" || len(result.Carriers) != 1 {
		t.Fatalf("pair did not join: %+v %v", result, err)
	}
	if result.Carriers[0].Hash != gasreplica.ComponentHash(c) || result.Carriers[0].GasHash != gasreplica.ComponentHash(f.records[0]) || result.ValidUntil.After(c.Timing.SampleTime.Add(c.TTL)) {
		t.Fatal("carrier hash/lifetime binding differs")
	}
	outer, err := authority.SignRecord(f.grant, f.key, c, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if err = gasreplica.CheckSignedSize(outer); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(outer)
	t.Logf("complete signed pair: %d bytes", len(raw))
	members[0][0] = 'x'
	if _, err = ValidatePair(f.cfg, c, f.now, f.verify); err != nil {
		t.Fatal("input mutation altered owned carrier", err)
	}
	legacy, _ := New(f.cfg)
	r, err := legacy.Step(f.records, f.now)
	if err != nil || r.State != "supported_rise" {
		t.Fatal("separate path regressed", r.State, err)
	}
	uncommissioned, _ := New(f.cfg)
	r, err = uncommissioned.Step([]world.Component{c, f.records[1]}, f.now)
	if err != nil || r.State == "supported_rise" {
		t.Fatal("carrier substituted for required member verification")
	}
}
func TestPairRejectsPartialTamperedMixedAndOversizedMembers(t *testing.T) {
	f := signedPairFixture(t)
	c, members := f.pair(t, 2)
	for name, mutate := range map[string]func(*Pair){
		"partial": func(p *Pair) { p.Members = p.Members[:1] },
		"order":   func(p *Pair) { p.Members[0], p.Members[1] = p.Members[1], p.Members[0] },
		"hash":    func(p *Pair) { p.Hashes[0] = p.Hashes[1] },
		"schema":  func(p *Pair) { p.Schema = "foreign" },
		"signature": func(p *Pair) {
			var signed authority.SignedRecord
			json.Unmarshal(p.Members[0], &signed)
			signed.Signature[0] ^= 1
			p.Members[0], _ = json.Marshal(signed)
		},
		"original": func(p *Pair) {
			var signed authority.SignedRecord
			json.Unmarshal(p.Members[0], &signed)
			signed.Component.Payload = append(signed.Component.Payload[:len(signed.Component.Payload)-1], []byte(",\"tampered\":true}")...)
			p.Members[0], _ = json.Marshal(signed)
		},
		"oversize": func(p *Pair) { p.Members[0] = bytes.Repeat([]byte(" "), MaxPairBytes) },
	} {
		t.Run(name, func(t *testing.T) {
			bad := own(c)
			var p Pair
			json.Unmarshal(bad.Payload, &p)
			mutate(&p)
			bad.Payload, _ = json.Marshal(p)
			if _, err := ValidatePair(f.cfg, bad, f.now, f.verify); err == nil {
				t.Fatal("invalid pair admitted")
			}
		})
	}
	var signed authority.SignedRecord
	json.Unmarshal(members[1], &signed)
	signed.Component.Version.Epoch = 2
	signed, err := authority.SignRecord(f.grant, f.key, signed.Component, f.now)
	if err != nil {
		t.Fatal(err)
	}
	mixed := own(members)
	mixed[1], _ = json.Marshal(signed)
	if _, err = MakePair(f.cfg, "east", c.Version, mixed, f.now, f.verify); err == nil {
		t.Fatal("mixed epoch admitted")
	}
	if _, err = MakePair(f.cfg, "west", c.Version, members, f.now, f.verify); err == nil {
		t.Fatal("wrong commissioned carrier publisher accepted")
	}
	f.revoke(t, 2, []string{f.grant.Grant.ID})
	if _, err = ValidatePair(f.cfg, c, f.now, f.verify); err == nil {
		t.Fatal("revoked member admitted")
	}
}
func TestPairConflictReplayExpiryAndNewerRecovery(t *testing.T) {
	f := signedPairFixture(t)
	first, _ := f.pair(t, 2)
	s, _ := NewWithPairVerifier(f.cfg, f.verify)
	step := func(c world.Component, at float64) Result {
		f.now = f.cfg.Spatial.Gas.Instant(at)
		r, err := s.Step([]world.Component{c, f.records[1]}, f.now)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := step(first, 2.1); r.State != "supported_rise" {
		t.Fatal(r.State)
	}
	f.now = f.cfg.Spatial.Gas.Instant(2.12)
	conflict, _ := f.pair(t, 2)
	if gasreplica.ComponentHash(first) == gasreplica.ComponentHash(conflict) {
		t.Fatal("fixture is not a conflict")
	}
	if r := step(conflict, 2.13); r.State == "supported_rise" {
		t.Fatal("equal-version conflict admitted")
	}
	if r := step(first, 2.14); r.State == "supported_rise" {
		t.Fatal("replay restored conflicted generation")
	}
	f.now = f.cfg.Spatial.Gas.Instant(2.15)
	newer, _ := f.pair(t, 3)
	if r := step(newer, 2.16); r.State != "supported_rise" {
		t.Fatal("newer did not recover", r.State)
	}
	if r := step(first, 2.17); r.State == "supported_rise" {
		t.Fatal("older delivery restored obsolete generation")
	}
	// A duplicate with a tighter admitted remaining lifetime is never renewed by
	// later delivery of the unaged original envelope.
	fresh, _ := NewWithPairVerifier(f.cfg, f.verify)
	aged := newer
	aged.Timing.AgeHint = aged.TTL - 80*time.Millisecond
	f.now = f.cfg.Spatial.Gas.Instant(2.18)
	r, err := fresh.Step([]world.Component{aged, f.records[1]}, f.now)
	if err != nil || r.State != "supported_rise" {
		t.Fatal(r.State, err)
	}
	until := r.ValidUntil
	f.now = f.cfg.Spatial.Gas.Instant(2.20)
	r, err = fresh.Step([]world.Component{newer, f.records[1]}, f.now)
	if err != nil || r.ValidUntil.After(until) {
		t.Fatal("duplicate renewed pair", r, err)
	}
	f.now = f.cfg.Spatial.Gas.Instant(2.27)
	r, err = fresh.Step([]world.Component{newer, f.records[1]}, f.now)
	if err != nil || r.State == "supported_rise" {
		t.Fatal("expired pair current", r, err)
	}
}

func TestPairAndSeparateMemberConflictLatchesClosed(t *testing.T) {
	f := signedPairFixture(t)
	carrier, _ := f.pair(t, 2)
	s, _ := NewWithPairVerifier(f.cfg, f.verify)
	conflict := own(f.records[0])
	conflict.Provenance.AppVersion = "different"
	r, err := s.Step([]world.Component{carrier, f.records[1], conflict}, f.now)
	if err != nil || r.State == "supported_rise" {
		t.Fatal("separate same-version conflict ignored", r.State, err)
	}
	f.now = f.cfg.Spatial.Gas.Instant(2.12)
	r, err = s.Step([]world.Component{carrier, f.records[1]}, f.now)
	if err != nil || r.State == "supported_rise" {
		t.Fatal("carrier replay restored conflicting generation", r.State, err)
	}
	f.now = f.cfg.Spatial.Gas.Instant(2.13)
	next, _ := f.pair(t, 3)
	r, err = s.Step([]world.Component{next, f.records[1]}, f.now)
	if err != nil || r.State != "supported_rise" {
		t.Fatal("new pair did not recover", r.State, err)
	}

	legacy := own(f.records)
	legacy[0].Version.Sequence = 4
	legacy[2].Version.Sequence = 4
	f.now = f.cfg.Spatial.Gas.Instant(2.14)
	r, err = s.Step(legacy, f.now)
	if err != nil || r.State != "supported_rise" || len(r.Carriers) != 0 {
		t.Fatal("newer separate claims did not remain compatible", r.State, err)
	}
}
