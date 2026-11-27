package services

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net/url"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestLiteIssuerRefreshCommitsBeforePublishingAndSurvivesOfflineRestart(t *testing.T) {
	const tenant = "11111111-1111-4111-8111-111111111111"
	root := newRenewCA(t)
	now := time.Now()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	lite := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: tenant + " Lite Device CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, MaxPathLenZero: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, lite, root.cert, key.Public(), root.key)
	if err != nil {
		t.Fatal(err)
	}
	bundle := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	svc, dir, _ := enrolledForRenewal(t, root, "", nil)
	state, err := svc.readProvisioningState()
	if err != nil {
		t.Fatal(err)
	}
	state.ACMEDirectoryURL = "https://acme.pki.test/" + tenant + "/acme/directory"
	if err := svc.saveState(state); err != nil {
		t.Fatal(err)
	}
	svc = NewProvisioningService(zap.NewNop(), dir)

	fetch := fetchLiteIssuers
	t.Cleanup(func() { fetchLiteIssuers = fetch })
	fetchLiteIssuers = func(_ context.Context, endpoint *url.URL) (string, error) {
		if endpoint.Host != "est.pki.test" || endpoint.Path != "/.well-known/est/"+tenant+"/cacerts" {
			t.Errorf("unexpected EST endpoint: %v", endpoint)
		}
		return bundle, nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var published bool
	svc.RunLiteIssuerRefresh(ctx, func() {
		state, err := svc.readProvisioningState()
		if err != nil {
			t.Fatal(err)
		}
		published = svc.LitePeerIssuers() == bundle && state.LitePeerIssuers == bundle
		cancel()
	})
	if !published {
		t.Fatal("new issuers were not committed before the live reload callback")
	}

	fetchLiteIssuers = func(context.Context, *url.URL) (string, error) { return "", io.EOF }
	restarted := NewProvisioningService(zap.NewNop(), dir)
	changed, err := restarted.refreshLiteIssuers(t.Context())
	if changed || !errors.Is(err, io.EOF) || restarted.LitePeerIssuers() != bundle {
		t.Fatalf("offline restart lost the valid issuer snapshot: changed=%t error=%v", changed, err)
	}
}
