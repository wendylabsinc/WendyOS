package mcp

import (
	"errors"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestProxyDiag_RecordAndRead(t *testing.T) {
	s := newTestServer(&config.Config{}, nil)
	s.recordProxyDiag("paperless", "initialize", errors.New("boom"))
	d := s.proxyDiagnostics()
	if len(d) != 1 || d[0].AppName != "paperless" || d[0].Stage != "initialize" || d[0].Error != "boom" {
		t.Fatalf("unexpected diagnostics: %+v", d)
	}
}

func TestProxyDiag_NilErrorNotRecorded(t *testing.T) {
	s := newTestServer(&config.Config{}, nil)
	s.recordProxyDiag("paperless", "initialize", nil)
	d := s.proxyDiagnostics()
	if len(d) != 0 {
		t.Fatalf("expected no diagnostics recorded for nil error, got: %+v", d)
	}
}
