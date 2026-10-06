package mcp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const gatewayMCPEventBodyLimit = 256 * 1024

// No proxies, redirects, or reused connections: every connection is checked at
// dial time, and TLS still verifies the callback's original hostname.
func gatewayMCPWebhookClient() *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		DisableKeepAlives:     true,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 8 * time.Second,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, fmt.Errorf("invalid callback address")
			}
			addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil || len(addresses) == 0 {
				return nil, fmt.Errorf("callback DNS unavailable")
			}
			// Reject mixed public/private answers too. Never race an unvalidated
			// address through a second DNS lookup or a transport fallback.
			for _, ip := range addresses {
				if !gatewayMCPPublicIP(ip) {
					return nil, fmt.Errorf("callback address is not public")
				}
			}
			for _, ip := range addresses {
				conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if dialErr == nil {
					return conn, nil
				}
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
			}
			return nil, fmt.Errorf("callback connection unavailable")
		},
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

var gatewayMCPNonPublicPrefixes = func() []netip.Prefix {
	var out []netip.Prefix
	for _, prefix := range []string{
		"0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12",
		"192.0.0.0/24", "192.0.2.0/24", "192.88.99.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4",
		"::/96", "::ffff:0:0/96", "64:ff9b::/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20", "5f00::/16", "fc00::/7", "fe80::/10", "ff00::/8",
	} {
		out = append(out, netip.MustParsePrefix(prefix))
	}
	return out
}()

func gatewayMCPPublicIP(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return false
	}
	for _, prefix := range gatewayMCPNonPublicPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func gatewayMCPCallbackURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 2048 || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || strings.Contains(u.Hostname(), "%") {
		return "", fmt.Errorf("callback must be an HTTPS URL without credentials or fragment")
	}
	if ip, err := netip.ParseAddr(u.Hostname()); err == nil && !gatewayMCPPublicIP(ip) {
		return "", fmt.Errorf("callback address must be public")
	}
	if port := u.Port(); port != "" {
		value, err := strconv.Atoi(port)
		if err != nil || value < 1 || value > 65535 {
			return "", fmt.Errorf("invalid callback port")
		}
	}
	// A deterministic URL identity treats hostname case and default ports alike.
	u.Host = strings.ToLower(u.Host)
	if u.Port() == "443" {
		u.Host = u.Hostname()
		if strings.Contains(u.Host, ":") {
			u.Host = "[" + u.Host + "]"
		}
	}
	if u.Path == "" {
		u.Path = "/"
	}
	return u.String(), nil
}

func gatewayMCPWebhookKey(secret string) ([]byte, error) {
	if !strings.HasPrefix(secret, "whsec_") {
		return nil, fmt.Errorf("a whsec_ signing secret is required")
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, "whsec_"))
	if err != nil || len(key) < 24 || len(key) > 64 {
		return nil, fmt.Errorf("signing secret must contain 24–64 base64-encoded bytes")
	}
	return key, nil
}

func gatewayMCPSignature(secret, id, timestamp string, body []byte) (string, error) {
	key, err := gatewayMCPWebhookKey(secret)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(id + "." + timestamp + "."))
	_, _ = mac.Write(body)
	return "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

type gatewayMCPWebhookFailure struct{ reason string }

func (e *gatewayMCPWebhookFailure) Error() string { return "callback verification failed: " + e.reason }

func gatewayMCPPost(ctx context.Context, client *http.Client, callback, subscription, secret, oldSecret, id string, body []byte, now time.Time) (*http.Response, error) {
	if len(body) > gatewayMCPEventBodyLimit {
		return nil, fmt.Errorf("event payload exceeds 256 KiB")
	}
	if _, err := gatewayMCPCallbackURL(callback); err != nil {
		return nil, err
	}
	timestamp := strconv.FormatInt(now.Unix(), 10)
	signature, err := gatewayMCPSignature(secret, id, timestamp, body)
	if err != nil {
		return nil, err
	}
	if oldSecret != "" && oldSecret != secret {
		old, err := gatewayMCPSignature(oldSecret, id, timestamp, body)
		if err != nil {
			return nil, err
		}
		signature += " " + old
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, callback, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("webhook-id", id)
	req.Header.Set("webhook-timestamp", timestamp)
	req.Header.Set("webhook-signature", signature)
	req.Header.Set("X-MCP-Subscription-Id", subscription)
	return client.Do(req)
}

func gatewayMCPVerifyCallback(ctx context.Context, client *http.Client, callback, subscription, secret string, now time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return err
	}
	challenge := base64.RawURLEncoding.EncodeToString(random[:])
	id := "verification_" + challenge
	body, _ := json.Marshal(map[string]string{"type": "verification", "challenge": challenge})
	response, err := gatewayMCPPost(ctx, client, callback, subscription, secret, "", id, body, now)
	if err != nil {
		reason := "connection_failed"
		var networkError net.Error
		if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &networkError) && networkError.Timeout()) {
			reason = "timeout"
		}
		return &gatewayMCPWebhookFailure{reason}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &gatewayMCPWebhookFailure{"http_status"}
	}
	result, err := io.ReadAll(io.LimitReader(response.Body, 65537))
	if err != nil || len(result) > 65536 {
		return &gatewayMCPWebhookFailure{"challenge_failed"}
	}
	var verification struct {
		Challenge string `json:"challenge"`
	}
	if json.Unmarshal(result, &verification) != nil || subtle.ConstantTimeCompare([]byte(verification.Challenge), []byte(challenge)) != 1 {
		return &gatewayMCPWebhookFailure{"challenge_failed"}
	}
	return nil
}
