//go:build linux

package bleprovider

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"

	"github.com/google/uuid"
	"github.com/wendylabsinc/wendy/go/internal/shared/meshid"
)

func TestBLEServerRevalidatesResumedClient(t *testing.T) {
	var valid = true
	credentials := &localmesh.Credentials{Device: selfFixture,
		Verify: func(chain [][]byte, _ time.Time) (localmesh.Identity, error) {
			if !valid || len(chain) != 1 || len(chain[0]) != 1 {
				return localmesh.Identity{}, errors.New("expired or missing identity")
			}
			peer := uuid.MustParse("6f6b1c9e-2d4a-4c8f-9a01-7e5d3b2c1a00")
			peer[15] = chain[0][0]
			return localmesh.Identity{Device: meshid.NewDevice(testTenant, peer)}, nil
		}}
	r := &runtime{cfg: Config{Credentials: credentials}}
	config := r.makeServerTLS()
	if config.SessionTicketsDisabled || config.WrapSession == nil || config.UnwrapSession == nil {
		t.Fatal("BLE server cannot resume scoped in-memory tickets")
	}
	state := tls.ConnectionState{DidResume: true, PeerCertificates: []*x509.Certificate{{Raw: []byte{0x60}}}}
	if err := config.VerifyConnection(state); err != nil {
		t.Fatalf("valid resumed device rejected: %v", err)
	}
	state.PeerCertificates[0].Raw[0] = 0x04 // UUID order does not gate authenticated inbound peers
	if err := config.VerifyConnection(state); err != nil {
		t.Fatal("resumed lower device rejected")
	}
	state.PeerCertificates[0].Raw[0] = 0x60
	valid = false // current trust/expiry check must still run on resumption
	if err := config.VerifyConnection(state); err == nil {
		t.Fatal("resumed expired identity accepted")
	}
}

func TestBLEDiscoveryAuthenticatesFullUUIDWithCollidingHint(t *testing.T) {
	peer := selfFixture.UUID()
	peer[15]--
	identity := meshid.NewDevice(testTenant, peer)
	trusted := true
	credentials := &localmesh.Credentials{Device: selfFixture,
		Verify: func(_ [][]byte, _ time.Time) (localmesh.Identity, error) {
			if !trusted {
				return localmesh.Identity{}, errors.New("untrusted certificate")
			}
			return localmesh.Identity{Device: identity}, nil
		},
	}
	r := &runtime{cfg: Config{Credentials: credentials}}
	config, authenticated, err := r.clientTLS(candidate{hint: deviceHint(peer)})
	if err != nil {
		t.Fatal(err)
	}
	for _, resumed := range []bool{false, true} {
		state := tls.ConnectionState{DidResume: resumed}
		if err := config.VerifyConnection(state); err != nil || *authenticated != identity {
			t.Fatalf("lower UUID with same hint: %v", err)
		}
		identity = selfFixture
		if err := config.VerifyConnection(state); err == nil {
			t.Fatal("accepted self identity")
		}
		peer[15] += 2
		identity = meshid.NewDevice(testTenant, peer)
		if err := config.VerifyConnection(state); err != nil {
			t.Fatal("rejected higher UUID with same hint")
		}
		peer[15] -= 2
		identity = meshid.NewDevice(otherTenant, peer)
		if err := config.VerifyConnection(state); err == nil {
			t.Fatal("accepted other tenant")
		}
		identity = lowFixture
		if err := config.VerifyConnection(state); err == nil {
			t.Fatal("accepted different discovery hint")
		}
		identity = meshid.NewDevice(testTenant, peer)
		trusted = false
		if err := config.VerifyConnection(state); err == nil {
			t.Fatal("accepted untrusted certificate")
		}
		trusted = true
	}
}
