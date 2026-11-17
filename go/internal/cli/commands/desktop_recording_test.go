package commands

import (
	"context"
	"strings"
	"testing"

	"github.com/wendylabsinc/wendy/go/internal/shared/config"
)

func TestDesktopRecordingConnectRejectsOtherSavedCloudIdentity(t *testing.T) {
	const tenantA = "8a53be77-2a69-464f-8f73-83643fe0beaa"
	const tenantB = "4d344658-f8f6-4c1e-aa14-9d96c2d56963"
	const asset = "7791d76e-a942-4a8e-b582-063d063b5623"
	login := func(name, tenant string) config.AuthConfig {
		return config.AuthConfig{Name: name, CloudGRPC: "cloud.example:443", Certificates: []config.CertificateInfo{{PrincipalURI: "spiffe://wendy.sh/tenant/" + tenant + "/operator/test"}}}
	}
	setTempConfig(t, &config.Config{CurrentContext: "active", Auth: []config.AuthConfig{login("other", tenantB), login("active", tenantA)}})
	selector := "cloud://cloud.example:443/tenant/" + tenantB + "/asset/" + asset
	if _, err := DesktopRecordingConnect(context.Background(), selector, false); err == nil || !strings.Contains(err.Error(), "current Cloud organization") {
		t.Fatalf("recording scan accepted another login: %v", err)
	}
}
