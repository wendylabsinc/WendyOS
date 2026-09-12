package services

import (
	"os"
	"strings"
	"testing"
)

// TestDataPathSendsNoIdentityHeader pins the one property the data path's
// identity must keep: it is a client certificate, presented in the Transport
// Layer Security handshake, and no request header carries it. The
// x-wendy-client-cert header was a self-asserted identity the ingest service
// no longer reads; the tunnel broker client is a different service and is not
// covered here.
func TestDataPathSendsNoIdentityHeader(t *testing.T) {
	for _, f := range []string{"data_transfer_worker.go", "cloud_flusher.go"} {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"x-wendy-client-cert", "urn:wendy:org:", "certIdentityDialOptions"} {
			if strings.Contains(string(src), forbidden) {
				t.Errorf("%s mentions %q; the data path must not carry an identity header", f, forbidden)
			}
		}
	}
}
