package patrolreplica

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/client"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type Backend interface {
	NextEpoch(context.Context) (uint64, error)
	Upload(context.Context, []byte, string, time.Time) error
	Publish(context.Context, []byte) error
}
type NodeBackend struct{ Client *client.Client }

func (n NodeBackend) NextEpoch(ctx context.Context) (uint64, error) { return n.Client.NextEpoch(ctx) }
func (n NodeBackend) Upload(ctx context.Context, b []byte, kind string, until time.Time) error {
	_, e := n.Client.UploadEvidence(ctx, b, kind, until)
	return e
}
func (n NodeBackend) Publish(ctx context.Context, b []byte) error {
	return n.Client.Do(ctx, "publish", http.MethodPost, "/v1/components", b, nil)
}

type pending struct {
	ID   string          `json:"id"`
	Body json.RawMessage `json:"body"`
}
type journal struct {
	Version  int               `json:"version"`
	Binding  string            `json:"binding"`
	Epoch    uint64            `json:"epoch"`
	Sequence uint64            `json:"sequence"`
	LastLive uint64            `json:"last_live"`
	Done     map[string]string `json:"done"`
	Pending  *pending          `json:"pending,omitempty"`
}
type Publisher struct {
	cfg         Config
	credentials client.Config
	backend     Backend
	path        string
	lock        *os.File
	j           journal
	fault       error
}

func ReadFile(path string, limit int64) ([]byte, error) {
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	info, e := f.Stat()
	if e != nil {
		return nil, e
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("input is not a bounded regular file")
	}
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(b)) > limit {
		return nil, errors.New("input exceeds bound")
	}
	return b, e
}
func AtomicJSON(path string, v any) error {
	b, e := json.Marshal(v)
	if e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".patrol-debrief-*")
	if e != nil {
		return e
	}
	name := f.Name()
	defer os.Remove(name)
	if _, e = f.Write(append(b, '\n')); e == nil {
		e = f.Sync()
	}
	ce := f.Close()
	if e == nil {
		e = ce
	}
	if e != nil {
		return e
	}
	if e = os.Rename(name, path); e != nil {
		return e
	}
	d, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func OpenPublisher(ctx context.Context, c Config, credentials client.Config, backend Backend, path string) (*Publisher, error) {
	if e := c.Validate(); e != nil {
		return nil, e
	}
	g := credentials.Grant.Grant
	if g.Source != c.Source || g.Tenant != c.Tenant || g.Site != c.Site || g.Domain != "simulation" || c.RetainUntilMS > g.NotAfterUnixMS || credentials.Audience != g.PublishAudience || len(credentials.PrivateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("publisher credentials do not bind configured scope/retention")
	}
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return nil, e
	}
	lock, e := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		lock.Close()
		return nil, errors.New("publisher journal already owned")
	}
	p := &Publisher{cfg: c, credentials: credentials, backend: backend, path: path, lock: lock}
	binding := JSONHash(struct {
		Config   Config
		Grant    authority.SignedGrant
		Audience string
	}{c, credentials.Grant, credentials.Audience})
	b, e := ReadFile(path, 2<<20)
	if errors.Is(e, os.ErrNotExist) {
		epoch, e := backend.NextEpoch(ctx)
		if e != nil {
			p.Close()
			return nil, e
		}
		p.j = journal{Version: 1, Binding: binding, Epoch: epoch, Done: map[string]string{}}
		e = p.save()
		if e != nil {
			p.Close()
			return nil, e
		}
	} else if e != nil {
		p.Close()
		return nil, e
	} else if e = Decode(b, &p.j, 2<<20); e != nil || p.j.Version != 1 || p.j.Binding != binding || p.j.Epoch == 0 || p.j.Done == nil || len(p.j.Done) > MaxReports {
		p.Close()
		return nil, errors.New("publisher journal invalid or scope changed")
	}
	if p.j.Pending != nil {
		var r authority.SignedRecord
		if json.Unmarshal(p.j.Pending.Body, &r) != nil || r.Component.Version.Epoch != p.j.Epoch || r.Component.Version.Sequence != p.j.Sequence || r.Component.Key.Source != c.Source || r.Component.Key.Type != ReportType {
			p.Close()
			return nil, errors.New("invalid pending origin record")
		}
		var report Report
		if json.Unmarshal(r.Component.Payload, &report) != nil || report.Validate(c) != nil || report.ID != p.j.Pending.ID {
			p.Close()
			return nil, errors.New("invalid pending historical report")
		}
	}
	return p, nil
}
func (p *Publisher) Close() {
	if p.lock != nil {
		_ = syscall.Flock(int(p.lock.Fd()), syscall.LOCK_UN)
		_ = p.lock.Close()
		p.lock = nil
	}
}
func (p *Publisher) save() error {
	if p.fault != nil {
		return p.fault
	}
	e := AtomicJSON(p.path, p.j)
	if e != nil {
		p.fault = e
	}
	return e
}
func (p *Publisher) Flush(ctx context.Context) error {
	if p.fault != nil {
		return p.fault
	}
	if p.j.Pending == nil {
		return nil
	}
	if time.Now().UnixMilli() >= p.cfg.RetainUntilMS {
		return errors.New("fixed debrief retention elapsed")
	}
	if e := p.backend.Publish(ctx, p.j.Pending.Body); e != nil {
		return e
	}
	var r authority.SignedRecord
	_ = json.Unmarshal(p.j.Pending.Body, &r)
	p.j.Done[p.j.Pending.ID] = Hash(r.Component.Payload)
	p.j.Pending = nil
	return p.save()
}
func (p *Publisher) Count() int { return len(p.j.Done) }
func (p *Publisher) Report(ctx context.Context, r Report, blobs map[string][]byte, now time.Time) error {
	if e := r.Validate(p.cfg); e != nil {
		return e
	}
	if e := p.Flush(ctx); e != nil {
		return e
	}
	payload, _ := json.Marshal(r)
	if prior, ok := p.j.Done[r.ID]; ok {
		if prior != Hash(payload) {
			return errors.New("immutable report collision")
		}
		return nil
	}
	if len(p.j.Done) >= MaxReports || (r.Kind == "capture" && len(p.j.Done) >= MaxReports-1) {
		return errors.New("bounded report journal full")
	}
	for _, ref := range r.Evidence {
		b, ok := blobs[ref.SHA256]
		if !ok || Hash(b) != ref.SHA256 || int64(len(b)) != ref.Size {
			return errors.New("original missing before upload")
		}
		if e := p.backend.Upload(ctx, b, ref.ContentType, time.UnixMilli(p.cfg.RetainUntilMS)); e != nil {
			return e
		}
	}
	p.j.Sequence++
	component, e := Component(p.cfg, r, p.j.Epoch, p.j.Sequence, now)
	if e != nil {
		return e
	}
	signed, e := authority.SignRecord(p.credentials.Grant, p.credentials.PrivateKey, component, now)
	if e != nil {
		return e
	}
	body, e := json.Marshal(signed)
	if e != nil {
		return e
	}
	if len(body) > 32<<10 {
		return errors.New("report exceeds origin bound")
	}
	p.j.Pending = &pending{r.ID, body}
	if e = p.save(); e != nil {
		return e
	}
	return p.Flush(ctx)
}
func (p *Publisher) Progress(ctx context.Context, s Snapshot, v world.Component, now time.Time) error {
	if e := p.Flush(ctx); e != nil {
		return e
	}
	if p.fault != nil {
		return p.fault
	}
	if v.Version.Sequence <= p.j.LastLive || !now.Before(v.Timing.SampleTime.Add(v.TTL)) {
		return nil
	}
	p.j.Sequence++
	p.j.LastLive = v.Version.Sequence
	if e := p.save(); e != nil {
		return e
	}
	c := Live(p.cfg, s, v, p.j.Epoch, p.j.Sequence)
	signed, e := authority.SignRecord(p.credentials.Grant, p.credentials.PrivateKey, c, now)
	if e != nil {
		return e
	}
	b, _ := json.Marshal(signed)
	return p.backend.Publish(ctx, b)
}

// Collect imports at most 64 local captures. Older un-attributed files are held
// out rather than assigning a room from receipt time or simulation truth.
func (p *Publisher) Collect(ctx context.Context, dir string, s Snapshot, v world.Component, now time.Time) error {
	if e := p.Flush(ctx); e != nil {
		return e
	}
	if Terminal(s.Patrol.State) {
		r, e := Result(p.cfg, s, v)
		if e != nil {
			return e
		}
		if e = p.Report(ctx, r, nil, now); e != nil {
			return e
		}
	}
	entries, e := os.ReadDir(dir)
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return e
	}
	names := []string{}
	for _, entry := range entries {
		if !entry.IsDir() && filepath.Ext(entry.Name()) == ".json" {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	if len(names) > 64 {
		return errors.New("capture directory exceeds 64 originals")
	}
	for _, name := range names {
		b, e := ReadFile(filepath.Join(dir, name), MaxSourceBytes)
		if e != nil {
			return e
		}
		r, blobs, e := Capture(p.cfg, s, b)
		if e != nil {
			if len(r.RoomIDs) == 0 {
				continue
			}
			return fmt.Errorf("capture %s: %w", name, e)
		}
		if p.Count() >= MaxReports-1 {
			continue
		}
		if e = p.Report(ctx, r, blobs, now); e != nil {
			return e
		}
	}
	return nil
}
