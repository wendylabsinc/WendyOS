package mcp

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// fakeLoginSession is a LoginSession the test finishes by hand.
type fakeLoginSession struct {
	url     string
	expires time.Time
	done    chan struct{}
	mu      sync.Mutex
	err     error
}

func newFakeLoginSession(url string) *fakeLoginSession {
	return &fakeLoginSession{url: url, expires: time.Now().Add(5 * time.Minute), done: make(chan struct{})}
}

func (f *fakeLoginSession) URL() string           { return f.url }
func (f *fakeLoginSession) ExpiresAt() time.Time  { return f.expires }
func (f *fakeLoginSession) Done() <-chan struct{} { return f.done }
func (f *fakeLoginSession) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

func (f *fakeLoginSession) finish(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
	close(f.done)
}

// fakeStarter hands out numbered sessions and records each call's context.
type fakeStarter struct {
	mu       sync.Mutex
	calls    int
	ctxs     []context.Context
	sessions []*fakeLoginSession
}

func (f *fakeStarter) start(ctx context.Context) (LoginSession, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.ctxs = append(f.ctxs, ctx)
	session := newFakeLoginSession(fmt.Sprintf("https://cloud.example.invalid/cli-auth?n=%d", f.calls))
	f.sessions = append(f.sessions, session)
	return session, nil
}

func (f *fakeStarter) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// stubLoginBrowser fixes whether this is a desktop session and records the
// URLs auth_login opens.
func stubLoginBrowser(t *testing.T, desktop bool) func() []string {
	t.Helper()
	var mu sync.Mutex
	var opened []string
	prevOpen, prevDesktop := openLoginURL, desktopSession
	openLoginURL = func(u string) error {
		mu.Lock()
		defer mu.Unlock()
		opened = append(opened, u)
		return nil
	}
	desktopSession = func() bool { return desktop }
	t.Cleanup(func() { openLoginURL, desktopSession = prevOpen, prevDesktop })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), opened...)
	}
}

func callAuthLogin(t *testing.T, s *mcpServer, ctx context.Context) map[string]any {
	t.Helper()
	r, err := s.handleAuthLogin(ctx, callToolReq("auth_login", nil))
	if err != nil {
		t.Fatalf("handleAuthLogin: %v", err)
	}
	return structuredMap(t, r)
}

func TestAuthLogin_ReturnsPendingURLAndOpensBrowserOnDesktop(t *testing.T) {
	opened := stubLoginBrowser(t, true)
	starter := &fakeStarter{}
	s := New(&config.Config{}, nil)
	s.SetLoginStarter(starter.start)

	out := callAuthLogin(t, s, context.Background())
	if out["status"] != "pending" || out["url"] != "https://cloud.example.invalid/cli-auth?n=1" {
		t.Fatalf("result = %v", out)
	}
	if out["browser_opened"] != true {
		t.Fatalf("browser_opened = %v, want true on a desktop session", out["browser_opened"])
	}
	if _, err := time.Parse(time.RFC3339, fmt.Sprint(out["expires_at"])); err != nil {
		t.Fatalf("expires_at = %v: %v", out["expires_at"], err)
	}
	if next := fmt.Sprint(out["next_step"]); !strings.Contains(next, "this machine") {
		t.Fatalf("next_step = %q, want the same-machine requirement", next)
	}
	if got := opened(); !slices.Equal(got, []string{"https://cloud.example.invalid/cli-auth?n=1"}) {
		t.Fatalf("opened = %v", got)
	}
}

// Review Focus 5 (handler half): no display, no browser — the link is the way in.
func TestAuthLogin_HeadlessDoesNotOpenBrowser(t *testing.T) {
	opened := stubLoginBrowser(t, false)
	s := New(&config.Config{}, nil)
	s.SetLoginStarter((&fakeStarter{}).start)

	out := callAuthLogin(t, s, context.Background())
	if out["browser_opened"] != false || out["url"] == "" {
		t.Fatalf("result = %v", out)
	}
	if got := opened(); len(got) != 0 {
		t.Fatalf("opened a browser on a headless session: %v", got)
	}
	next := fmt.Sprint(out["next_step"])
	if !strings.Contains(next, "machine running Wendy") || !strings.Contains(next, "ssh -L") {
		t.Fatalf("next_step = %q, want the same-machine requirement and the port forward", next)
	}
}

func TestAuthLogin_RepeatWhilePendingReturnsSameURL(t *testing.T) {
	opened := stubLoginBrowser(t, true)
	starter := &fakeStarter{}
	s := New(&config.Config{}, nil)
	s.SetLoginStarter(starter.start)

	first := callAuthLogin(t, s, context.Background())
	second := callAuthLogin(t, s, context.Background())
	if first["url"] != second["url"] || starter.callCount() != 1 {
		t.Fatalf("repeat call started a new session: %v vs %v (starts=%d)", first["url"], second["url"], starter.callCount())
	}
	if second["browser_opened"] != false || len(opened()) != 1 {
		t.Fatalf("repeat call reopened the browser: %v, opened=%v", second["browser_opened"], opened())
	}
}

// Review Focus 1: a client retrying a slow first call must not start two sign-ins.
func TestAuthLogin_ConcurrentCallsShareOneSession(t *testing.T) {
	stubLoginBrowser(t, false)
	starter := &fakeStarter{}
	s := New(&config.Config{}, nil)
	s.SetLoginStarter(starter.start)

	var wg sync.WaitGroup
	urls := make([]any, 8)
	for i := range urls {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Off the test goroutine: report with t.Error, never t.Fatal.
			r, err := s.handleAuthLogin(context.Background(), callToolReq("auth_login", nil))
			if err != nil {
				t.Errorf("handleAuthLogin: %v", err)
				return
			}
			urls[i] = r.StructuredContent.(map[string]any)["url"]
		}(i)
	}
	wg.Wait()
	if starter.callCount() != 1 {
		t.Fatalf("started %d sessions, want 1", starter.callCount())
	}
	for _, u := range urls {
		if u != urls[0] {
			t.Fatalf("calls got different URLs: %v", urls)
		}
	}
}

// Review Focus 2 (handler half): an abandoned sign-in doesn't block the next one.
func TestAuthLogin_NewSessionAfterPreviousEnded(t *testing.T) {
	stubLoginBrowser(t, false)
	starter := &fakeStarter{}
	s := New(&config.Config{}, nil)
	s.SetLoginStarter(starter.start)

	first := callAuthLogin(t, s, context.Background())
	starter.sessions[0].finish(errors.New("timed out after 5m0s: no browser finished the sign-in"))
	second := callAuthLogin(t, s, context.Background())
	if starter.callCount() != 2 || first["url"] == second["url"] {
		t.Fatalf("want a fresh session after the first ended; starts=%d urls=%v,%v", starter.callCount(), first["url"], second["url"])
	}
}

// Review Focus 3: the browser round trip finishes long after the tool call
// returns, so the session must not share the request's context.
func TestAuthLogin_SessionOutlivesRequestContext(t *testing.T) {
	stubLoginBrowser(t, false)
	starter := &fakeStarter{}
	s := New(&config.Config{}, nil)
	s.SetLoginStarter(starter.start)

	reqCtx, cancel := context.WithCancel(context.Background())
	callAuthLogin(t, s, reqCtx)
	cancel()
	if err := starter.ctxs[0].Err(); err != nil {
		t.Fatalf("the session context ended with the request: %v", err)
	}
}

func TestAuthLogin_NoStarterIsUnsupported(t *testing.T) {
	stubLoginBrowser(t, false)
	s := New(&config.Config{}, nil)
	out := callAuthLogin(t, s, context.Background())
	if out["error_code"] != string(errCodeUnsupported) {
		t.Fatalf("result = %v, want UNSUPPORTED", out)
	}
}

func TestAuthLogin_StarterErrorIsInternal(t *testing.T) {
	stubLoginBrowser(t, false)
	s := New(&config.Config{}, nil)
	s.SetLoginStarter(func(context.Context) (LoginSession, error) {
		return nil, errors.New("starting local callback server: address in use")
	})
	out := callAuthLogin(t, s, context.Background())
	if out["error_code"] != string(errCodeInternal) {
		t.Fatalf("result = %v, want INTERNAL", out)
	}
}

// Review Focus 5: which platforms count as a desktop a browser can reach.
func TestDesktopSessionFor(t *testing.T) {
	env := func(vars map[string]string) func(string) string {
		return func(k string) string { return vars[k] }
	}
	cases := []struct {
		name string
		goos string
		vars map[string]string
		want bool
	}{
		{"macOS", "darwin", nil, true},
		{"Windows", "windows", nil, true},
		{"Linux X11", "linux", map[string]string{"DISPLAY": ":0"}, true},
		{"Linux Wayland", "linux", map[string]string{"WAYLAND_DISPLAY": "wayland-0"}, true},
		{"Linux over SSH", "linux", map[string]string{"SSH_CONNECTION": "1.2.3.4 5 6.7.8.9 22"}, false},
		{"FreeBSD", "freebsd", map[string]string{"DISPLAY": ":0"}, false},
	}
	for _, c := range cases {
		if got := desktopSessionFor(c.goos, env(c.vars)); got != c.want {
			t.Errorf("%s: desktopSessionFor = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestAuthLogin_AdvertisedInCoreWithAnnotations(t *testing.T) {
	s := New(&config.Config{}, nil)
	srv, err := s.newProtocolServer()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range protocolTools(t, srv) {
		names = append(names, tool.Name)
	}
	if !slices.Contains(names, "auth_login") {
		t.Fatalf("auth_login is not in the default tool list: %v", names)
	}
	a := srv.GetTool("auth_login").Tool.Annotations
	if *a.ReadOnlyHint || *a.DestructiveHint || !*a.OpenWorldHint {
		t.Fatalf("annotations = readOnly %v, destructive %v, openWorld %v; want false, false, true",
			*a.ReadOnlyHint, *a.DestructiveHint, *a.OpenWorldHint)
	}
}
