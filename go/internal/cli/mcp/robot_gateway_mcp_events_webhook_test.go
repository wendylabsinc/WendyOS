package mcp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"
)

type gatewayMCPRoundTripFunc func(*http.Request) (*http.Response, error)

func (f gatewayMCPRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestMCPEventCallbackRejectsNonPublicDestinations(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.1.2.3", "100.100.100.100", "169.254.169.254", "172.17.0.1", "192.168.1.1", "192.0.2.1", "198.18.0.1", "198.51.100.1", "203.0.113.2", "0.0.0.0", "240.1.1.1", "::1", "::ffff:127.0.0.1", "fc00::1", "fe80::1", "2001:db8::1", "64:ff9b::7f00:1", "2002:7f00:1::"} {
		if gatewayMCPPublicIP(netip.MustParseAddr(raw)) {
			t.Errorf("accepted non-public destination %s", raw)
		}
	}
	for _, raw := range []string{"1.1.1.1", "8.8.8.8", "2606:4700:4700::1111"} {
		if !gatewayMCPPublicIP(netip.MustParseAddr(raw)) {
			t.Errorf("rejected public destination %s", raw)
		}
	}
	for _, raw := range []string{"http://example.com/callback", "https://user:pass@example.com/", "https://example.com/#secret", "https://127.0.0.1/callback", "https://[::1]/callback", "https://example.com:99999/"} {
		if _, err := gatewayMCPCallbackURL(raw); err == nil {
			t.Errorf("accepted callback %s", raw)
		}
	}
	client := gatewayMCPWebhookClient()
	if err := client.CheckRedirect(nil, nil); err != http.ErrUseLastResponse {
		t.Fatal("callback redirects are enabled")
	}
	transport := client.Transport.(*http.Transport)
	if transport.Proxy != nil || !transport.DisableKeepAlives {
		t.Fatal("callback address validation can be bypassed by proxy or connection reuse")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if conn, err := transport.DialContext(ctx, "tcp", "127.0.0.1:443"); err == nil {
		conn.Close()
		t.Fatal("transport dialed a private callback")
	}
}

func TestMCPEventVerificationAndRotationSignExactBody(t *testing.T) {
	key := strings.Repeat("a", 32)
	secret := "whsec_" + base64.StdEncoding.EncodeToString([]byte(key))
	oldSecret := "whsec_" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("b", 32)))
	now := time.Unix(1790800000, 0)
	calls := 0
	client := &http.Client{Transport: gatewayMCPRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		body, _ := io.ReadAll(r.Body)
		id, timestamp := r.Header.Get("webhook-id"), r.Header.Get("webhook-timestamp")
		mac := hmac.New(sha256.New, []byte(key))
		mac.Write([]byte(id + "." + timestamp + "."))
		mac.Write(body)
		expected := "v1," + base64.StdEncoding.EncodeToString(mac.Sum(nil))
		if !strings.HasPrefix(r.Header.Get("webhook-signature"), expected) || timestamp != "1790800000" || r.Header.Get("X-MCP-Subscription-Id") != "sub_test" {
			t.Fatal("invalid Standard Webhooks signature headers")
		}
		if calls == 1 {
			var challenge map[string]string
			json.Unmarshal(body, &challenge)
			if challenge["type"] != "verification" || len(challenge["challenge"]) < 32 {
				t.Fatal("missing fresh challenge")
			}
			response, _ := json.Marshal(map[string]string{"challenge": challenge["challenge"]})
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(response)))}, nil
		}
		if strings.Count(r.Header.Get("webhook-signature"), "v1,") != 2 {
			t.Fatal("secret rotation omitted previous signature")
		}
		return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	if err := gatewayMCPVerifyCallback(context.Background(), client, "https://receiver.example/events", "sub_test", secret, now); err != nil {
		t.Fatal(err)
	}
	resp, err := gatewayMCPPost(context.Background(), client, "https://receiver.example/events", "sub_test", secret, oldSecret, "event_test", []byte(`{"eventId":"event_test","data":{"name":"é"}}`), now)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestMCPEventVerificationRequiresMatchingChallenge(t *testing.T) {
	secret := "whsec_" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 32)))
	for _, status := range []int{200, 302, 410} {
		client := &http.Client{Transport: gatewayMCPRoundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"challenge":"wrong"}`))}, nil
		})}
		if err := gatewayMCPVerifyCallback(context.Background(), client, "https://receiver.example/", "sub_test", secret, time.Now()); err == nil {
			t.Fatalf("accepted mismatching challenge with status %d", status)
		}
	}
	for _, value := range []string{"secret", "whsec_?", "whsec_" + base64.StdEncoding.EncodeToString(make([]byte, 23)), "whsec_" + base64.StdEncoding.EncodeToString(make([]byte, 65))} {
		if _, err := gatewayMCPWebhookKey(value); err == nil {
			t.Fatal("accepted invalid signing secret")
		}
	}
}
