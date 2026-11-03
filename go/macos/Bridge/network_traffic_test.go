package main

import (
	"errors"
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"math"
	"testing"
	"unsafe"
)

func TestNetworkTrafficBridgePreservesAvailabilityAndOwnership(t *testing.T) {
	rate := 12.5
	input := &commands.NetworkTraffic{Interfaces: []commands.NetworkTrafficRow{{Name: "eth0", Index: 2, ReceivedBytes: math.MaxUint64 - 1, ReceiveBytesPerSecond: &rate}},
		Agent: &commands.NetworkTrafficRow{Name: "Wendy agent service", SentBytes: 23}}
	result := networkTrafficResult(input, nil)
	defer WDNetworkTrafficFree(result)
	input.Interfaces[0].Name = "changed"
	rows := unsafe.Slice(result.rows, int(result.count))
	if len(rows) != 2 || cstring(unsafe.Pointer(rows[0].name)) != "eth0" || rows[0].received_bytes != math.MaxUint64-1 || rows[0].receive_rate.available != 1 || rows[0].receive_rate.value != 12.5 || rows[0].send_rate.available != 0 || rows[1].agent != 1 {
		t.Fatal("network data lost at bridge")
	}
	WDNetworkTrafficFree(networkTrafficResult(nil, errors.New("offline")))
	WDNetworkTrafficFree(nil)
}
