package mcp

import (
	"errors"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestProxyDiag_RecordAndRead(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.recordProxyDiag("paperless", "initialize", errors.New("boom"))
	d := s.proxyDiagnostics()
	if len(d) != 1 || d[0].AppName != "paperless" || d[0].Stage != "initialize" || d[0].Error != "boom" {
		t.Fatalf("unexpected diagnostics: %+v", d)
	}
}

func TestProxyDiag_NilErrorNotRecorded(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.recordProxyDiag("paperless", "initialize", nil)
	d := s.proxyDiagnostics()
	if len(d) != 0 {
		t.Fatalf("expected no diagnostics recorded for nil error, got: %+v", d)
	}
}

func TestProxyDiag_BoundsPeriodicFailures(t *testing.T) {
	s := New(&config.Config{}, nil)
	s.recordProxyDiag("oldest", "connect", errors.New("offline"))
	for i := 0; i < maxProxyDiagnostics; i++ {
		s.recordProxyDiag("latest", "connect", errors.New("offline"))
	}
	d := s.proxyDiagnostics()
	if len(d) != maxProxyDiagnostics || d[0].AppName != "latest" {
		t.Fatalf("periodic failures were not bounded to the newest entries: %+v", d)
	}
}
