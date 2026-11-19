package backend

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPreviewThermalReaderBoundsAndRedirects(t *testing.T) {
	tests := []struct {
		name, body string
		code       int
		valid      bool
	}{
		{"valid", `{"sensors":[]}`, 200, true},
		{"malformed", `<html>error</html>`, 200, false},
		{"oversized", `{"sensors":[],"padding":"` + strings.Repeat("x", 262144) + `"}`, 200, false},
		{"failure", `{"sensors":[]}`, 503, false},
		{"redirect", ``, 302, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tt.code == 302 {
					w.Header().Set("Location", "/target")
				}
				if r.URL.Path == "/target" {
					t.Error("followed redirect")
				}
				w.WriteHeader(tt.code)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()
			_, err := readPreviewThermal(context.Background(), server.URL)
			if (err == nil) != tt.valid {
				t.Fatalf("valid=%v err=%v", tt.valid, err)
			}
		})
	}
}

func TestPreviewThermalReaderCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := readPreviewThermal(ctx, server.URL); err == nil {
		t.Fatal("cancelled read succeeded")
	}
}
