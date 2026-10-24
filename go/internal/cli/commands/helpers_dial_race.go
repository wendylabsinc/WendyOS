package commands

import (
	"crypto/x509"
	"sync"

	"github.com/wendylabsinc/wendy/go/internal/shared/certs"
)

type concurrentPinChecker struct {
	mu    sync.Mutex
	store certs.PinChecker
}

func (p *concurrentPinChecker) CheckAndUpdate(leaf *x509.Certificate, displayName string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.store.CheckAndUpdate(leaf, displayName)
}
