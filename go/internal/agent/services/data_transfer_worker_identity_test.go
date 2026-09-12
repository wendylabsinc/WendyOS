package services

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"go.uber.org/zap/zapcore"
)

// workerWithIdentity builds a worker whose provisioning holds the given
// certificate triple. ProvisioningService is used directly rather than mocked
// because ProvisioningCerts reads only its own fields.
func workerWithIdentity(t *testing.T, logger *zap.Logger, certPEM, chainPEM, keyPEM string) *DataTransferWorker {
	t.Helper()
	return &DataTransferWorker{
		logger: logger,
		provisioningSvc: &ProvisioningService{
			logger:   zap.NewNop(),
			enrolled: true,
			certPEM:  certPEM,
			chainPEM: chainPEM,
			keyPEM:   []byte(keyPEM),
		},
		ingestHost: "ingest.data.wendy.sh:443",
	}
}

// TestIngestIdentityUsesTheEnrolledCertificate pins that the worker presents
// the certificate provisioning obtained, and nothing it chose for itself.
func TestIngestIdentityUsesTheEnrolledCertificate(t *testing.T) {
	w := workerWithIdentity(t, zap.NewNop(), "device-leaf", "device-chain", "device-key")

	certPEM, chainPEM, keyData, err := w.ingestIdentity()
	if err != nil {
		t.Fatalf("ingestIdentity: %v", err)
	}
	if certPEM != "device-leaf" || chainPEM != "device-chain" || string(keyData) != "device-key" {
		t.Errorf("got (%q, %q, %q), want the enrolled triple", certPEM, chainPEM, keyData)
	}
}

// TestIngestIdentityRefusesWhenNoCertificate pins the absence of a fallback:
// with no enrolled certificate there is no second identity to reach for, so
// the worker reports errNoIdentity rather than dialling with something else.
func TestIngestIdentityRefusesWhenNoCertificate(t *testing.T) {
	w := workerWithIdentity(t, zap.NewNop(), "", "", "")

	certPEM, chainPEM, keyData, err := w.ingestIdentity()
	if !errors.Is(err, errNoIdentity) {
		t.Fatalf("err = %v, want errNoIdentity", err)
	}
	if certPEM != "" || chainPEM != "" || keyData != nil {
		t.Errorf("got (%q, %q, %v), want nothing", certPEM, chainPEM, keyData)
	}
}

// TestDialFactoryLogsOnceWithoutIdentity pins the reporting contract: a device
// with no identity says so once and does not dial, rather than repeating the
// same line on every pass or dialling anonymously.
func TestDialFactoryLogsOnceWithoutIdentity(t *testing.T) {
	core, logs := observer.New(zapcore.ErrorLevel)
	w := workerWithIdentity(t, zap.New(core), "", "", "")

	for range 3 {
		client, closeFn, err := w.dialFactory(context.Background())
		if !errors.Is(err, errNoIdentity) {
			t.Fatalf("err = %v, want errNoIdentity", err)
		}
		if client != nil || closeFn != nil {
			t.Fatal("dialFactory returned a client without an identity")
		}
	}
	if n := logs.FilterMessageSnippet("no enrolled device identity").Len(); n != 1 {
		t.Errorf("logged %d times, want exactly 1", n)
	}
}
