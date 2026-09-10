package services

import (
	"errors"
	"testing"

	"go.uber.org/zap"

	"github.com/wendylabsinc/wendy/go/internal/agent/pkienroll"
)

// stubPKIIdentity stands in for the pki-core store so the choice of identity
// can be tested without issuing certificates.
type stubPKIIdentity struct {
	material pkienroll.Material
	err      error
	calls    int
}

func (s *stubPKIIdentity) Load() (pkienroll.Material, error) {
	s.calls++
	return s.material, s.err
}

// workerWithIdentities builds a worker whose asset (Certificate Authority
// Service) identity is the given material. ProvisioningService is used
// directly rather than mocked because ProvisioningCerts reads only its own
// fields.
func workerWithIdentities(t *testing.T, assetCert, assetChain, assetKey string) *DataTransferWorker {
	t.Helper()
	prov := &ProvisioningService{
		logger:   zap.NewNop(),
		enrolled: true,
		orgID:    2,
		assetID:  408,
		certPEM:  assetCert,
		chainPEM: assetChain,
		keyPEM:   []byte(assetKey),
	}
	return &DataTransferWorker{
		logger:          zap.NewNop(),
		provisioningSvc: prov,
		ingestHost:      "ingest.data.wendy.sh:443",
	}
}

func TestIngestIdentityPrefersPKITriple(t *testing.T) {
	w := workerWithIdentities(t, "asset-leaf", "asset-chain", "asset-key")
	stub := &stubPKIIdentity{material: pkienroll.Material{
		LeafPEM:  "pki-leaf",
		ChainPEM: "pki-chain",
		KeyData:  []byte("pki-key"),
	}}
	w.SetPKIIdentity(stub)

	certPEM, chainPEM, keyData, source, err := w.ingestIdentity()
	if err != nil {
		t.Fatalf("ingestIdentity: %v", err)
	}
	if source != identitySourcePKI {
		t.Errorf("source = %q, want %q", source, identitySourcePKI)
	}
	if certPEM != "pki-leaf" || chainPEM != "pki-chain" || string(keyData) != "pki-key" {
		t.Errorf("got (%q, %q, %q), want the pki triple", certPEM, chainPEM, keyData)
	}
	if stub.calls != 1 {
		t.Errorf("store consulted %d times, want 1", stub.calls)
	}
}

func TestIngestIdentityFallsBackWhenNoPKIIdentity(t *testing.T) {
	// No store wired at all: exactly today's behaviour, and the case every
	// device that has never been enrolled against pki-core is in.
	w := workerWithIdentities(t, "asset-leaf", "asset-chain", "asset-key")
	certPEM, chainPEM, keyData, source, err := w.ingestIdentity()
	if err != nil {
		t.Fatalf("ingestIdentity: %v", err)
	}
	if source != identitySourceAsset {
		t.Errorf("source = %q, want %q", source, identitySourceAsset)
	}
	if certPEM != "asset-leaf" || chainPEM != "asset-chain" || string(keyData) != "asset-key" {
		t.Errorf("got (%q, %q, %q), want the asset triple", certPEM, chainPEM, keyData)
	}
}

func TestIngestIdentityFallsBackOnEmptyStore(t *testing.T) {
	w := workerWithIdentities(t, "asset-leaf", "asset-chain", "asset-key")
	w.SetPKIIdentity(&stubPKIIdentity{err: pkienroll.ErrNoIdentity})

	certPEM, _, _, source, err := w.ingestIdentity()
	if err != nil {
		t.Fatalf("ingestIdentity: %v", err)
	}
	if source != identitySourceAsset {
		t.Errorf("source = %q, want %q", source, identitySourceAsset)
	}
	if certPEM != "asset-leaf" {
		t.Errorf("certPEM = %q, want the asset leaf", certPEM)
	}
}

func TestIngestIdentityFallsBackOnUnreadableStore(t *testing.T) {
	// A real read fault, not an absent identity. The dial still has to happen,
	// so the asset certificate is used and the fault is reported rather than
	// stopping uploads.
	w := workerWithIdentities(t, "asset-leaf", "asset-chain", "asset-key")
	w.SetPKIIdentity(&stubPKIIdentity{err: errors.New("permission denied")})

	_, _, _, source, err := w.ingestIdentity()
	if err != nil {
		t.Fatalf("ingestIdentity: %v", err)
	}
	if source != identitySourceAsset {
		t.Errorf("source = %q, want %q", source, identitySourceAsset)
	}
}

func TestIngestIdentityFallsBackOnHalfWrittenStore(t *testing.T) {
	// A leaf with no key (or the reverse) is not a usable identity. Presenting
	// it would fail the handshake, so it must read as absent.
	for name, material := range map[string]pkienroll.Material{
		"no key":  {LeafPEM: "pki-leaf"},
		"no leaf": {KeyData: []byte("pki-key")},
	} {
		t.Run(name, func(t *testing.T) {
			w := workerWithIdentities(t, "asset-leaf", "asset-chain", "asset-key")
			w.SetPKIIdentity(&stubPKIIdentity{material: material})
			_, _, _, source, err := w.ingestIdentity()
			if err != nil {
				t.Fatalf("ingestIdentity: %v", err)
			}
			if source != identitySourceAsset {
				t.Errorf("source = %q, want %q", source, identitySourceAsset)
			}
		})
	}
}

func TestIngestIdentityReadsThePKIStorePerDial(t *testing.T) {
	// The store is consulted on every dial rather than cached, so a device
	// enrolled after the agent came up switches identity on its next upload
	// pass without a restart.
	w := workerWithIdentities(t, "asset-leaf", "asset-chain", "asset-key")
	stub := &stubPKIIdentity{err: pkienroll.ErrNoIdentity}
	w.SetPKIIdentity(stub)

	if _, _, _, source, _ := w.ingestIdentity(); source != identitySourceAsset {
		t.Fatalf("source = %q before enrolment, want %q", source, identitySourceAsset)
	}
	stub.err = nil
	stub.material = pkienroll.Material{LeafPEM: "pki-leaf", KeyData: []byte("pki-key")}
	if _, _, _, source, _ := w.ingestIdentity(); source != identitySourcePKI {
		t.Errorf("source = %q after enrolment, want %q", source, identitySourcePKI)
	}
}

func TestSetPKIIdentityNilRestoresPreviousBehaviour(t *testing.T) {
	w := workerWithIdentities(t, "asset-leaf", "asset-chain", "asset-key")
	w.SetPKIIdentity(&stubPKIIdentity{material: pkienroll.Material{LeafPEM: "pki-leaf", KeyData: []byte("k")}})
	w.SetPKIIdentity(nil)
	if _, _, _, source, _ := w.ingestIdentity(); source != identitySourceAsset {
		t.Errorf("source = %q, want %q", source, identitySourceAsset)
	}
}
