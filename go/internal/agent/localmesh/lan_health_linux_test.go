//go:build linux

package localmesh

import (
	"errors"
	"net"
	"strings"
	"testing"
)

func TestLANHealthPartialBindFailureRetryAndInterfaceRemoval(t *testing.T) {
	eth := lanInterface{iface: net.Interface{Index: 1, Name: "eth0"}, ip: net.IPv4(192, 0, 2, 1), cost: EthernetLinkCost}
	wifi := lanInterface{iface: net.Interface{Index: 2, Name: "wlan0"}, ip: net.IPv4(192, 0, 2, 2), cost: WiFiLinkCost}
	var status CarrierStatus
	h := lanCarrierHealth{report: func(s CarrierStatus) { status = s }}
	h.wanted([]lanInterface{eth, wifi})
	ethernet := h.begin(eth.key())
	wireless := h.begin(wifi.key())
	if status.Ready || status.Err != nil {
		t.Fatalf("unbound interfaces ready: %+v", status)
	}
	ethernet(CarrierStatus{Ready: true})
	wireless(CarrierStatus{Err: errors.New("mDNS bind failed")})
	if !status.Ready || status.Err == nil || !strings.Contains(status.Err.Error(), "wlan0: mDNS bind failed") {
		t.Fatalf("partial failure masked: %+v", status)
	}
	retry := h.begin(wifi.key())
	if status.Err == nil {
		t.Fatal("retry cleared bind failure")
	}
	wireless(CarrierStatus{Ready: true})
	if status.Err == nil {
		t.Fatal("old attempt cleared replacement failure")
	}
	retry(CarrierStatus{Ready: true})
	if !status.Ready || status.Err != nil || status.Detail != "2/2 eligible interfaces ready" {
		t.Fatalf("recovery: %+v", status)
	}
	h.wanted([]lanInterface{eth})
	retry(CarrierStatus{Err: errors.New("late removed interface failure")})
	if !status.Ready || status.Err != nil {
		t.Fatalf("removed worker poisoned status: %+v", status)
	}
	h.wanted(nil)
	ethernet(CarrierStatus{Ready: true})
	if status.Ready || status.Err != nil || !strings.Contains(status.Detail, "waiting") {
		t.Fatalf("unplugged interfaces ready: %+v", status)
	}
}
