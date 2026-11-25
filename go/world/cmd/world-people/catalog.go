package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"os"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/people"
	"github.com/wendylabsinc/wendy/go/world/internal/presence"
	"github.com/wendylabsinc/wendy/go/world/internal/tracking"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
	"github.com/wendylabsinc/wendy/go/world/internal/zonepub"
)

type catalogStatus struct {
	SHA256                   string       `json:"sha256"`
	Source                   world.Source `json:"source"`
	State                    string       `json:"state"`
	CheckedAt                time.Time    `json:"checked_at"`
	CurrentRemoteState       string       `json:"current_remote_state"`
	HistoricalGeometryPinned bool         `json:"historical_geometry_pinned"`
}
type catalogMonitor struct {
	c         *client.Client
	v         *authority.Verifier
	root      []byte
	trustPath string
	selected  zonepub.Selection
	status    catalogStatus
	terminal  bool
}

func boundedFile(path string, limit int64, out any) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	raw, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e != nil {
		return e
	}
	if int64(len(raw)) > limit {
		return errors.New("catalog configuration exceeds bound")
	}
	return authority.DecodeStrictLimit(raw, out, int(limit))
}

type catalogTrust struct {
	Root        []byte                      `json:"root_public_key"`
	Revocations authority.SignedRevocations `json:"revocations"`
}

func (m *catalogMonitor) reloadTrust() error {
	var trust catalogTrust
	if err := boundedFile(m.trustPath, 1<<20, &trust); err != nil {
		return err
	}
	if !bytes.Equal(m.root, trust.Root) {
		return errors.New("catalog trust root changed")
	}
	return m.v.UpdateRevocations(trust.Revocations)
}
func openCatalog(credentials, trustPath, digest, sourcePath string) (*catalogMonitor, error) {
	var cfg client.Config
	var source world.Source
	var trust catalogTrust
	if err := boundedFile(credentials, 1<<20, &cfg); err != nil {
		return nil, err
	}
	if cfg.Grant.Grant.Domain != "simulation" {
		return nil, errors.New("catalog presence supports simulation only")
	}
	if err := boundedFile(sourcePath, 4096, &source); err != nil {
		return nil, err
	}
	if source.Validate() != nil || !zonepub.ValidDigest(digest) {
		return nil, errors.New("invalid catalog selection")
	}
	if err := boundedFile(trustPath, 1<<20, &trust); err != nil {
		return nil, err
	}
	v, err := authority.NewVerifier(authority.Config{RootPublicKey: ed25519.PublicKey(trust.Root), Tenant: cfg.Grant.Grant.Tenant, Site: cfg.Grant.Grant.Site, Domain: "simulation", Audience: cfg.Audience, Now: time.Now, Replay: authority.NewMemoryReplay(10000)})
	if err != nil {
		return nil, err
	}
	c, err := client.New(cfg)
	if err != nil {
		return nil, err
	}
	m := &catalogMonitor{c: c, v: v, root: bytes.Clone(trust.Root), trustPath: trustPath, status: catalogStatus{SHA256: digest, Source: source, HistoricalGeometryPinned: true}}
	if err = m.reloadTrust(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reply, err := c.Zones(ctx, v)
	if err != nil {
		return nil, err
	}
	m.selected, err = client.SelectZone(reply, digest, source, time.Now())
	if err != nil {
		return nil, err
	}
	if m.selected.Publication == nil {
		return nil, errors.New("selected catalog is deleted")
	}
	m.status.State = "available_at_replica"
	m.status.CurrentRemoteState = "checked_at_replica"
	m.status.CheckedAt = m.selected.CheckedAt
	return m, nil
}

// check never changes the selected geometry or its proof deadline. A confirmed
// deletion, invalid authority or expiry latches closed until a new process starts.
func (m *catalogMonitor) fail(state string) bool {
	m.status.State = state
	m.status.CurrentRemoteState = "unavailable"
	m.terminal = true
	return false
}

// verifyPinned is also called after blocking I/O and after inference. Authority
// verified before either operation cannot be reserved across its duration.
func (m *catalogMonitor) verifyPinned() bool {
	if m.terminal {
		return false
	}
	if m.reloadTrust() != nil {
		return m.fail("trust_unavailable")
	}
	if _, err := zonepub.Verify(m.selected.Record, m.v, time.Now()); err != nil {
		return m.fail("expired_or_revoked")
	}
	return true
}
func (m *catalogMonitor) check() bool {
	if !m.verifyPinned() {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	reply, err := m.c.Zones(ctx, m.v)
	if !m.verifyPinned() {
		return false
	}
	if err != nil {
		if client.Transient(err) {
			m.status.State = "cached_snapshot"
			m.status.CurrentRemoteState = "unknown"
			return true
		}
		return m.fail("verification_or_access_failed")
	}
	selected, err := client.SelectZone(reply, m.status.SHA256, m.status.Source, time.Now())
	if err != nil {
		return m.fail("unavailable_at_replica")
	}
	m.status.CheckedAt = selected.CheckedAt
	if selected.Record.Component.Tombstone {
		return m.fail("deleted_at_replica")
	}
	// A newer refresh of the same content does not renew this session's proof.
	m.status.State = "available_at_replica"
	m.status.CurrentRemoteState = "checked_at_replica"
	return true
}
func (m *catalogMonitor) bind(req *people.Request) error {
	d := m.selected.Publication.Catalog
	if req.RunID != d.RunID || req.Frame != d.Frame {
		return errors.New("request run/frame differs from pinned zone catalog")
	}
	for _, image := range req.Images {
		if image.Sample.SiteID != d.Site {
			return errors.New("image site differs from pinned zone catalog")
		}
	}
	e, err := presence.New(d.Zones, d.RunID, "simulation:"+d.RunID, d.Frame)
	if err != nil {
		return err
	}
	if len(req.Zones) > 0 && !e.Matches(req.Zones) {
		return errors.New("request zones differ from pinned catalog")
	}
	req.Zones = append([]presence.Zone(nil), d.Zones...)
	return nil
}
func suppressedHistory(out *people.Output) {
	out.Tracks = []tracking.Track{}
	out.Cameras = []people.CameraResult{}
	if out.Presence == nil {
		return
	}
	for i := range out.Presence.Zones {
		z := &out.Presence.Zones[i]
		z.State = "unknown"
		z.Observed = 0
		z.Possible = 0
		z.Occupants = []presence.Occupant{}
	}
}
func catalogOutput(out people.Output, m *catalogMonitor) any {
	return struct {
		people.Output
		Catalog catalogStatus `json:"zone_catalog"`
	}{out, m.status}
}
