package commands

// The wendy-auth sign-in as a background session. It writes nothing to
// stdout, so `wendy mcp serve` — whose stdout is the MCP protocol stream — can
// run it for the auth_login tool. performOIDCLogin runs the same steps for a
// person at a terminal and prints each one.

import (
	"context"
	"sync"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

// oidcLoginSession is one wendy-auth browser sign-in: a loopback listener
// waiting for the authorization redirect, then the token exchange, operator
// certificate and the config save.
type oidcLoginSession struct {
	url       string
	expiresAt time.Time

	done chan struct{} // closed when the session ends, signed in or not

	mu  sync.Mutex
	err error
}

// URL is the sign-in link for a browser on this machine.
func (l *oidcLoginSession) URL() string { return l.url }

// ExpiresAt is when the session stops waiting for the browser.
func (l *oidcLoginSession) ExpiresAt() time.Time { return l.expiresAt }

// Done is closed when the session ends, signed in or not.
func (l *oidcLoginSession) Done() <-chan struct{} { return l.done }

// Err is why the session failed; nil while it runs and after success.
func (l *oidcLoginSession) Err() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.err
}

func (l *oidcLoginSession) finish(err error) {
	l.mu.Lock()
	l.err = err
	l.mu.Unlock()
	close(l.done)
}

// beginOIDCLogin starts a wendy-auth sign-in and returns as soon as the sign-in
// URL exists. Waiting for the browser (up to browserLoginTimeout), the token
// exchange, certificate issuance and the save run in the background until Done
// closes. Cancelling ctx ends the wait with ctx.Err(). saving, when set, may
// adjust the config the new session is saved into.
func beginOIDCLogin(ctx context.Context, opts oidcLoginOptions, saving func(cfg *config.Config, added config.AuthConfig)) (*oidcLoginSession, error) {
	opts.PrintClaims = false // printClaims writes to stdout
	session := &oidcLoginSession{done: make(chan struct{})}
	urls := make(chan string, 1)
	go func() {
		session.finish(runOIDCLogin(ctx, opts, oidcLoginHooks{
			authorize: func(authURL string) { urls <- authURL },
			saving:    saving,
		}))
	}()
	select {
	case session.url = <-urls:
	case <-session.done:
		select {
		case session.url = <-urls: // it ended just after its URL existed
		default:
			// It ended before a browser was needed: invalid options, a
			// refused realm, or the callback port in use.
			return nil, session.Err()
		}
	}
	session.expiresAt = time.Now().Add(browserLoginTimeout)
	return session, nil
}
