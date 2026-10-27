//go:build linux

package localmesh

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLeafTimestampRelayBoundsAndDestination(t *testing.T) {
	for _, tc := range []struct {
		name     string
		magic    string
		size     uint32
		response int
		status   int
		valid    bool
	}{
		{"valid", "WMT3", 128, 1234, 200, true},
		{"empty request", "WMT3", 0, 1, 200, false},
		{"oversize request", "WMT3", 129, 1, 200, false},
		{"wrong protocol", "WMT2", 1, 1, 200, false},
		{"oversize response", "WMT3", 1, 65537, 200, false},
		{"empty response", "WMT3", 1, 0, 200, false},
		{"HTTP error", "WMT3", 1, 1, 500, false},
		{"redirect", "WMT3", 1, 1, 307, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			authority := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/timestamp" || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/timestamp-query" {
					t.Error("changed authority request")
				}
				request, _ := io.ReadAll(r.Body)
				if len(request) != int(tc.size) {
					t.Error("changed timestamp request")
				}
				w.Header().Set("Location", "https://redirect.invalid/timestamp")
				w.WriteHeader(tc.status)
				_, _ = w.Write(bytes.Repeat([]byte{0x30}, tc.response))
			}))
			defer authority.Close()
			original := http.DefaultTransport
			http.DefaultTransport = authority.Client().Transport
			defer func() { http.DefaultTransport = original }()
			server, client := net.Pipe()
			defer client.Close()
			_ = client.SetDeadline(time.Now().Add(3 * time.Second))
			done := make(chan error, 1)
			go func() {
				defer server.Close()
				done <- serveLeafTime(context.Background(), server, authority.URL+"/timestamp")
			}()
			var header [8]byte
			copy(header[:4], tc.magic)
			binary.BigEndian.PutUint32(header[4:], tc.size)
			_, _ = client.Write(header[:])
			if tc.size > 0 && tc.size <= 128 && tc.magic == "WMT3" {
				_, _ = client.Write(bytes.Repeat([]byte{0x30}, int(tc.size)))
			}
			response, readErr := io.ReadAll(client)
			err := <-done
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
			if tc.valid && (readErr != nil || len(response) != tc.response+4 || binary.BigEndian.Uint32(response[:4]) != uint32(tc.response)) {
				t.Fatalf("invalid response: %d, %v", len(response), readErr)
			}
			if (tc.size == 0 || tc.size > 128 || tc.magic != "WMT3") && calls != 0 {
				t.Fatal("invalid request reached authority")
			}
			if calls > 1 {
				t.Fatal("followed redirect")
			}
		})
	}
}

func TestLeafTimestampURLIsFixedHTTPS(t *testing.T) {
	for _, value := range []string{"", "http://example.test/tsa", "https://user:secret@example.test/tsa", "https://example.test/tsa?q=1", "https://example.test/tsa#fragment"} {
		if validateLeafTimeURL(value) == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	if err := validateLeafTimeURL("https://example.test/tsa"); err != nil {
		t.Fatal(err)
	}
}

func TestLeafTimestampCancellation(t *testing.T) {
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	// ServeLeafRadio owns cancellation of its carrier; the helper has a
	// bounded deadline even if its caller doesn't close the connection.
	stop := context.AfterFunc(ctx, func() { _ = server.Close() })
	defer stop()
	done := make(chan error, 1)
	go func() { done <- serveLeafTime(ctx, server, "https://example.test/tsa") }()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("accepted canceled request")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation blocked")
	}
}

func TestLeafBLEConfig(t *testing.T) {
	self := testDevice(t, testPrincipal(testTenantUUID, testDeviceAID))
	body := `{"leafBLEPeers":[{"device":"` + testDeviceBID + `","address":"c1:22:33:44:55:66","addressType":"random"}],"leafTimeURL":"https://example.test/tsa"}`
	for _, tc := range []struct {
		body  string
		valid bool
	}{
		{body, true},
		{strings.Replace(body, testDeviceBID, testDeviceAID, 1), false},
		{strings.Replace(body, "random", "unknown", 1), false},
		{strings.Replace(body, "https:", "http:", 1), false},
		{strings.Replace(body, "c1:22:33:44:55:66", "bad", 1), false},
		{strings.Replace(body, `"addressType":"random"`, `"psm":127`, 1), false},
		{strings.Replace(body, `,"leafTimeURL":"https://example.test/tsa"`, "", 1), false},
	} {
		path := t.TempDir() + "/mesh.json"
		if err := os.WriteFile(path, []byte(tc.body), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadTCPConfig(path, self)
		if (err == nil) != tc.valid {
			t.Fatalf("valid=%v, error=%v, %s", tc.valid, err, tc.body)
		}
		if tc.valid && cfg.LeafBLEPeers[0].PSM != 129 {
			t.Fatal("missing default mesh PSM")
		}
	}
}
