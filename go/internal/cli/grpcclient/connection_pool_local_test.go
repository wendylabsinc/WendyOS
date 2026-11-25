package grpcclient

import (
	"sync/atomic"
	"testing"
)

// A pooled handle to a Closed device keeps what the local-key dial verified:
// the registry and `wendy device access open` read it from the handle.
func TestSharedHandleKeepsLocalKeyState(t *testing.T) {
	spki := []byte("device key")
	verified := new(atomic.Pointer[[]byte])
	verified.Store(&spki)
	c := &AgentConnection{IsMTLS: true, IsLocalOperator: true, localDeviceVerified: verified, notLocalDevice: new(atomic.Bool)}
	h := c.sharedHandle(nil)
	if !h.IsLocalOperator || string(h.LocalDeviceKey()) != "device key" || h.notLocalDevice == nil {
		t.Fatalf("handle lost local-key state: local=%v key=%q", h.IsLocalOperator, h.LocalDeviceKey())
	}
}
