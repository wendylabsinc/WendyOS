package localmesh

import (
	"errors"
	"testing"
	"time"
)

func TestCryptoPathReuseDoesNotCacheIdentityAuthorization(t *testing.T) {
	self, peer := testCredentials(t)
	now := time.Now()
	revoked := false
	checks := 0
	errRevoked := errors.New("identity revoked")
	cache, err := OpenIdentityCache("", DefaultCacheLimits(), func(chain [][]byte, at time.Time) (Identity, error) {
		checks++
		identity, err := self.Verify(chain, at)
		if err != nil {
			return Identity{}, err
		}
		if revoked {
			return Identity{}, errRevoked
		}
		return identity, nil
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := cache.Put(peer.Certificate.Certificate, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok := cache.Get(principal, now); !ok {
		t.Fatal("current identity rejected")
	}
	before := checks
	revoked = true
	if _, _, ok := cache.Get(principal, now); ok || checks != before+1 {
		t.Fatal("warm cryptographic path bypassed current identity revocation")
	}
	// The cryptographic path of this foreign-tenant certificate is valid and
	// becomes warm on the first call. Tenant authorization must reject both.
	foreign := testCredentialsFor(t, testPrincipal(testOtherTenant, testDeviceBID))
	for range 2 {
		if _, err := self.Verify(foreign.Certificate.Certificate, now); err == nil {
			t.Fatal("warm cryptographic path bypassed tenant authorization")
		}
	}
}
