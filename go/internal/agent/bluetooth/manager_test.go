//go:build !linux

package bluetooth

import (
	"context"
	"testing"

	"go.uber.org/zap"
)

func TestNewManager_ReturnsNonNil(t *testing.T) {
	logger := zap.NewNop()
	m := NewManager(logger)
	if m == nil {
		t.Fatal("NewManager returned nil")
	}
}

func TestStubManager_ScanReturnsUnsupported(t *testing.T) {
	logger := zap.NewNop()
	m := NewManager(logger)

	ch, err := m.Scan(context.Background())
	if ch != nil {
		t.Error("expected nil channel from stub Scan")
	}
	if err == nil {
		t.Fatal("expected error from stub Scan")
	}
	if err.Error() != errUnsupported.Error() {
		t.Errorf("expected errUnsupported, got: %v", err)
	}
}

func TestStubManager_ConnectReturnsUnsupported(t *testing.T) {
	logger := zap.NewNop()
	m := NewManager(logger)

	err := m.Connect(context.Background(), "AA:BB:CC:DD:EE:FF", false, false)
	if err == nil {
		t.Fatal("expected error from stub Connect")
	}
	if err.Error() != errUnsupported.Error() {
		t.Errorf("expected errUnsupported, got: %v", err)
	}
}

func TestStubManager_DisconnectReturnsUnsupported(t *testing.T) {
	logger := zap.NewNop()
	m := NewManager(logger)

	err := m.Disconnect(context.Background(), "AA:BB:CC:DD:EE:FF")
	if err == nil {
		t.Fatal("expected error from stub Disconnect")
	}
	if err.Error() != errUnsupported.Error() {
		t.Errorf("expected errUnsupported, got: %v", err)
	}
}

func TestStubManager_ForgetReturnsUnsupported(t *testing.T) {
	logger := zap.NewNop()
	m := NewManager(logger)

	err := m.Forget(context.Background(), "AA:BB:CC:DD:EE:FF")
	if err == nil {
		t.Fatal("expected error from stub Forget")
	}
	if err.Error() != errUnsupported.Error() {
		t.Errorf("expected errUnsupported, got: %v", err)
	}
}

func TestStubManager_ImplementsManagerInterface(t *testing.T) {
	logger := zap.NewNop()
	m := NewManager(logger)

	// Verify the returned value satisfies the Manager interface at compile time.
	var _ Manager = m
}
