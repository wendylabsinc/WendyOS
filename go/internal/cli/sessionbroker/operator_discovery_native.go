package sessionbroker

import "github.com/wendylabsinc/wendy/go/internal/shared/discovery"

// Native discovery complements the same retained router and injected sources.
func init() { browseOperatorLAN = discovery.BrowseMDNSServicesContinuous }
