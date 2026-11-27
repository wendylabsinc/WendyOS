package viewerbridge

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/node"
	appv1 "github.com/wendylabsinc/wendy/go/world/production/api/world/app/v1"
	"github.com/wendylabsinc/wendy/go/world/production/jws"
	"golang.org/x/sys/unix"
)

// HTTPConfig contains only public trust/configuration, never a bridge signing
// key. Existing World read credentials authenticate each HTTP request.
type HTTPConfig struct {
	Version                 int                         `json:"version"`
	Listen                  string                      `json:"listen"`
	AllowInsecureSimulation bool                        `json:"allow_insecure_simulation"`
	Audience                string                      `json:"audience"`
	RootPublicKey           []byte                      `json:"root_public_key"`
	Revocations             authority.SignedRevocations `json:"revocations"`
	RequestTimeoutMS        int64                       `json:"request_timeout_ms,omitempty"`
	Reader                  Config                      `json:"reader"`
	EvidenceSources         []EvidenceSource            `json:"evidence_sources,omitempty"`
}

func (c HTTPConfig) Normalized() (HTTPConfig, error) {
	reader, err := c.Reader.Normalized()
	if err != nil {
		return c, err
	}
	if c.RequestTimeoutMS == 0 {
		c.RequestTimeoutMS = 5000
	}
	host, port, err := net.SplitHostPort(c.Listen)
	number, pe := strconv.Atoi(port)
	if c.Version != 1 || !c.AllowInsecureSimulation || err != nil || pe != nil || net.ParseIP(host) == nil || number < 1 || number > 65535 || c.Audience == "" || len(c.Audience) > 256 || len(c.RootPublicKey) != ed25519.PublicKeySize || c.RequestTimeoutMS < 100 || c.RequestTimeoutMS > 10000 {
		return c, ErrUnavailable
	}
	c.Reader = reader
	c.EvidenceSources, err = normalizedEvidenceSources(c.EvidenceSources, reader.People.Device)
	if err != nil {
		return c, err
	}
	c.RootPublicKey = append([]byte(nil), c.RootPublicKey...)
	return c, nil
}

func LoadConfig(path string) (HTTPConfig, error) {
	var c HTTPConfig
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return c, ErrUnavailable
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return c, err
	}
	f := os.NewFile(uintptr(fd), "native-viewer-config")
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0022 != 0 || st.Size() < 1 || st.Size() > 256<<10 {
		return c, ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(f, 256<<10+1))
	if err != nil || len(raw) > 256<<10 || jws.DecodeStrict(raw, &c) != nil {
		return c, ErrUnavailable
	}
	return c.Normalized()
}

type HTTPBridge struct {
	Handler http.Handler
	session *Session
	replay  *authority.ReplayCache
}

func NewHTTP(config HTTPConfig, api appv1.WorldAppServiceClient) (*HTTPBridge, error) {
	return newHTTP(config, api, time.Now)
}

func newHTTP(config HTTPConfig, api appv1.WorldAppServiceClient, wall func() time.Time) (*HTTPBridge, error) {
	c, err := config.Normalized()
	if err != nil {
		return nil, err
	}
	s, err := New(c.Reader, api)
	if err != nil {
		return nil, err
	}
	cache := authority.NewMemoryReplay(4096)
	cleanup := func() { s.Close(); cache.Close() }
	p := c.Reader.People
	v, err := authority.NewVerifier(authority.Config{RootPublicKey: ed25519.PublicKey(c.RootPublicKey), Tenant: p.Tenant, Site: p.Site, Domain: "simulation", Audience: c.Audience, Now: wall, Replay: cache})
	if err != nil {
		cleanup()
		return nil, err
	}
	if err = v.UpdateRevocations(c.Revocations); err != nil {
		cleanup()
		return nil, err
	}
	h, err := node.NewComponentReadHandler(node.ComponentReadConfig{Tenant: p.Tenant, Site: p.Site, Audience: c.Audience, MaxRecords: 16, RequestTimeout: time.Duration(c.RequestTimeoutMS) * time.Millisecond}, v, s.Query)
	if err != nil {
		cleanup()
		return nil, err
	}
	if len(c.EvidenceSources) > 0 {
		evidence, e := node.NewNativeEvidenceReadHandler(node.ComponentReadConfig{Tenant: p.Tenant, Site: p.Site, Audience: c.Audience, RequestTimeout: time.Duration(c.RequestTimeoutMS) * time.Millisecond}, v, func(ctx context.Context, q node.NativeEvidenceRequest) (node.NativeEvidenceResponse, error) {
			return s.readEvidence(ctx, q, c.EvidenceSources)
		})
		if e != nil {
			cleanup()
			return nil, e
		}
		components := h
		h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == node.NativeEvidencePath {
				evidence.ServeHTTP(w, r)
				return
			}
			components.ServeHTTP(w, r)
		})
	}
	return &HTTPBridge{Handler: h, session: s, replay: cache}, nil
}

func (b *HTTPBridge) Close() error {
	if b == nil {
		return nil
	}
	return errors.Join(b.session.Close(), b.replay.Close())
}
