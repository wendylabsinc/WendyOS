package liteclient

import "github.com/wendylabsinc/wendy/go/internal/shared/literelay"

// Network transport and verification are shared with the browser client.
type RelayOptions = literelay.RelayOptions
type relayConn = literelay.Conn

var openRelayConn = literelay.Open
var validLiteGrantRecord = literelay.ValidGrantRecord
