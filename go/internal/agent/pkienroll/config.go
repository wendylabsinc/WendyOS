package pkienroll

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// pki-core's CSR frontend hosts, per environment. Both are public edge
// endpoints; the fabric relay that mints the enrollment token is private and is
// not reachable from a device, which is why the token arrives staged rather
// than fetched.
const (
	ProdCSRFrontendHost = "csr.pki.wendy.sh"
	DevCSRFrontendHost  = "csr.dev.pki.wendy.sh"
)

// Environment names accepted by CSRFrontendURL. Anything else is treated as
// production, because guessing "dev" from an unrecognised value is the failure
// that sends a production device to a dev certificate authority.
const (
	EnvProd = "prod"
	EnvDev  = "dev"
)

// Environment variables. The endpoint override is named after
// WENDY_PKI_RENEW_ENDPOINT on origin/sem/wdy-2899-acme-enrollment, which
// established both the naming and the rule that follows.
//
// There is deliberately NO derivation from the enrolled cloud host. Guessing a
// pki host and port from another service's address is what once sent enrollment
// tokens to the wrong place in cleartext (WDY-2799).
const (
	// CSREndpointEnv fully overrides the frontend URL, e.g.
	// "https://csr.dev.pki.wendy.sh". A value already carrying a /v1/ path is
	// used verbatim.
	CSREndpointEnv = "WENDY_PKI_CSR_ENDPOINT"

	// EnvironmentEnv selects the derived host, "dev" or "prod".
	EnvironmentEnv = "WENDY_PKI_ENV"
)

// ErrPlaintextEndpoint reports a configured frontend URL that would carry the
// enrollment token, a single-use bearer credential, over cleartext HyperText
// Transfer Protocol (HTTP) to somewhere other than this machine.
//
// It is refused at configuration time rather than at request time so that the
// enrolment never starts: by the time a request is on the wire the token has
// already left the device, and a token seen in cleartext must be treated as
// spent and revoked. Loopback is exempt because a local pki-core, a port
// forward and the package's own tests all speak plain HTTP to 127.0.0.1, where
// the packet never reaches a network interface.
var ErrPlaintextEndpoint = fmt.Errorf("pki enrollment: refusing a plaintext http endpoint")

// CSRFrontendURL resolves the frontend base URL.
//
// Precedence, most specific first: an explicit override (the staged credential
// file's csrEndpoint, then CSREndpointEnv), then the derived host for the named
// environment. The override wins so that a local pki-core, a staging tenant or
// a port-forwarded frontend needs no code change.
//
// A scheme-less value is given https. An explicit http:// value is accepted
// only for a loopback host; anything else returns ErrPlaintextEndpoint, so a
// misconfiguration cannot send the bearer enrolment token in cleartext.
func CSRFrontendURL(override, environment string) (string, error) {
	if v := strings.TrimSpace(override); v != "" {
		return checkedScheme(v)
	}
	if v := strings.TrimSpace(os.Getenv(CSREndpointEnv)); v != "" {
		return checkedScheme(v)
	}
	return checkedScheme(csrFrontendHost(environment))
}

// csrFrontendHost maps an environment name to a host. An empty environment
// falls back to EnvironmentEnv, and an unrecognised one to production.
func csrFrontendHost(environment string) string {
	environment = strings.ToLower(strings.TrimSpace(environment))
	if environment == "" {
		environment = strings.ToLower(strings.TrimSpace(os.Getenv(EnvironmentEnv)))
	}
	if environment == EnvDev {
		return DevCSRFrontendHost
	}
	return ProdCSRFrontendHost
}

// checkedScheme applies the default scheme and then the plaintext rule.
func checkedScheme(v string) (string, error) {
	withDefault := withScheme(v)
	if err := checkTransport(withDefault); err != nil {
		return "", err
	}
	return withDefault, nil
}

func withScheme(v string) string {
	if strings.Contains(v, "://") {
		return v
	}
	return "https://" + v
}

// checkTransport refuses a cleartext endpoint that is not on this machine. It
// is deliberately permissive about everything else: an unparseable URL and an
// unknown scheme are frontendEndpoint's to report, with its own message.
func checkTransport(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("parsing csr frontend url %q: %w", endpoint, err)
	}
	if !strings.EqualFold(u.Scheme, "http") {
		return nil
	}
	if isLoopbackHost(u.Hostname()) {
		return nil
	}
	return fmt.Errorf("%w: %q would send the single-use enrollment token in cleartext; "+
		"use https, or a loopback host (127.0.0.0/8, ::1, localhost) for a local frontend",
		ErrPlaintextEndpoint, endpoint)
}

// isLoopbackHost reports whether host names this machine. "localhost" is
// accepted by name because a local frontend is normally reached that way and
// resolving it here would make the check depend on the resolver.
func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") || strings.HasSuffix(strings.ToLower(host), ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// StagedFileName is the credential file, relative to the agent's config
// directory, that hands the agent an enrollment token once. It is defined here
// rather than in the services package because the wendy CLI writes it and the
// CLI is cross-compiled for Windows, where the services package (which pulls in
// the audio and data managers) does not build.
const StagedFileName = "pki-enrollment.json"
