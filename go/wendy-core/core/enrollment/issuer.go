package enrollment

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ResetIssuer resolves only the enrolled Cloud endpoint, never token URLs.
func ResetIssuer(cloud string) (string, error) {
	t, err := target(cloud)
	if err != nil {
		return "", err
	}
	host, port, _ := net.SplitHostPort(t)
	if port == "443" {
		return "https://" + host, nil
	}
	return "https://" + t, nil
}
func target(endpoint string) (string, error) {
	if strings.Contains(endpoint, "://") {
		u, err := url.Parse(endpoint)
		if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
			return "", fmt.Errorf("relay endpoint must be an HTTPS URL or host:port")
		}
		endpoint = u.Host
	}
	if strings.ContainsAny(endpoint, "/ ?#@\r\n\t") {
		return "", fmt.Errorf("invalid relay endpoint")
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		host = endpoint
		port = "443"
	}
	if host == "" || port == "" {
		return "", fmt.Errorf("empty relay endpoint")
	}
	return net.JoinHostPort(host, port), nil
}
