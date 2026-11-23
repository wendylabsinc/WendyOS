package discovery

import "testing"

func TestLANDeviceReadsAccessShape(t *testing.T) {
	for txt, want := range map[string]string{"closed": "closed", "open": "open", "": ""} {
		records := map[string]string{}
		if txt != "" {
			records["access"] = txt
		}
		if got := lanDeviceFromService(MDNSService{TXTRecords: records}).Access; got != want {
			t.Errorf("access=%q: Access = %q, want %q", txt, got, want)
		}
	}
}
