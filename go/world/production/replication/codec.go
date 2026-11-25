package replication

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	replicav1 "github.com/wendylabsinc/wendy/go/world/production/api/world/replica/v1"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"github.com/wendylabsinc/wendy/go/world/production/proofref"
	"github.com/wendylabsinc/wendy/go/world/production/serviceaccess"
	"github.com/wendylabsinc/wendy/go/world/production/serviceorigin"
	"github.com/wendylabsinc/wendy/go/world/production/sourceauthority"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func hash(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func digest(s string) bool {
	b, e := hex.DecodeString(s)
	return e == nil && len(b) == 32 && hex.EncodeToString(b) == s
}
func bounded(s string) bool {
	return s != "" && len(s) <= 256 && utf8.ValidString(s) && strings.TrimSpace(s) == s && strings.IndexFunc(s, unicode.IsControl) < 0
}
func validEndpoint(e Endpoint) bool {
	tenant, err := serviceaccess.DeviceTenant(e.Device)
	if err != nil || tenant != e.Tenant || !bounded(e.Site) || !bounded(e.SampleClockDomain) {
		return false
	}
	if e.ExecutionDomain == "simulation" {
		return strings.HasPrefix(e.SampleClockDomain, "simulation:") && e.SampleClockDomain != "simulation:"
	}
	return e.ExecutionDomain == "physical" && !strings.HasPrefix(e.SampleClockDomain, "simulation:") && !strings.HasPrefix(e.SampleClockDomain, "replay:")
}
func (e Entry) actor() Actor {
	if e.Kind != Record {
		return e.Authority.Actor
	}
	s := e.Reference.Source()
	return Actor{s.Tenant, s.Site, s.Device, s.App, s.Service}
}
func validActor(a Actor, e Endpoint, origin string) bool {
	return a.Tenant == e.Tenant && a.Site == e.Site && a.Device == origin && (sourceauthority.Source{Tenant: a.Tenant, Device: a.Device, App: a.App, Service: a.Service}).Validate() == nil
}

// OrderKey is stable across versions and floor re-attestations. Its kind prefix
// ensures floors precede leases and records in each complete repair pass.
func (e Entry) OrderKey() string {
	var value any = e.Authority.Actor
	if e.Kind == Record {
		value = e.Reference.Key
	}
	b, _ := json.Marshal(value)
	return fmt.Sprintf("%d:%s", e.Kind, hash(b))
}
func validOrder(s string) bool {
	return len(s) == 66 && s[1] == ':' && s[0] >= '1' && s[0] <= '3' && digest(s[2:])
}
func validateEntry(e Entry, endpoint Endpoint, origin string) error {
	if !validActor(e.actor(), endpoint, origin) {
		return ErrProtocol
	}
	switch e.Kind {
	case Record:
		if e.Authority != (AuthorityHint{}) || e.Reference.Validate() != nil || !SupportedComponent(e.Reference.Key.Type) || e.ExpiresAtUnixNS <= 0 {
			return ErrProtocol
		}
	case Floor, Lease:
		a := e.Authority
		if e.Reference != (proofref.Reference{}) || e.ExpiresAtUnixNS != 0 || e.Tombstone || a.Revision == 0 || a.Epoch == 0 || !digest(a.EnvelopeSHA256) {
			return ErrProtocol
		}
		if e.Kind == Floor && (a.ThroughRevision == 0 || a.ThroughRevision > a.Revision) || e.Kind == Lease && a.ThroughRevision != 0 {
			return ErrProtocol
		}
	default:
		return ErrProtocol
	}
	return nil
}
func allowed(e Entry, sources []proofref.Source) bool {
	a := e.actor()
	for _, s := range sources {
		if s.Tenant != a.Tenant || s.Site != a.Site || s.Device != a.Device || s.App != a.App || s.Service != a.Service {
			continue
		}
		if e.Kind != Record || s == e.Reference.Source() {
			return true
		}
	}
	return false
}
func cloneItem(i Item) Item { return Item{Entry: i.Entry, Envelope: bytes.Clone(i.Envelope)} }

// ValidateItem binds the exact advertised hint to the unchanged original bytes.
// This is structural validation, not cryptographic or current authorization.
func ValidateItem(i Item, endpoint Endpoint, origin string) error {
	if validateEntry(i.Entry, endpoint, origin) != nil || len(i.Envelope) == 0 || len(i.Envelope) > MaxEnvelopeBytes {
		return ErrProtocol
	}
	a := i.Entry.Authority
	switch i.Entry.Kind {
	case Floor:
		_, c, e := sourceauthority.DecodeFloor(i.Envelope)
		if e != nil || hash(i.Envelope) != a.EnvelopeSHA256 || c.AttestationRevision != a.Revision || c.ThroughRevision != a.ThroughRevision || c.MinimumEpoch != a.Epoch || (Actor{c.Source.Tenant, c.Site, c.Source.Device, c.Source.App, c.Source.Service}) != a.Actor || c.ExecutionDomain != endpoint.ExecutionDomain || c.SampleClockDomain != endpoint.SampleClockDomain {
			return ErrProtocol
		}
	case Lease:
		_, c, e := sourceauthority.DecodeLease(i.Envelope)
		if e != nil || hash(i.Envelope) != a.EnvelopeSHA256 || c.AuthorityRevision != a.Revision || c.Epoch != a.Epoch || (Actor{c.Scope.Tenant, c.Scope.Site, c.Scope.Device, c.Scope.App, c.Scope.Service}) != a.Actor || c.Scope.ExecutionDomain != endpoint.ExecutionDomain || c.SampleClockDomain != endpoint.SampleClockDomain {
			return ErrProtocol
		}
	case Record:
		var signed serviceorigin.SignedRecord
		var record serviceorigin.Record
		if authority.DecodeStrictLimit(i.Envelope, &signed, MaxEnvelopeBytes) != nil || len(signed.Record) == 0 || len(signed.Record) > serviceorigin.MaxRecordBytes || jws.DecodeStrict(signed.Record, &record) != nil {
			return ErrProtocol
		}
		ref := i.Entry.Reference
		p := record.Publication
		key := world.Key{Tenant: record.Scope.Tenant, Site: record.Scope.Site, Entity: p.Entity, Type: p.ComponentType, Source: world.Source{Device: record.Scope.Device, App: record.Scope.App, Service: record.Scope.Service, Stream: p.Stream}}
		end := p.Timing.SampleTime.Add(p.TTL)
		if hash(i.Envelope) != ref.EnvelopeSHA256 || hash(signed.Record) != ref.RecordSHA256 || key != ref.Key || (world.Version{Epoch: record.Epoch, Sequence: record.Sequence}) != ref.Version || p.Tombstone != i.Entry.Tombstone || record.Scope.ExecutionDomain != endpoint.ExecutionDomain || p.Timing.ClockDomain != endpoint.SampleClockDomain || p.Timing.Validate() != nil || p.TTL <= 0 || !time.Unix(0, end.UnixNano()).Equal(end) || i.Entry.ExpiresAtUnixNS > end.UnixNano() {
			return ErrProtocol
		}
		if _, err := RecordInterpretationUntil(record); err != nil {
			return err
		}
	}
	return nil
}

func known(m protoreflect.Message) bool {
	if !m.IsValid() || len(m.GetUnknown()) != 0 {
		return false
	}
	ok := true
	m.Range(func(f protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if f.IsMap() {
			ok = false
			return false
		}
		if f.Kind() == protoreflect.MessageKind {
			if f.IsList() {
				l := v.List()
				for n := 0; n < l.Len(); n++ {
					if !known(l.Get(n).Message()) {
						ok = false
						break
					}
				}
			} else {
				ok = known(v.Message())
			}
		}
		return ok
	})
	return ok
}
func scopeWire(e Endpoint) *replicav1.Scope {
	return &replicav1.Scope{Tenant: e.Tenant, Site: e.Site, Device: e.Device, ExecutionDomain: e.ExecutionDomain, SampleClockDomain: e.SampleClockDomain}
}
func scopeEqual(p *replicav1.Scope, e Endpoint) bool {
	return p != nil && known(p.ProtoReflect()) && p.Tenant == e.Tenant && p.Site == e.Site && p.Device == e.Device && p.ExecutionDomain == e.ExecutionDomain && p.SampleClockDomain == e.SampleClockDomain
}
func actorWire(a Actor) *replicav1.Actor {
	return &replicav1.Actor{Tenant: a.Tenant, Site: a.Site, Device: a.Device, App: a.App, Service: a.Service}
}
func actorValue(p *replicav1.Actor) Actor {
	if p == nil {
		return Actor{}
	}
	return Actor{p.Tenant, p.Site, p.Device, p.App, p.Service}
}
func entryWire(e Entry) *replicav1.Entry {
	p := &replicav1.Entry{Kind: replicav1.Kind(e.Kind)}
	if e.Kind == Record {
		r := e.Reference
		p.Hint = &replicav1.Entry_Record{Record: &replicav1.RecordHint{ExpiresAtUnixNs: e.ExpiresAtUnixNS, Tombstone: e.Tombstone, Reference: &replicav1.RecordReference{Actor: actorWire(e.actor()), Entity: r.Key.Entity, ComponentType: r.Key.Type, Stream: r.Key.Source.Stream, Epoch: r.Version.Epoch, Sequence: r.Version.Sequence, EnvelopeSha256: r.EnvelopeSHA256, RecordSha256: r.RecordSHA256}}}
	} else {
		a := e.Authority
		p.Hint = &replicav1.Entry_Authority{Authority: &replicav1.AuthorityHint{Actor: actorWire(a.Actor), Revision: a.Revision, Epoch: a.Epoch, ThroughRevision: a.ThroughRevision, EnvelopeSha256: a.EnvelopeSHA256}}
	}
	return p
}
func entryValue(p *replicav1.Entry, e Endpoint, origin string) (Entry, error) {
	var out Entry
	if p == nil || !known(p.ProtoReflect()) || p.Kind < replicav1.Kind_KIND_FLOOR || p.Kind > replicav1.Kind_KIND_RECORD {
		return out, ErrProtocol
	}
	out.Kind = Kind(p.Kind)
	if out.Kind == Record {
		h := p.GetRecord()
		if h == nil || h.Reference == nil || h.Reference.Actor == nil {
			return out, ErrProtocol
		}
		r := h.Reference
		a := actorValue(r.Actor)
		out.Reference = proofref.Reference{Key: world.Key{Tenant: a.Tenant, Site: a.Site, Entity: r.Entity, Type: r.ComponentType, Source: world.Source{Device: a.Device, App: a.App, Service: a.Service, Stream: r.Stream}}, Version: world.Version{Epoch: r.Epoch, Sequence: r.Sequence}, EnvelopeSHA256: r.EnvelopeSha256, RecordSHA256: r.RecordSha256}
		out.ExpiresAtUnixNS = h.ExpiresAtUnixNs
		out.Tombstone = h.Tombstone
	} else {
		h := p.GetAuthority()
		if h == nil || h.Actor == nil {
			return out, ErrProtocol
		}
		out.Authority = AuthorityHint{Actor: actorValue(h.Actor), Revision: h.Revision, Epoch: h.Epoch, ThroughRevision: h.ThroughRevision, EnvelopeSHA256: h.EnvelopeSha256}
	}
	return out, validateEntry(out, e, origin)
}
