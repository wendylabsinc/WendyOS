package cloudlink

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/cli/browserauth"
	"github.com/wendylabsinc/wendy/go/internal/cli/grpcclient"
	"github.com/wendylabsinc/wendy/go/internal/shared/flock"
	cloudpbv2 "github.com/wendylabsinc/wendy/go/proto/gen/cloudpb/v2"
)

type Client struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	RedirectURIs []string `json:"redirect_uris"`
	SecretEnv    string   `json:"secret_env,omitempty"`
}
type Config struct {
	Services         browserauth.Settings `json:"services"`
	StateFile        string               `json:"state_file"`
	EncryptionKeyEnv string               `json:"encryption_key_env"`
	Clients          []Client             `json:"clients"`
	Scopes           []string             `json:"scopes"`
	AllowCamera      bool                 `json:"allow_camera,omitempty"`
	AllowAllApps     bool                 `json:"allow_all_apps,omitempty"`
}

func (c Config) Validate() error {
	if err := c.Services.Validate(); err != nil {
		return err
	}
	if !filepath.IsAbs(c.StateFile) || c.EncryptionKeyEnv == "" || len(c.Clients) == 0 || len(c.Clients) > 32 || len(c.Scopes) == 0 {
		return errors.New("Cloud link requires private state, an encryption key, registered clients, and scopes")
	}
	ids := map[string]bool{}
	for _, client := range c.Clients {
		if client.ID == "" || len(client.ID) > 256 || client.Name == "" || len(client.Name) > 128 || ids[client.ID] || len(client.RedirectURIs) == 0 || len(client.RedirectURIs) > 16 {
			return errors.New("Cloud link clients require unique IDs, names, and registered HTTPS callbacks")
		}
		ids[client.ID] = true
		for _, raw := range client.RedirectURIs {
			u, err := url.Parse(raw)
			if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
				return errors.New("Cloud link client callbacks must be HTTPS without credentials or fragments")
			}
		}
	}
	return nil
}

type cloudSession interface {
	Begin(context.Context, string, string) (string, error)
	Complete(context.Context, string, string, string) (browserauth.Profile, error)
	Restore(context.Context) (*browserauth.Profile, error)
	DiscoverFiltered(context.Context, func(context.Context, string) (net.Conn, error), bool) ([]*cloudpbv2.Asset, error)
	ConnectDevice(context.Context, string, func(context.Context, string) (net.Conn, error), *http.Client) (*grpcclient.AgentConnection, error)
}
type sessionSlot struct {
	mu      sync.Mutex
	session cloudSession
}
type pending struct {
	client, redirect, state, challenge, csrf string
	scopes                                   []string
	expires                                  time.Time
	upstream                                 cloudSession
	store                                    *memoryStore
	upstreamState, issuer                    string
}
type authorizationCode struct {
	account, client, redirect, challenge string
	expires                              time.Time
}

type Manager struct {
	mu               sync.Mutex
	config           Config
	resource, origin string
	clients          map[string]Client
	clientSecrets    map[string]string
	store            *encryptedStore
	data             database
	pending          map[string]*pending
	codes            map[string]authorizationCode
	sessions         map[string]*sessionSlot
	http             *http.Client
	newSession       func(browserauth.CredentialStore) cloudSession
	dial             func(context.Context, string) (net.Conn, error)
	release          func()
	closed           bool
}

func New(config Config, resource string, getenv func(string) string) (*Manager, error) {
	// Own the settings and client registrations; caller mutation cannot change
	// redirect matching or the services trusted by already linked accounts.
	raw, _ := json.Marshal(config)
	_ = json.Unmarshal(raw, &config)
	if err := config.Validate(); err != nil {
		return nil, err
	}
	u, err := url.Parse(resource)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Path != "/mcp" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("Cloud link resource must be an HTTPS MCP URL")
	}
	aad, _ := json.Marshal(struct {
		Resource string
		Services browserauth.Settings
	}{resource, config.Services})
	dir, err := os.Lstat(filepath.Dir(config.StateFile))
	if err != nil || !dir.IsDir() || dir.Mode().Perm()&0077 != 0 {
		return nil, errors.New("Cloud link state_file requires an existing private directory")
	}
	release, err := flock.Acquire(config.StateFile+".lock", 0)
	if err != nil {
		return nil, errors.New("Cloud link state is locked by another gateway process")
	}
	store, data, err := openStore(config.StateFile, getenv(config.EncryptionKeyEnv), aad)
	if err != nil {
		release()
		return nil, err
	}
	m := &Manager{config: config, resource: resource, origin: "https://" + u.Host, store: store, data: data, clients: map[string]Client{}, clientSecrets: map[string]string{}, pending: map[string]*pending{}, codes: map[string]authorizationCode{}, sessions: map[string]*sessionSlot{}, http: &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	m.release = release
	for _, client := range config.Clients {
		m.clients[client.ID] = client
		if client.SecretEnv != "" {
			secret := getenv(client.SecretEnv)
			if len(secret) < 32 {
				release()
				return nil, errors.New("Cloud link confidential client secret must contain at least 32 bytes")
			}
			m.clientSecrets[client.ID] = secret
		}
	}
	m.newSession = func(store browserauth.CredentialStore) cloudSession {
		return &browserauth.Session{Settings: &m.config.Services, Store: store, Client: m.http}
	}
	m.dial = func(ctx context.Context, target string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(target)
		if err != nil {
			return nil, err
		}
		return (&tls.Dialer{Config: &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2"}}}).DialContext(ctx, "tcp", target)
	}
	return m, nil
}

// Close disables this manager before releasing exclusive database ownership.
// In-flight calls cannot write through a manager whose lock has been released.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.release()
}

type Principal struct {
	Subject string
	Scopes  []string
}

func (m *Manager) Authenticate(_ context.Context, bearer string) (Principal, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return Principal{}, errors.New("Cloud link service closed")
	}
	if len(bearer) < 32 || len(bearer) > 512 {
		return Principal{}, errors.New("invalid token")
	}
	token, ok := m.data.Access[tokenHash(bearer)]
	a, accountOK := m.data.Accounts[token.Account]
	if !ok || !accountOK || token.Expires <= time.Now().Unix() || a.Expires <= time.Now().Unix() || token.Client != a.Client {
		return Principal{}, errors.New("invalid token")
	}
	client, registered := m.clients[token.Client]
	if !registered || client.ID == "" {
		return Principal{}, errors.New("client no longer registered")
	}
	return Principal{token.Account, intersection(a.Scopes, m.config.Scopes)}, nil
}

func intersection(scopes, allowed []string) []string {
	result := []string{}
	for _, scope := range scopes {
		if slices.Contains(allowed, scope) && !slices.Contains(result, scope) {
			result = append(result, scope)
		}
	}
	return result
}

func (m *Manager) session(ctx context.Context, subject string) (cloudSession, error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, errors.New("Cloud link service closed")
	}
	a, ok := m.data.Accounts[subject]
	if !ok || a.Expires <= time.Now().Unix() {
		m.mu.Unlock()
		return nil, errors.New("Cloud connection expired or revoked")
	}
	slot := m.sessions[subject]
	if slot == nil {
		slot = &sessionSlot{}
		m.sessions[subject] = slot
	}
	m.mu.Unlock()
	slot.mu.Lock()
	defer slot.mu.Unlock()
	if slot.session == nil {
		s := m.newSession(accountStore{m, subject})
		profile, err := s.Restore(ctx)
		if err != nil {
			return nil, err
		}
		if profile == nil {
			return nil, errors.New("Cloud connection has no saved identity")
		}
		slot.session = s
	}
	return slot.session, nil
}

func (m *Manager) Discover(ctx context.Context, subject string, onlineOnly bool) ([]*cloudpbv2.Asset, error) {
	s, err := m.session(ctx, subject)
	if err != nil {
		return nil, err
	}
	return s.DiscoverFiltered(ctx, m.dial, onlineOnly)
}
func (m *Manager) Connect(ctx context.Context, subject, asset string) (*grpcclient.AgentConnection, error) {
	s, err := m.session(ctx, subject)
	if err != nil {
		return nil, err
	}
	return s.ConnectDevice(ctx, asset, m.dial, m.http)
}

func (m *Manager) commitLocked(next database) error {
	if m.closed {
		return errors.New("Cloud link service closed")
	}
	next.prune(time.Now())
	if len(next.Accounts) > 10000 || len(next.Access) > 20000 || len(next.Refresh) > 20000 {
		return errors.New("Cloud link connection limit reached")
	}
	if err := m.store.save(next); err != nil {
		return err
	}
	m.data = next
	for id := range m.sessions {
		if _, ok := next.Accounts[id]; !ok {
			delete(m.sessions, id)
		}
	}
	return nil
}
func (m *Manager) revokeAccount(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := clone(m.data)
	delete(next.Accounts, id)
	return m.commitLocked(next)
}
