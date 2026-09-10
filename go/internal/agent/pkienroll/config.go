package pkienroll

import (
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

// CSRFrontendURL resolves the frontend base URL.
//
// Precedence, most specific first: an explicit override (the staged credential
// file's csrEndpoint, then CSREndpointEnv), then the derived host for the named
// environment. The override wins so that a local pki-core, a staging tenant or
// a port-forwarded frontend needs no code change.
func CSRFrontendURL(override, environment string) string {
	if v := strings.TrimSpace(override); v != "" {
		return withScheme(v)
	}
	if v := strings.TrimSpace(os.Getenv(CSREndpointEnv)); v != "" {
		return withScheme(v)
	}
	return withScheme(csrFrontendHost(environment))
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

func withScheme(v string) string {
	if strings.Contains(v, "://") {
		return v
	}
	return "https://" + v
}
