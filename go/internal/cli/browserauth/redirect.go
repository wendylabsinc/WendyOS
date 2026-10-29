package browserauth

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const RedirectLifetime = 10 * time.Minute

// MaxRedirectRecord bounds one stored transaction. MAX_BYTES in
// web-client/public/wendy-credentials.js must match it.
const MaxRedirectRecord = 8192

// PendingStore owns private PKCE transactions, separately from saved sessions.
// Put must persist before returning. Take must atomically delete before returning
// the bytes, even if decoding or callback validation will subsequently fail.
// Implementations must bound record count/size and clean up expired records.
// Neither records nor their contents may be returned to the embedding page.
type PendingStore interface {
	Put(handle string, record []byte) error
	Take(handle string) ([]byte, error)
	Delete(handle string) error
}

type RedirectStart struct {
	URL     string `json:"url"`
	Pending string `json:"pending"`
}

type RedirectCallback struct {
	Pending  string `json:"pending"`
	Redirect string `json:"redirect"`
	Code     string `json:"code"`
	State    string `json:"state"`
	Issuer   string `json:"issuer"`
	Error    string `json:"error,omitempty"`
}

// No signing key, token or discovered endpoint is persisted here. An empty
// Issuer means universal login; the callback must still name a trusted realm.
type redirectTransaction struct {
	Version                int       `json:"version"`
	Verifier               string    `json:"verifier"`
	State                  string    `json:"state"`
	Issuer                 string    `json:"issuer"`
	Redirect               string    `json:"redirect"`
	Started                time.Time `json:"started"`
	Settings               Settings  `json:"settings"`
	RequireHostedMCPPolicy bool      `json:"require_hosted_mcp_policy"`
}

// validRedirect accepts only the form the callback page reports back,
// location.origin + location.pathname, because CompleteRedirect compares the
// two exactly. A form the browser would rewrite fails here, before the login.
func validRedirect(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && len(raw) <= 2048 && canonicalHost(u.Scheme, u.Host) && raw == u.Scheme+"://"+u.Host+u.Path && u.User == nil && u.Path == "/auth/callback" && u.RawPath == "" && u.RawQuery == "" && !u.ForceQuery && u.Fragment == "" && u.Opaque == "" && (u.Scheme == "https" || u.Scheme == "http" && u.Host == "localhost:5173")
}

// canonicalHost rejects what location.origin would not reproduce: upper-case
// or non-ASCII names, IP literals in brackets, and empty, zero-padded or
// default ports.
func canonicalHost(scheme, host string) bool {
	name, port, hasPort := strings.Cut(host, ":")
	if name == "" || strings.ContainsFunc(name, func(c rune) bool {
		return !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_')
	}) {
		return false
	}
	if !hasPort {
		return true
	}
	n, err := strconv.Atoi(port)
	return err == nil && port[0] != '0' && n <= 65535 && !(scheme == "https" && n == 443)
}

func validPendingSecret(raw string) bool {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	defer clear(b)
	return err == nil && len(b) == 32 && base64.RawURLEncoding.EncodeToString(b) == raw
}

// BeginRedirect returns an authorize URL only after durable storage succeeds.
// It leaves any current session or legacy Begin attempt untouched.
func (s *Session) BeginRedirect(ctx context.Context, email, issuer, redirect string) (RedirectStart, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Pending == nil {
		return RedirectStart{}, errors.New("Browser pending sign-in storage is unavailable")
	}
	_, authorize, pending, err := s.prepareLogin(ctx, email, issuer, redirect)
	if err != nil {
		return RedirectStart{}, err
	}
	record, err := json.Marshal(pending)
	if err != nil {
		return RedirectStart{}, err
	}
	defer clear(record)
	if len(record) > MaxRedirectRecord {
		return RedirectStart{}, errors.New("Pending sign-in is too large")
	}
	handle := random()
	if err := s.Pending.Put(handle, record); err != nil {
		return RedirectStart{}, fmt.Errorf("Could not save pending sign-in: %w", err)
	}
	return RedirectStart{URL: authorize, Pending: handle}, nil
}

// CompleteRedirect consumes the transaction exactly once, including invalid and
// error callbacks. All local bindings are checked before discovery or exchange.
func (s *Session) CompleteRedirect(ctx context.Context, callback RedirectCallback) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Pending == nil {
		return Profile{}, errors.New("Browser pending sign-in storage is unavailable")
	}
	if !validPendingSecret(callback.Pending) {
		return Profile{}, errors.New("Invalid pending sign-in; start sign-in again")
	}
	raw, err := s.Pending.Take(callback.Pending)
	if err != nil {
		return Profile{}, fmt.Errorf("Could not resume pending sign-in: %w", err)
	}
	defer clear(raw)
	if len(raw) == 0 {
		return Profile{}, errors.New("Pending sign-in is missing or already used; start sign-in again")
	}
	var pending redirectTransaction
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if len(raw) > MaxRedirectRecord || decoder.Decode(&pending) != nil || decoder.Decode(new(any)) != io.EOF || pending.Version != 1 || !validPendingSecret(pending.Verifier) || !validPendingSecret(pending.State) {
		return Profile{}, errors.New("Invalid pending sign-in record; start sign-in again")
	}
	if err := s.settings().Validate(); err != nil {
		return Profile{}, err
	}
	if pending.Settings != s.settings() || pending.RequireHostedMCPPolicy != s.RequireHostedMCPPolicy {
		return Profile{}, errors.New("Sign-in settings changed; start sign-in again")
	}
	now := time.Now()
	if pending.Started.IsZero() || pending.Started.After(now) || !now.Before(pending.Started.Add(RedirectLifetime)) {
		return Profile{}, errors.New("Pending sign-in expired or has an invalid start time; start sign-in again")
	}
	if !validRedirect(pending.Redirect) || callback.Redirect != pending.Redirect {
		return Profile{}, errors.New("Sign-in callback changed; start sign-in again")
	}
	if subtle.ConstantTimeCompare([]byte(callback.State), []byte(pending.State)) != 1 {
		return Profile{}, errors.New("Login state mismatch; start sign-in again")
	}
	// OAuth error responses may omit iss. If supplied, it must still match;
	// successful code responses always require the expected trusted realm.
	if (callback.Error == "" || callback.Issuer != "") && (!s.orgIssuer(callback.Issuer) || pending.Issuer != "" && pending.Issuer != callback.Issuer) {
		return Profile{}, errors.New("Authentication issuer changed; start sign-in again")
	}
	if callback.Error != "" {
		// Retain the protocol cause without reflecting arbitrary callback text,
		// descriptions, credentials or control characters through the bridge.
		if !validOAuthError(callback.Error) {
			return Profile{}, errors.New("Invalid OAuth error code; start sign-in again")
		}
		return Profile{}, fmt.Errorf("Authentication failed: %s", callback.Error)
	}
	if callback.Code == "" {
		return Profile{}, errors.New("No authorization code received")
	}
	// Complete on a temporary session: a failed attempt must not replace an
	// existing sign-in or leave private PKCE/key material on the live Session.
	attempt := &Session{Settings: s.Settings, Client: s.Client, Store: s.Store, RequireHostedMCPPolicy: s.RequireHostedMCPPolicy,
		verifier: pending.Verifier, redirect: pending.Redirect}
	if err := attempt.generateKey(); err != nil {
		return Profile{}, err
	}
	profile, err := attempt.completeLogin(ctx, callback.Code, callback.Issuer)
	if err != nil {
		return Profile{}, err
	}
	s.meta, s.key, s.privatePEM = attempt.meta, attempt.key, attempt.privatePEM
	s.tokens, s.profile, s.certificate = attempt.tokens, attempt.profile, attempt.certificate
	s.liteCertificate = attempt.liteCertificate
	s.state, s.verifier, s.redirect, s.started = "", "", "", time.Time{}
	return profile, nil
}

// CancelRedirect removes only this attempt, including when it has expired.
func (s *Session) CancelRedirect(handle string) error {
	if s.Pending == nil {
		return errors.New("Browser pending sign-in storage is unavailable")
	}
	if !validPendingSecret(handle) {
		return errors.New("Invalid pending sign-in")
	}
	return s.Pending.Delete(handle)
}

func validOAuthError(code string) bool {
	if len(code) == 0 || len(code) > 128 {
		return false
	}
	for _, c := range code {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}
