package main

/*
#include "../Sources/Shared/Bridge.h"
*/
import "C"

import (
	"github.com/wendylabsinc/wendy/go/internal/cli/commands"
	"unsafe"
)

//export WDNetworkTraffic
func WDNetworkTraffic(session C.uint64_t) *C.WDNetworkTrafficResult {
	return networkTrafficResult(desktop.NetworkTraffic(uint64(session)))
}

func networkTrafficResult(value *commands.NetworkTraffic, err error) *C.WDNetworkTrafficResult {
	p := alloc[C.WDNetworkTrafficResult](1)
	p.status = status(err)
	if err != nil || value == nil {
		return p
	}
	p.interfaces_error, p.agent_error = C.CString(value.InterfacesUnavailableReason), C.CString(value.AgentUnavailableReason)
	rows := append([]commands.NetworkTrafficRow{}, value.Interfaces...)
	if value.Agent != nil {
		rows = append(rows, *value.Agent)
	}
	p.rows, p.count = alloc[C.WDNetworkTrafficRow](len(rows)), C.size_t(len(rows))
	for i, row := range rows {
		unsafe.Slice(p.rows, len(rows))[i] = C.WDNetworkTrafficRow{name: C.CString(row.Name), index: C.uint32_t(row.Index),
			received_bytes: C.uint64_t(row.ReceivedBytes), sent_bytes: C.uint64_t(row.SentBytes),
			virtual_interface: boolInt(row.Virtual), loopback: boolInt(row.Loopback), agent: boolInt(value.Agent != nil && i == len(rows)-1),
			receive_rate: optional(row.ReceiveBytesPerSecond), send_rate: optional(row.SendBytesPerSecond)}
	}
	return p
}

//export WDNetworkTrafficFree
func WDNetworkTrafficFree(p *C.WDNetworkTrafficResult) {
	if p == nil {
		return
	}
	for _, row := range unsafe.Slice(p.rows, int(p.count)) {
		free(row.name)
	}
	free(p.rows)
	free(p.interfaces_error)
	free(p.agent_error)
	free(p.status.message)
	free(p)
}
