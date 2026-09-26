package cloudrelay

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// DevelopmentBrowserBroker is the dev broker's Cloud Run URL, derived from
// cloud/infra/Pulumi.dev.yaml's tunnelBrokerServiceName and runUrlSuffix.
const DevelopmentBrowserBroker = "wendy-cloud-dev-tunnel-broker-nkohwk7hda-uc.a.run.app"

// BrowserBrokerTarget normalizes the HTTPS URL or gRPC authority supplied by
// Cloud. Both the WASM worker and local relay enforce this same destination
// policy. Cloud Run is multi-tenant, so only Wendy's exact service is allowed.
func BrowserBrokerTarget(endpoint string) (string, error) {
	original := endpoint
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("Cloud returned an invalid broker endpoint")
	}
	host := strings.ToLower(u.Hostname())
	if u.Scheme != "https" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" || (u.Port() != "" && u.Port() != "443") || strings.HasSuffix(u.Host, ":") || !browserBrokerHostAllowed(host) {
		return "", fmt.Errorf("Cloud broker endpoint %q is not an allowed Wendy HTTPS broker", original)
	}
	return net.JoinHostPort(host, "443"), nil
}

func browserBrokerHostAllowed(host string) bool {
	// Other Wendy services must never receive broker authorization traffic.
	switch host {
	case "relay.dev.wendy.sh", "relay.wendy.sh", "eu.relay.wendy.sh", DevelopmentBrowserBroker:
		return true
	default:
		return false
	}
}
