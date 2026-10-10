//go:build linux

package bleprovider

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"testing"
	"time"

	"github.com/wendylabsinc/wendy/go/internal/agent/localmesh"
)

func TestBLEServerRevalidatesResumedClient(t *testing.T) {
	var valid = true
	credentials := &localmesh.Credentials{Org: 64, Asset: 445,
		Verify: func(chain [][]byte, _ time.Time) (localmesh.Identity, error) {
			if !valid || len(chain) != 1 || len(chain[0]) != 1 {
				return localmesh.Identity{}, errors.New("expired or missing identity")
			}
			return localmesh.Identity{Org: 64, Asset: int32(chain[0][0]) + 400}, nil
		}}
	r := &runtime{cfg: Config{Credentials: credentials}}
	config := r.makeServerTLS()
	if config.SessionTicketsDisabled || config.WrapSession == nil || config.UnwrapSession == nil {
		t.Fatal("BLE server cannot resume scoped in-memory tickets")
	}
	state := tls.ConnectionState{DidResume: true, PeerCertificates: []*x509.Certificate{{Raw: []byte{60}}}}
	if err := config.VerifyConnection(state); err != nil {
		t.Fatalf("valid resumed asset rejected: %v", err)
	}
	state.PeerCertificates[0].Raw[0] = 44 // lower asset is never BLE inbound
	if err := config.VerifyConnection(state); err == nil {
		t.Fatal("resumed lower asset accepted")
	}
	state.PeerCertificates[0].Raw[0] = 60
	valid = false // current trust/expiry check must still run on resumption
	if err := config.VerifyConnection(state); err == nil {
		t.Fatal("resumed expired identity accepted")
	}
}
