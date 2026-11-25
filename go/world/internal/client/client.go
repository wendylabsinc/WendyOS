// Package client implements the initial signed World HTTP client contract.
package client

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wendylabsinc/wendy/go/world/internal/annotationclock"
	"github.com/wendylabsinc/wendy/go/world/internal/authority"
	"github.com/wendylabsinc/wendy/go/world/internal/selection"
	"github.com/wendylabsinc/wendy/go/world/internal/world"
)

type Config struct {
	ClockTrust              *annotationclock.Pins `json:"clock_trust,omitempty"`
	URL                     string                `json:"url"`
	Audience                string                `json:"audience"`
	Grant                   authority.SignedGrant `json:"grant"`
	PrivateKey              []byte                `json:"private_key"`
	AllowInsecureSimulation bool                  `json:"allow_insecure_simulation"`
}
type Client struct {
	config Config
	http   *http.Client
}
type Snapshot struct {
	ReplicaScope *selection.ReplicaScope `json:"replica_scope,omitempty"`
	Components   []world.Component       `json:"components"`
	Cursor       string                  `json:"cursor"`
}

// HTTPStatusError preserves the existing diagnostic text while allowing callers
// to report only the numeric status when the response body must stay private.
type HTTPStatusError struct {
	StatusCode int
	message    string
}

func (e *HTTPStatusError) Error() string { return e.message }

func New(c Config) (*Client, error) {
	u, e := url.Parse(c.URL)
	if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || c.Audience == "" {
		return nil, errors.New("invalid node URL or audience")
	}
	if u.Scheme != "https" && !(u.Scheme == "http" && c.AllowInsecureSimulation && c.Grant.Grant.Domain == "simulation") {
		return nil, errors.New("HTTP requires explicit simulation-only configuration")
	}
	if len(c.PrivateKey) != ed25519.PrivateKeySize || len(c.Grant.Grant.PublicKey) != ed25519.PublicKeySize || !bytes.Equal(c.PrivateKey[32:], c.Grant.Grant.PublicKey) {
		return nil, errors.New("credential private key does not match grant")
	}
	if c.ClockTrust != nil {
		p := *c.ClockTrust
		decoded, err := hex.DecodeString(p.GatewayKeySHA256)
		if c.Grant.Grant.Domain != "simulation" || len(p.RootPublicKey) != ed25519.PublicKeySize || p.GatewayPrincipal == "" || len(p.GatewayPrincipal) > 256 || err != nil || len(decoded) != 32 || p.RunID == "" || p.ClockDomain != "simulation:"+p.RunID {
			return nil, errors.New("invalid annotation clock trust")
		}
		p.RootPublicKey = bytes.Clone(p.RootPublicKey)
		c.ClockTrust = &p
	}
	c.URL = strings.TrimSuffix(c.URL, "/")
	c.PrivateKey = append([]byte(nil), c.PrivateKey...)
	return &Client{config: c, http: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("node redirects are forbidden") }}}, nil
}

func (c *Client) Do(ctx context.Context, operation, method, path string, body []byte, out any) error {
	return c.doAt(ctx, operation, method, path, body, out, time.Now(), time.Minute)
}
func (c *Client) doAt(ctx context.Context, operation, method, path string, body []byte, out any, issued time.Time, lifetime time.Duration) error {
	if !strings.HasPrefix(path, "/v1/") || strings.Contains(path, "#") || len(body) > 1<<20 {
		return errors.New("invalid API path or body size")
	}
	nonce := make([]byte, 24)
	if _, e := rand.Read(nonce); e != nil {
		return e
	}
	signed, e := authority.SignRequest(c.config.Grant, ed25519.PrivateKey(c.config.PrivateKey), c.config.Audience, operation, body, hex.EncodeToString(nonce), issued, lifetime)
	if e != nil {
		return e
	}
	header, e := json.Marshal(signed)
	if e != nil {
		return e
	}
	r, e := http.NewRequestWithContext(ctx, method, c.config.URL+path, bytes.NewReader(body))
	if e != nil {
		return e
	}
	r.Header.Set("X-World-Authorization", base64.StdEncoding.EncodeToString(header))
	r.Header.Set("Content-Type", "application/json")
	response, e := c.http.Do(r)
	if e != nil {
		return e
	}
	defer response.Body.Close()
	b, e := io.ReadAll(io.LimitReader(response.Body, (1<<20)+1))
	if e != nil {
		return e
	}
	if len(b) > 1<<20 {
		return errors.New("node response exceeds 1 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &HTTPStatusError{StatusCode: response.StatusCode, message: fmt.Sprintf("node HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(b)))}
	}
	if out != nil {
		d := json.NewDecoder(bytes.NewReader(b))
		if e = d.Decode(out); e != nil {
			return e
		}
		if e = d.Decode(new(any)); !errors.Is(e, io.EOF) {
			return errors.New("multiple response values")
		}
	}
	return nil
}
func (c *Client) NextEpoch(ctx context.Context) (uint64, error) {
	var v struct {
		Epoch uint64 `json:"epoch"`
	}
	e := c.Do(ctx, "publish", http.MethodPost, "/v1/publishers/epoch", []byte("{}"), &v)
	if e == nil && v.Epoch == 0 {
		e = errors.New("node returned zero publisher epoch")
	}
	return v.Epoch, e
}
func (c *Client) Publish(ctx context.Context, component world.Component) error {
	prepared, err := c.PreparePublication(component)
	if err != nil {
		return err
	}
	return c.PublishPrepared(ctx, prepared)
}
func (c *Client) Query(ctx context.Context) (Snapshot, error) {
	var s Snapshot
	e := c.Do(ctx, "read", http.MethodGet, "/v1/components", nil, &s)
	if e == nil {
		e = c.validateReplicaScope(s.ReplicaScope)
	}
	return s, e
}
