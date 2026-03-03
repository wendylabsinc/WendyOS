package discovery

// MDNSService represents a generic mDNS service entry discovered on the network.
type MDNSService struct {
	InstanceName string
	Hostname     string
	IPAddress    string
	Port         int
	TXTRecords   map[string]string
}
