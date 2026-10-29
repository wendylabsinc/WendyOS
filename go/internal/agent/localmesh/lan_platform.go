package localmesh

import (
	"github.com/hashicorp/mdns"
	"strings"
)

// Platform chooses the transport family independently of routing role.
// Legacy role-only advertisements keep their previous planning behavior.
func shouldDialTLSLAN(entry *mdns.ServiceEntry, role string) bool {
	if entry == nil {
		return false
	}
	for _, field := range entry.InfoFields {
		key, value, ok := strings.Cut(field, "=")
		if ok && strings.EqualFold(key, "platform") {
			if strings.EqualFold(value, "WendyOS") {
				return false
			}
			if strings.EqualFold(value, "Lite") {
				return true
			}
		}
	}
	return role != "router"
}
