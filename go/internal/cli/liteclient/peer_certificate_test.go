package liteclient

import "testing"

// Only a connect that verified the device's certificate reports one.
func TestPeerCertificateNilWithoutVerifiedConnect(t *testing.T) {
	if NewWendyLiteClient().PeerCertificate() != nil {
		t.Fatal("PeerCertificate set on a client that never connected")
	}
	c, _ := connectedOverPipe(t)
	if c.PeerCertificate() != nil {
		t.Fatal("PeerCertificate set on an unverified link")
	}
}
