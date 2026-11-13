//go:build linux

package localmesh

import (
	"context"
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

func TestLANHealthDoesNotReportReadyBeforeQUICSetup(t *testing.T) {
	_, credentials := testCredentials(t)
	for _, scenario := range []string{"missing-credentials", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer socket.Close()
			mdnsConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer mdnsConn.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ready := false
			cfg := LANConfig{Credentials: credentials, Status: func(status CarrierStatus) { ready = ready || status.Ready }}
			if scenario == "missing-credentials" {
				cfg.Credentials = nil
			} else {
				cancel()
			}
			err = serveLANInterface(ctx, cfg, lanInterface{}, &lanClaims{}, socket, mdnsConn, nil)
			if scenario == "missing-credentials" && err == nil {
				t.Fatal("TLS setup failure hidden")
			}
			if scenario == "canceled" && err != nil {
				t.Fatalf("cancellation reported failure: %v", err)
			}
			if ready {
				t.Fatal("failed or canceled QUIC setup reported ready")
			}
		})
	}
}
