package config

import (
	"errors"
	"fmt"
)

// ErrNotLoggedIn is returned when no auth sessions are stored.
var ErrNotLoggedIn = errors.New("not logged in; run 'wendy auth login' first")

// ErrMultipleSessions wraps the resolver error raised when several sessions
// exist, no --cloud-grpc flag was given, no valid default is set, and no
// interactive picker is available. Callers may match it with errors.Is to
// substitute a surface-specific message (e.g. the MCP tool's cloud_grpc wording).
var ErrMultipleSessions = errors.New("multiple auth sessions exist")

// SessionPicker selects one session interactively. It is injected by callers
// that can show a TUI; non-interactive callers (MCP, non-TTY) pass nil.
type SessionPicker func(cfg *Config) (*AuthConfig, error)

// DefaultAuth resolves DefaultCloudGRPC to a stored session. ok is false when
// no default is set or the named session no longer exists (stale default).
func (c *Config) DefaultAuth() (*AuthConfig, bool) {
	if c == nil || c.DefaultCloudGRPC == "" {
		return nil, false
	}
	var preferred *AuthConfig
	selectedOrg := 0
	for i := range c.Auth {
		if c.Auth[i].CloudGRPC == c.DefaultCloudGRPC {
			if preferred == nil {
				selectedOrg = authEntryOrgID(c.Auth[i])
			}
			if authEntryOrgID(c.Auth[i]) != selectedOrg {
				continue
			}
			preferred = preferAuth(preferred, &c.Auth[i])
		}
	}
	return preferred, preferred != nil
}

// preferAuth resolves legacy/operator duplicates for the same selection. The
// Cloud request-signing contract makes an operator session strictly more
// capable: it has a refreshable bearer token and can sign privileged writes,
// while the legacy session cannot satisfy the operator-signature gate.
func preferAuth(current, candidate *AuthConfig) *AuthConfig {
	if current == nil || (current.OAuthIssuer == "" && candidate.OAuthIssuer != "") {
		return candidate
	}
	return current
}

// ResolveAuth chooses the auth session to use. Precedence:
//  1. cloudGRPC flag set      -> endpoint match; when several orgs share that
//     endpoint, prefer the DefaultOrgID session, then the first (error if none)
//  2. exactly one session     -> use it
//  3. DefaultOrgID set        -> session whose cert org matches (if unique)
//  4. valid persisted default -> use it (DefaultCloudGRPC)
//  5. pick != nil             -> interactive picker
//  6. otherwise               -> ErrMultipleSessions
//
// The returned session is guaranteed to hold certificate material or an API token.
func ResolveAuth(cfg *Config, cloudGRPC string, pick SessionPicker) (*AuthConfig, error) {
	if cfg == nil || len(cfg.Auth) == 0 {
		return nil, ErrNotLoggedIn
	}
	if cloudGRPC != "" {
		// Several orgs can share one endpoint (multiple orgs on the production
		// cloud). The flag alone cannot name an org, so among the endpoint's
		// sessions honor the persisted default org before falling back to the
		// first — otherwise the flag silently pins the oldest login's org.
		var matches []*AuthConfig
		for i := range cfg.Auth {
			if cfg.Auth[i].CloudGRPC == cloudGRPC {
				matches = append(matches, &cfg.Auth[i])
			}
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("no auth session for %s; run 'wendy auth login --cloud-grpc %s' first", cloudGRPC, cloudGRPC)
		}
		if cfg.DefaultOrgID != 0 {
			var preferred *AuthConfig
			for _, m := range matches {
				if len(m.Certificates) > 0 && int32(m.Certificates[0].OrganizationID) == cfg.DefaultOrgID {
					preferred = preferAuth(preferred, m)
				}
			}
			if preferred != nil {
				return authWithCerts(preferred)
			}
		}
		var preferred *AuthConfig
		selectedOrg := authEntryOrgID(*matches[0])
		for _, match := range matches {
			if authEntryOrgID(*match) != selectedOrg {
				continue
			}
			preferred = preferAuth(preferred, match)
		}
		return authWithCerts(preferred)
	}
	if len(cfg.Auth) == 1 {
		return authWithCerts(&cfg.Auth[0])
	}
	if cfg.DefaultOrgID != 0 {
		var preferred *AuthConfig
		for i := range cfg.Auth {
			a := &cfg.Auth[i]
			if len(a.Certificates) > 0 && int32(a.Certificates[0].OrganizationID) == cfg.DefaultOrgID {
				preferred = preferAuth(preferred, a)
			}
		}
		if preferred != nil {
			return authWithCerts(preferred)
		}
		// DefaultOrgID set but no matching session; fall through so the user
		// can still operate (e.g. the session was removed).
	}
	if def, ok := cfg.DefaultAuth(); ok {
		return authWithCerts(def)
	}
	if pick != nil {
		picked, err := pick(cfg)
		if err != nil {
			return nil, err
		}
		return authWithCerts(picked)
	}
	return nil, fmt.Errorf("%w; pass --cloud-grpc or run 'wendy auth use' to choose a default", ErrMultipleSessions)
}

// authWithCerts rejects sessions with no certificate material.
func authWithCerts(a *AuthConfig) (*AuthConfig, error) {
	if len(a.Certificates) == 0 && !a.HasAPIKey() {
		return nil, fmt.Errorf("auth session %s has no certificates or API token; re-run 'wendy auth login'", a.CloudGRPC)
	}
	return a, nil
}
