package configpartition

import (
	"strings"
	"testing"
)

func TestApplicationTLSAdvertisementRequiresEnrolledIdentity(t *testing.T) {
	const setup = `<service><type>_wendyos._udp</type><port>50051</port><txt-record>tls=false</txt-record></service>`
	if got := updateApplicationTLSTXT(setup, 43023); strings.Contains(got, "app-tls=") {
		t.Fatal("unenrolled endpoint advertised app TLS")
	}
	enrolled := updateWendyOSServicePort(setup, 50052, true, testPrincipal)
	ready := updateApplicationTLSTXT(enrolled, 43023)
	if strings.Count(ready, "app-tls=43023") != 1 || updateApplicationTLSTXT(ready, 43023) != ready {
		t.Fatalf("application advertisement is not idempotent: %s", ready)
	}
	if got := updateApplicationTLSTXT(ready, 0); strings.Contains(got, "app-tls=") {
		t.Fatal("application withdrawal retained capability")
	}
	if got := updateWendyOSServicePort(ready, 50051, false, ""); strings.Contains(got, "app-tls=") {
		t.Fatal("unenrollment retained app TLS endpoint")
	}
}
