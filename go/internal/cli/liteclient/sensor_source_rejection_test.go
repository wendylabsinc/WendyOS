package liteclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

type fakeAlert string

func (e fakeAlert) Error() string { return string(e) }

func TestPeerRejectedCertificate(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&net.OpError{Op: "remote error", Err: fakeAlert("tls: bad certificate")}, true},
		{&net.OpError{Op: "remote error", Err: fakeAlert("tls: unknown certificate authority")}, true},
		{&net.OpError{Op: "remote error", Err: fakeAlert("tls: certificate required")}, true},
		{&net.OpError{Op: "local error", Err: fakeAlert("tls: bad record MAC")}, true},
		{&net.OpError{Op: "remote error", Err: fakeAlert("tls: internal error")}, false},
		{&net.OpError{Op: "read", Err: errors.New("connection reset by peer")}, false},
		{context.DeadlineExceeded, false},
	} {
		if got := peerRejectedCertificate(tc.err); got != tc.want {
			t.Errorf("peerRejectedCertificate(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// A source that does not trust the consumer's certificate rejects it only
// after the consumer's TLS 1.3 handshake has finished. The consumer must
// report that, not a bare TLS read failure.
func TestSensorSourceReportsRejectedConsumerCertificate(t *testing.T) {
	const tenant = "11111111-2222-4333-8444-555555555555"
	base := "spiffe://wendy.sh/tenant/" + tenant + "/"
	root, rootKey := issueRelayCertificate(t, 300, "root", "", true, nil, nil)
	ca, caKey := issueRelayCertificate(t, 301, "Identity CA", "", true, root, rootKey)
	consumer, key := issueRelayCertificate(t, 302, "consumer", base+"device/consumer", false, ca, caKey)
	client := tls.Certificate{Certificate: [][]byte{consumer.Raw, ca.Raw}, PrivateKey: key}
	chain := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw}))
	source, sourceKey := issueRelayCertificate(t, 303, "source", base+"device/source", false, root, rootKey)
	sourceRoot, _ := issueRelayCertificate(t, 304, "source's own root", "", true, nil, nil)
	sourceTrust := x509.NewCertPool()
	sourceTrust.AddCert(sourceRoot)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			return
		}
		defer raw.Close()
		raw.SetDeadline(time.Now().Add(5 * time.Second))
		conn := tls.Server(raw, &tls.Config{MinVersion: tls.VersionTLS13,
			Certificates: []tls.Certificate{{Certificate: [][]byte{source.Raw}, PrivateKey: sourceKey}},
			ClientAuth:   tls.RequireAndVerifyClientCert, ClientCAs: sourceTrust})
		conn.Handshake()
	}()
	err = NewWendyLiteClient().ConnectSensorSource(context.Background(), listener.Addr().String(), client, chain, base+"device/source", nil)
	if !errors.Is(err, ErrSourceRejectedCertificate) {
		t.Fatalf("err = %v, want ErrSourceRejectedCertificate", err)
	}
	if !strings.Contains(err.Error(), "Identity CA") {
		t.Fatalf("err = %v, want it to name this device's issuer", err)
	}
}
