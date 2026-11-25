package localmesh

// Discovery clients use the wire ports and route costs on every platform,
// including platforms that do not run an agent LAN listener.
const (
	LANPort          = 43023
	TLSLANPort       = 43024
	EthernetLinkCost = uint16(64)
	WiFiLinkCost     = uint16(128)
)
