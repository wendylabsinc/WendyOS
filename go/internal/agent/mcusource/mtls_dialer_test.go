package mcusource

import (
	"testing"

	"go.uber.org/zap"
)

func TestNewMTLSDialerRejectsMissingIdentity(t *testing.T) {
	factory := NewMTLSDialer(zap.NewNop(), func() (string, string, string) { return "", "", "" })
	if _, err := factory(SensorPairing{SourceAssetID: 7, OrgID: 3}); err == nil {
		t.Fatal("expected an error when the agent has no mTLS identity")
	}
}

func TestNewMTLSDialerPinsPairingIdentity(t *testing.T) {
	factory := NewMTLSDialer(zap.NewNop(), func() (string, string, string) { return "cert", "chain", "key" })
	d, err := factory(SensorPairing{SourceAssetID: 42, OrgID: 9})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	md, ok := d.(mtlsDialer)
	if !ok {
		t.Fatalf("expected mtlsDialer, got %T", d)
	}
	if md.orgID != 9 || md.assetID != 42 {
		t.Fatalf("expected dialer pinned to org=9 asset=42, got org=%d asset=%d", md.orgID, md.assetID)
	}
	if md.certPEM != "cert" || md.chainPEM != "chain" || md.keyPEM != "key" {
		t.Fatalf("expected agent identity carried through, got %+v", md)
	}
}
