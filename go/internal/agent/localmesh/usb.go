//go:build linux || darwin

package localmesh

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/wendylabsinc/wendy/go/internal/shared/discovery"
)

// The gadget address is assigned by usbgadget.EnsureWellKnownAddress. Admit
// only that address on a device-side usbN interface, never arbitrary virtual
// interfaces. USB participation follows the existing Ethernet carrier setting.
func usbLANInterface(iface net.Interface, address *net.IPNet) (lanInterface, bool) {
	if !strings.HasPrefix(iface.Name, "usb") || address == nil || !address.IP.Equal(net.ParseIP(discovery.WellKnownUSBAddr)) {
		return lanInterface{}, false
	}
	return lanInterface{iface: iface, ip: append(net.IP(nil), address.IP...), net: address, cost: EthernetLinkCost, transport: "usb"}, true
}

func runUSBInterface(ctx context.Context, cfg LANConfig, selected lanInterface, claims *lanClaims) error {
	address := net.JoinHostPort(selected.ip.String()+"%"+selected.iface.Name, strconv.Itoa(TLSLANPort))
	listener, err := (&net.ListenConfig{Control: bindToInterface(selected.iface.Name)}).Listen(ctx, "tcp6", address)
	if err != nil {
		return fmt.Errorf("USB mesh TLS listen: %w", err)
	}
	if cfg.Status != nil {
		cfg.Status(CarrierStatus{Ready: true})
	}
	// Reuse the native mTLS admission, bounded handshakes, operator authorization,
	// connection policy and cancellation/draining of the other physical carriers.
	return runNetTLSListener(WithCarrier(ctx, "usb"), cfg.Node, listener, selected.cost, claims.policy, strconv.Itoa(int(selected.cost)))
}
