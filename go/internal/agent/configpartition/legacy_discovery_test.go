package configpartition

import (
	"strings"
	"testing"
)

func TestAvahiTransitionsBetweenCloudEnrollments(t *testing.T) {
	input := `<service-group><service><type>_wendyos._udp</type><port>50052</port><txt-record>tls=true</txt-record><txt-record>principal=spiffe://old/device</txt-record></service><service><type>_http._tcp</type><port>8080</port></service></service-group>`
	legacy := updateWendyOSServicePort(input, 50052, true, "", 674, 64)
	for _, want := range []string{"assetid=674", "orgid=64", "tls=true", "<port>8080</port>"} {
		if !strings.Contains(legacy, want) {
			t.Fatalf("legacy advertisement missing %s: %s", want, legacy)
		}
	}
	if strings.Contains(legacy, "principal=") {
		t.Fatal("legacy advertisement retained the old principal")
	}
	pki := updateWendyOSServicePort(legacy, 50052, true, testPrincipal)
	if strings.Contains(pki, "assetid=") || strings.Contains(pki, "orgid=") || !strings.Contains(pki, "principal="+testPrincipal) {
		t.Fatalf("PKI transition retained numeric identity: %s", pki)
	}
	setup := updateWendyOSServicePort(legacy, 50051, false, "")
	if strings.Contains(setup, "assetid=") || strings.Contains(setup, "orgid=") || !strings.Contains(setup, "<port>50051</port>") || !strings.Contains(setup, "tls=false") {
		t.Fatalf("reset retained enrollment: %s", setup)
	}
}
