package mcp

import (
	"context"
	"os"
	"runtime"
	"time"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/wendylabsinc/wendy/go/internal/shared/browseropen"
	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// LoginSession is one browser sign-in to Wendy Cloud, started by auth_login.
// The commands package implements it; this package cannot import commands.
type LoginSession interface {
	// URL is the sign-in link to show the user.
	URL() string
	// ExpiresAt is when the session stops waiting for the browser.
	ExpiresAt() time.Time
	// Done is closed when the session ends, signed in or not.
	Done() <-chan struct{}
	// Err is why the session failed; nil while it runs and after success.
	Err() error
}

// LoginStarter begins a sign-in and returns once its URL exists. The session
// must keep running after the call returns and must not write to stdout, which
// carries the MCP protocol.
type LoginStarter func(ctx context.Context) (LoginSession, error)

// SetLoginStarter enables auth_login. Call it before Start.
func (s *mcpServer) SetLoginStarter(fn LoginStarter) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	s.loginStarter = fn
}

// openLoginURL opens the sign-in link in the default browser. A var for tests.
var openLoginURL = browseropen.Open

// desktopSession reports whether a browser window opened here would reach a
// person. A var for tests.
var desktopSession = func() bool { return desktopSessionFor(runtime.GOOS, os.Getenv) }

// desktopSessionFor is desktopSession for a given platform and environment.
// macOS and Windows always have a desktop. Linux has one only with a display
// server; over SSH or in a container xdg-open fails or opens nowhere.
func desktopSessionFor(goos string, getenv func(string) string) bool {
	switch goos {
	case "darwin", "windows":
		return true
	case "linux":
		return getenv("DISPLAY") != "" || getenv("WAYLAND_DISPLAY") != ""
	default:
		return false
	}
}

func (s *mcpServer) registerAuthTools(srv *server.MCPServer) {
	opts := []mcpgo.ToolOption{
		mcpgo.WithDescription("Sign in to Wendy Cloud; devices need a signed-in session. Returns a link to show the user (opened in their browser when possible) and finishes in the background; wendy_status then reports auth: logged_in."),
	}
	opts = append(opts, mutating()...)
	opts = append(opts, openWorld()...)
	srv.AddTool(mcpgo.NewTool("auth_login", opts...), s.handleAuthLogin)
}

// handleAuthLogin starts a sign-in, or returns the one already waiting for the
// browser. loginMu is held throughout so concurrent calls share one session;
// starting only opens a local listener, so the hold is brief.
func (s *mcpServer) handleAuthLogin(ctx context.Context, _ mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error) {
	s.loginMu.Lock()
	defer s.loginMu.Unlock()
	if s.login != nil && !loginFinished(s.login) {
		return okResult(authLoginResult(s.login, false)), nil
	}
	if s.loginStarter == nil {
		return errResult(errCodeUnsupported, "sign-in is not available from this server; run `wendy auth login` in a terminal"), nil
	}
	// The browser round trip finishes long after this call returns, so the
	// session must not end with the request's context.
	session, err := s.loginStarter(context.WithoutCancel(ctx))
	if err != nil {
		return errResultf(errCodeInternal, "starting sign-in: %s", err.Error()), nil
	}
	s.login = session
	opened := false
	if desktopSession() {
		opened = openLoginURL(session.URL()) == nil
	}
	return okResult(authLoginResult(session, opened)), nil
}

func authLoginResult(session LoginSession, browserOpened bool) map[string]any {
	next := "Show the user this link and ask them to sign in before expires_at. It must be opened in a browser on the machine running Wendy; from another machine they must first forward the port in the link's redirect_uri (e.g. ssh -L PORT:127.0.0.1:PORT). Then call wendy_status: auth becomes logged_in."
	if browserOpened {
		next = "A browser window opened for sign-in; share the link too in case it went unseen (it works only in a browser on this machine). Then call wendy_status: auth becomes logged_in."
	}
	return map[string]any{
		"status":         "pending",
		"url":            session.URL(),
		"expires_at":     session.ExpiresAt().UTC().Format(time.RFC3339),
		"browser_opened": browserOpened,
		"next_step":      next,
	}
}

// loginFinished reports whether session has ended.
func loginFinished(session LoginSession) bool {
	select {
	case <-session.Done():
		return true
	default:
		return false
	}
}

// authState summarizes sign-in for wendy_status: "pending" while an auth_login
// session waits for the browser; otherwise "logged_in" when any session's
// certificate is still valid, "expired" when sessions exist but none is, and
// "logged_out" with none. It reads config.json as it is now, so a `wendy auth
// login` in a terminal counts too. loginErr is why the latest auth_login
// failed, reported only while no session is valid.
func (s *mcpServer) authState(now time.Time) (state string, loginErr error) {
	s.loginMu.Lock()
	session := s.login
	s.loginMu.Unlock()
	if session != nil {
		if !loginFinished(session) {
			return "pending", nil
		}
		loginErr = session.Err()
	}
	cfg := s.currentConfig()
	if cfg == nil || len(cfg.Auth) == 0 {
		return "logged_out", loginErr
	}
	for _, auth := range cfg.Auth {
		// Like the mTLS ladder (loadAllCLICerts), only a session's first
		// certificate is used to authenticate.
		if len(auth.Certificates) > 0 && !authCertExpired(auth.Certificates[0], now) {
			return "logged_in", nil
		}
	}
	return "expired", loginErr
}

// authCertExpired reports whether cert's leaf is past its NotAfter. It mirrors
// commands.certExpired, which the mcp package cannot import: the same tolerant
// decoder, and an unparseable certificate counts as not expired.
func authCertExpired(cert config.CertificateInfo, now time.Time) bool {
	leaves, _ := certs.ParseCertsFromPEM([]byte(cert.PemCertificate))
	if len(leaves) == 0 {
		return false
	}
	return now.After(leaves[0].NotAfter)
}
